package e2e

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2ECore 基础互联：入网/远程浏览/取图逐字节/预取进度/取消/开关/离开。
// 拓扑：B、C 加入 A；D 加入 B。
func TestE2ECore(t *testing.T) {
	a := startNode(t, "A", nodeOpt{name: "甲"})
	b := startNode(t, "B", nodeOpt{name: "乙"})
	c := startNode(t, "C", nodeOpt{name: "丙"})
	d := startNode(t, "D", nodeOpt{name: "丁"})

	img1 := filePut(t, a.Gallery, "photo1.jpg", "jpeg-bytes-甲-111")
	sub := filePut(t, a.Gallery, "子目录/.keep", "")
	_ = sub
	img2 := filePut(t, a.Gallery, "子目录/photo2.png", "png-bytes-222")
	filePut(t, b.Gallery, "b.jpg", "b-content")

	// ---- 入网 ----
	r := b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	if results, _ := r["results"].([]any); len(results) == 0 {
		t.Fatalf("join 无结果: %v", r)
	} else if res := results[0].(map[string]any); res["ok"] != true {
		t.Fatalf("B 加入 A 失败: %v", res)
	}
	c.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	d.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))

	waitFor(t, 30*time.Second, "成员表收敛（B 可见 A、D）", func() bool {
		return contains(b.memberIDs(), a.selfID()) && contains(b.memberIDs(), d.selfID())
	})
	waitFor(t, 30*time.Second, "全网可见（D 可见 C）", func() bool {
		return contains(d.memberIDs(), c.selfID())
	})

	// state 字段（me.id 为 PeerID 格式）
	st := b.state()
	if me, _ := st["me"].(map[string]any); !strings.HasPrefix(fmt.Sprint(me["id"]), "12D3KooW") {
		t.Fatalf("PeerID 格式不符: %v", me["id"])
	}
	if st["joined"] != true {
		t.Fatal("joined 应为 true")
	}
	if fmt.Sprint(meName(st)) != "乙" {
		t.Fatalf("节点名不符: %v", meName(st))
	}

	// ---- 远程浏览（B → A）----
	code, bj := b.getJSON("/api/net/browse?node="+a.selfID(), 30*time.Second)
	if code != 200 {
		t.Fatalf("远程 roots 浏览失败: %d %s", code, bj)
	}
	if bj["name"] != "甲" {
		t.Fatalf("远程根名应为节点名甲: %v", bj["name"])
	}
	// 根视图列出 A 配置的根目录（gallery），内容在下一层
	folders, _ := bj["folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("远程根视图应有 1 个根目录: %v", bj)
	}
	if fm := folders[0].(map[string]any); fm["name"] != "gallery" {
		t.Fatalf("根目录名不符: %v", fm)
	}

	subPath := url.QueryEscape(filepath.Join(a.Gallery, "子目录"))
	code, sj := b.getJSON("/api/net/browse?node="+a.selfID()+"&path="+subPath, 30*time.Second)
	if code != 200 {
		t.Fatalf("远程子目录浏览失败: %d %s", code, sj)
	}
	images, _ := sj["images"].([]any)
	if len(images) != 1 || images[0].(map[string]any)["name"] != "photo2.png" {
		t.Fatalf("子目录 images 不符: %v", sj["images"])
	}

	// ---- 远程 times ----
	code, tj := b.getJSON("/api/net/times?node="+a.selfID()+"&path="+subPath, 30*time.Second)
	if code != 200 {
		t.Fatalf("远程 times 失败: %d %s", code, tj)
	}
	times, _ := tj["times"].([]any)
	if len(times) != 1 {
		t.Fatalf("times 不符: %v", tj)
	}

	// ---- 远程取图逐字节 ----
	imgPath := url.QueryEscape(img1)
	code, body := b.getBytes("/api/net/image?node="+a.selfID()+"&p="+imgPath, 60*time.Second)
	if code != 200 || string(body) != "jpeg-bytes-甲-111" {
		t.Fatalf("远程取图失败: %d %q", code, string(body))
	}
	// 二次取图命中本地缓存（不再回源——A 暂时关闭 share 仍能出图）
	if code2, _ := a.post("/api/net/settings", map[string]any{"share": false}, 10*time.Second); code2 != 200 {
		t.Fatal("settings 失败")
	}
	code, body2 := b.getBytes("/api/net/image?node="+a.selfID()+"&p="+imgPath, 60*time.Second)
	if code != 200 || string(body2) != "jpeg-bytes-甲-111" {
		t.Fatalf("缓存命中应可出图: %d %q", code, string(body2))
	}

	// share=false 期间：有缓存的目录回退缓存（offline:true）；没缓存的报错
	filePut(t, a.Gallery, "新目录/.keep", "")
	_, cb := b.getJSON("/api/net/browse?node="+a.selfID()+"&path="+subPath, 30*time.Second)
	if cb["offline"] != true {
		t.Fatalf("有缓存的目录应离线回退: %v", cb)
	}
	newDir := url.QueryEscape(filepath.Join(a.Gallery, "新目录"))
	code3, _ := b.getJSON("/api/net/browse?node="+a.selfID()+"&path="+newDir, 30*time.Second)
	if code3 == 200 {
		t.Fatal("无缓存的目录在 share=false 后应报错")
	}
	// 恢复
	a.post("/api/net/settings", map[string]any{"share": true}, 10*time.Second)

	// ---- 预取进度 + 取消 ----
	files := []string{img2}
	_, pf := b.post("/api/net/prefetch", map[string]any{
		"node": a.selfID(), "folder": filepath.Join(a.Gallery, "子目录"), "paths": files,
	}, 30*time.Second)
	if pf["ok"] != true || fmt.Sprint(pf["total"]) != "1" {
		t.Fatalf("prefetch 响应不符: %v", pf)
	}
	waitFor(t, 60*time.Second, "预取完成", func() bool {
		_, p := b.getJSON("/api/net/progress?node="+a.selfID()+"&folder="+url.QueryEscape(filepath.Join(a.Gallery, "子目录")), 10*time.Second)
		done, _ := p["done"].(float64)
		total, _ := p["total"].(float64)
		return total >= 1 && done >= 1
	})
	// 取消：进度清零
	b.post("/api/net/cancel", map[string]any{"node": a.selfID(), "folder": filepath.Join(a.Gallery, "子目录")}, 10*time.Second)
	_, p := b.getJSON("/api/net/progress?node="+a.selfID()+"&folder="+url.QueryEscape(filepath.Join(a.Gallery, "子目录")), 10*time.Second)
	if total, _ := p["total"].(float64); total != 0 {
		t.Fatalf("取消后进度未清零: %v", p)
	}

	// ---- 离开 ----
	_, lv := d.post("/api/net/leave", map[string]any{}, 15*time.Second)
	if lv != nil && lv["ok"] != true {
		t.Fatalf("leave 失败: %v", lv)
	}
	waitFor(t, 20*time.Second, "B 不再见到 D", func() bool {
		return !contains(b.memberIDs(), d.selfID())
	})
}

func meName(st map[string]any) any {
	me, _ := st["me"].(map[string]any)
	return me["name"]
}

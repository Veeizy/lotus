package e2e

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// TestE2ECache 链路缓存与离线浏览（对应旧 e2e-cache 场景）：
// 拓扑 A←B←C（C 只加入 B）。
//  1. B 缓存 A 的文件；A 下线后 C 仍可经 B 取到（stale 过期缓存，receipt 仍是 A 的）；
//  2. A 的目录结构在 C 侧离线可浏览（offline:true）；
//  3. A 重启（同目录）身份不变，缓存仍命中。
func TestE2ECache(t *testing.T) {
	a := startNode(t, "A", nodeOpt{name: "源节点"})
	b := startNode(t, "B", nodeOpt{name: "中间"})
	c := startNode(t, "C", nodeOpt{name: "末端"})

	img := filePut(t, a.Gallery, "共享.png", "shared-content-缓存验证")
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	c.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))

	waitFor(t, 30*time.Second, "C 知道 A（多跳可见）", func() bool {
		return contains(c.memberIDs(), a.selfID())
	})

	// B 预取（成为带凭证缓存的转发者）
	galleryDir := a.Gallery
	_, pf := b.post("/api/net/prefetch", map[string]any{
		"node": a.selfID(), "folder": galleryDir, "paths": []string{img},
	}, 30*time.Second)
	if pf["ok"] != true {
		t.Fatalf("B 预取失败: %v", pf)
	}
	waitFor(t, 60*time.Second, "B 预取完成", func() bool {
		_, p := b.getJSON("/api/net/progress?node="+a.selfID()+"&folder="+url.QueryEscape(galleryDir), 10*time.Second)
		done, _ := p["done"].(float64)
		return done >= 1
	})

	// C 先浏览一次 A（把目录列表缓存下来）
	code, bj := c.getJSON("/api/net/browse?node="+a.selfID(), 30*time.Second)
	if code != 200 {
		t.Fatalf("C 浏览 A 失败: %d %s", code, bj)
	}

	// ---- A 下线（分区）----
	idBefore := a.selfID()
	aID := a.selfID() // A 停止后无法再查询，先缓存
	a.stop()
	waitFor(t, 25*time.Second, "A 从 B/C 成员表消失", func() bool {
		return !b.memberSeenRecently(a.selfID()) && !c.memberSeenRecently(a.selfID())
	})

	// C 取 A 的图：C 无缓存 → 经 B 转发 → B 缓存代答（stale 过期缓存）
	imgQ := url.QueryEscape(img)
	code, body := c.getBytes("/api/net/image?node="+aID+"&p="+imgQ, 60*time.Second)
	if code != 200 || string(body) != "shared-content-缓存验证" {
		t.Logf("B 日志: %s", b.LogBuf.String())
		t.Logf("C 日志: %s", c.LogBuf.String())
		t.Fatalf("A 离线后应经 B 的 stale 缓存出图: %d %q", code, string(body))
	}
	// C 侧缓存索引里的凭证必须仍是 A 签发（索引 2s 防抖落盘，轮询等待）
	var entry map[string]any
	waitFor(t, 15*time.Second, "C 缓存索引落盘", func() bool {
		idx := readIndex(t, c.Dir, aID)
		if idx == nil {
			return false
		}
		entry = findEntry(t, idx, "共享.png")
		return entry != nil && entry["receipt"] != nil
	})
	if entry == nil {
		t.Fatalf("C 索引缺失或无凭证: %v", readIndex(t, c.Dir, aID))
	}

	// 离线浏览目录（offline:true）
	code, oj := c.getJSON("/api/net/browse?node="+aID, 30*time.Second)
	if code != 200 || oj["offline"] != true {
		t.Fatalf("离线浏览应回退缓存并标记 offline: %d %v", code, oj)
	}

	// ---- A 重启（同目录）：身份不变，链路恢复 ----
	a.spawn()
	if a.selfID() != idBefore {
		t.Fatalf("重启后身份变化: %s -> %s", idBefore, a.selfID())
	}
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort)) // 重新入网
	waitFor(t, 30*time.Second, "A 回到 B 的成员表", func() bool {
		return contains(b.memberIDs(), a.selfID())
	})
	code, body2 := b.getBytes("/api/net/image?node="+a.selfID()+"&p="+imgQ, 60*time.Second)
	if code != 200 || string(body2) != "shared-content-缓存验证" {
		t.Fatalf("重启后取图失败: %d %q", code, string(body2))
	}
}

// memberSeenRecently 判断节点是否仍在线（成员表内即在线——离线条目会被清除）。
func (n *e2eNode) memberSeenRecently(id string) bool {
	return contains(n.memberIDs(), id)
}

func readIndex(t *testing.T, dir, peerID string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "net-cache", peerID, "index.json"))
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := jsonUnmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

func findEntry(t *testing.T, idx map[string]any, name string) map[string]any {
	t.Helper()
	files, _ := idx["files"].(map[string]any)
	for _, v := range files {
		if e, ok := v.(map[string]any); ok {
			if rp, _ := e["rpath"].(string); strings.HasSuffix(rp, name) {
				return e
			}
		}
	}
	return nil
}

package e2e

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestE2ENetEnv 网络环境矩阵（用户重点场景）：
//  1. 多层级：A←B←C←D 四级链，D 取 A 的图经 3 跳转发；全网成员表收敛；
//  2. 高延迟：A 加 --delay-ms 400，B 取 512KB 图在预算内完成且逐字节一致；
//  3. 高延迟 stale：A 加 --delay-ms 9000（>8s stat 超时），B 持缓存，
//     C 经 B 取图 → stat 核对超时 → 走过期缓存应答；
//  4. 复杂嫁接：C 嫁接到 A（双 hub），随后 B 永久下线，C 经 A 继续工作；
//  5. 分区与愈合：B 重启后自动回到网络，成员表收敛。
func TestE2ENetEnv(t *testing.T) {
	// ---- 场景 1：多层级 ----
	a := startNode(t, "A", nodeOpt{name: "根"})
	b := startNode(t, "B", nodeOpt{name: "中间B"})
	c := startNode(t, "C", nodeOpt{name: "中间C"})
	d := startNode(t, "D", nodeOpt{name: "叶子"})

	img := filePut(t, a.Gallery, "deep.jpg", "root-content-多层级-验证")
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	c.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))
	d.join(fmt.Sprintf("127.0.0.1:%d", c.HTTPPort))

	waitFor(t, 40*time.Second, "D 可见根 A（gossip 跨 3 跳）", func() bool {
		return contains(d.memberIDs(), a.selfID())
	})
	waitFor(t, 30*time.Second, "A 可见叶子 D（反向可见）", func() bool {
		return contains(a.memberIDs(), d.selfID())
	})

	aID := a.selfID()
	imgQ := url.QueryEscape(img)
	code, body := d.getBytes("/api/net/image?node="+aID+"&p="+imgQ, 120*time.Second)
	if code != 200 || string(body) != "root-content-多层级-验证" {
		t.Fatalf("多层级转发取图失败: %d %q", code, string(body))
	}
	t.Log("✓ 多层级：D 经 C→B 两级转发取到 A 的图")

	// ---- 场景 4 前置：C 嫁接到 A（双 hub）----
	r := c.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	if results, _ := r["results"].([]any); len(results) > 0 {
		res := results[0].(map[string]any)
		if res["ok"] != true {
			t.Fatalf("C 嫁接到 A 失败: %v", res)
		}
	}
	waitFor(t, 20*time.Second, "C 直连 A", func() bool { return contains(c.memberIDs(), a.selfID()) })
	t.Log("✓ 嫁接：C 已同时挂 A 与 B")

	// ---- 场景 2：高延迟（A 以 400ms/32KB 重启）----
	a.restart("--delay-ms", "400")
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	c.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	waitFor(t, 30*time.Second, "A 重启后 B 重新可见", func() bool {
		return contains(b.memberIDs(), aID)
	})
	big := make([]byte, 512*1024)
	for i := range big {
		big[i] = byte(i * 7)
	}
	bigPath := filepath.Join(a.Gallery, "big-slow.jpg")
	if err := os.WriteFile(bigPath, big, 0o644); err != nil {
		t.Fatal(err)
	}
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort)) // 确保 B 直连 A
	waitFor(t, 20*time.Second, "B 可见 A", func() bool { return contains(b.memberIDs(), aID) })

	start := time.Now()
	code, body = b.getBytes("/api/net/image?node="+aID+"&p="+url.QueryEscape(bigPath), 120*time.Second)
	elapsed := time.Since(start)
	if code != 200 || len(body) != len(big) {
		t.Logf("A日志尾部: %s", tailStr(a.LogBuf.String(), 900))
		t.Logf("B日志尾部: %s", tailStr(b.LogBuf.String(), 900))
		t.Logf("C日志尾部: %s", tailStr(c.LogBuf.String(), 900))
		t.Fatalf("高延迟取图失败: %d len=%d body=%q", code, len(body), string(body[:minInt(len(body), 200)]))
	}
	for i := range big {
		if body[i] != big[i] {
			t.Fatalf("高延迟取图内容不符 @%d", i)
		}
	}
	if elapsed < 2*time.Second {
		t.Fatalf("延迟注入未生效（耗时 %v）", elapsed)
	}
	t.Logf("✓ 高延迟：512KB 在 %v 内完整到达（预算 120s）", elapsed)

	// ---- 场景 5：分区与愈合（B 重启）----
	bID := b.selfID()
	b.restart()
	if b.selfID() != bID {
		t.Fatalf("B 重启后身份变化")
	}
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	b.join(fmt.Sprintf("127.0.0.1:%d", c.HTTPPort))
	waitFor(t, 30*time.Second, "B 重启后回到 A 的成员表", func() bool {
		return contains(a.memberIDs(), bID)
	})
	t.Log("✓ 分区愈合：B 重启自动回网")

	// ---- B 永久下线，C 靠嫁接的 A 继续 ----
	bID2 := b.selfID()
	cID := c.selfID()
	b.stop()
	waitFor(t, 25*time.Second, "B 从 C 成员表消失", func() bool {
		return !contains(c.memberIDs(), bID2)
	})
	code, body = c.getBytes("/api/net/image?node="+aID+"&p="+url.QueryEscape(img), 60*time.Second)
	if code != 200 || string(body) != "root-content-多层级-验证" {
		t.Fatalf("B 下线后 C 应经嫁接的 A 取图: %d %q", code, string(body))
	}
	t.Log("✓ 嫁接容灾：B 下线后 C 经 A 继续取图")
	_ = cID
	_ = d
}

// TestE2EHighLatencyStale 高延迟下的过期缓存：A 的 stat 应答慢于 8s 核对预算，
// B 持有缓存时应答 stale 内容（不阻塞在 A 上）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func TestE2EHighLatencyStale(t *testing.T) {
	// A 正常启动（先建立缓存），随后以高延迟重启
	a := startNode(t, "A", nodeOpt{name: "慢源"})
	b := startNode(t, "B", nodeOpt{name: "缓存"})
	c := startNode(t, "C", nodeOpt{name: "请求方"})

	img := filePut(t, a.Gallery, "slow.png", "slow-content-v1")
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	c.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))
	waitFor(t, 30*time.Second, "C 可见 A", func() bool { return contains(c.memberIDs(), a.selfID()) })

	// B 预取
	_, pf := b.post("/api/net/prefetch", map[string]any{
		"node": a.selfID(), "folder": a.Gallery, "paths": []string{img},
	}, 30*time.Second)
	if pf["ok"] != true {
		t.Fatal("B 预取失败")
	}
	waitFor(t, 60*time.Second, "B 预取完成", func() bool {
		_, p := b.getJSON("/api/net/progress?node="+a.selfID()+"&folder="+url.QueryEscape(a.Gallery), 10*time.Second)
		done, _ := p["done"].(float64)
		return done >= 1
	})

	// A 以 9s/32KB 延迟重启（stat 应答必然超过 8s 核对预算）
	aID := a.selfID()
	a.restart("--delay-ms", "9000")
	waitFor(t, 30*time.Second, "A 重启就绪", func() bool {
		code, _ := a.get("/api/roots", 5*time.Second)
		return code == 200
	})
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))

	// C 取图：经 B，B 向 A 核对 stat 超时 → stale 应答
	start := time.Now()
	code, body := c.getBytes("/api/net/image?node="+aID+"&p="+url.QueryEscape(img), 120*time.Second)
	elapsed := time.Since(start)
	if code != 200 || string(body) != "slow-content-v1" {
		t.Fatalf("高延迟 stale 取图失败: %d %q", code, string(body))
	}
	if elapsed > 60*time.Second {
		t.Fatalf("stale 应答不应阻塞在慢源上（耗时 %v）", elapsed)
	}
	t.Logf("✓ 高延迟 stale：%.1fs 内由 B 的过期缓存应答", elapsed.Seconds())
}

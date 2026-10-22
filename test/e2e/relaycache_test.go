package e2e

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestE2ERelayCacheHit 中继节点缓存优化（用户验收项 1）：
// A←B←C←D 四级链，中间节点 B 主动访问过 A 的图片（留下带凭证缓存）；
// D 经链条询问 A 的同一资源时，由 **B 直接代答**（而非转发到 A）。
// 证据：B 日志出现「链路缓存代答」；A 日志全程没有「提供加密图片」该文件。
func TestE2ERelayCacheHit(t *testing.T) {
	a := startNode(t, "A", nodeOpt{name: "源"})
	b := startNode(t, "B", nodeOpt{name: "中间B"})
	c := startNode(t, "C", nodeOpt{name: "中间C"})
	d := startNode(t, "D", nodeOpt{name: "末端D"})

	// B 要访问的文件：专用文件名，确保 A 此前从未向任何人提供过它
	img := filePut(t, a.Gallery, "b-cache-hit.png", "relay-cache-hit-content-中转缓存")
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	c.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))
	d.join(fmt.Sprintf("127.0.0.1:%d", c.HTTPPort))

	aID := a.selfID()
	waitFor(t, 40*time.Second, "D 可见 A（跨 3 跳）", func() bool {
		return contains(d.memberIDs(), aID)
	})

	// ---- B 主动访问 A 的图片（留下缓存）----
	_, pf := b.post("/api/net/prefetch", map[string]any{
		"node": aID, "folder": a.Gallery, "paths": []string{img},
	}, 30*time.Second)
	if pf["ok"] != true {
		t.Fatalf("B 预取失败: %v", pf)
	}
	waitFor(t, 60*time.Second, "B 缓存完成", func() bool {
		_, p := b.getJSON("/api/net/progress?node="+aID+"&folder="+url.QueryEscape(a.Gallery), 10*time.Second)
		done, _ := p["done"].(float64)
		return done >= 1
	})

	// ---- D 经链条询问同一资源 ----
	// （预取阶段 A 合法服务过 B 一次；记录基线，D 的请求不应使 A 再出图）
	aServesBefore := strings.Count(a.LogBuf.String(), "提供加密图片")
	imgQ := url.QueryEscape(img)
	code, body := d.getBytes("/api/net/image?node="+aID+"&p="+imgQ, 120*time.Second)
	if code != 200 || string(body) != "relay-cache-hit-content-中转缓存" {
		t.Fatalf("D 取图失败: %d %q", code, string(body))
	}

	// ---- 证据：B 代答、A 未被回源 ----
	bLog := b.LogBuf.String()
	aLog := a.LogBuf.String()
	if !strings.Contains(bLog, "链路缓存代答") {
		t.Fatalf("B 日志缺少代答记录（应由 B 直接回复）:\n%s", tailStr(bLog, 1500))
	}
	aServesAfter := strings.Count(aLog, "提供加密图片")
	if aServesAfter != aServesBefore {
		t.Fatalf("D 的请求不应回源 A（A 出图次数 %d -> %d）:\n%s", aServesBefore, aServesAfter, tailStr(aLog, 1200))
	}
	t.Log("✓ 中继缓存：D 的请求由 B 直接代答（B 日志有代答记录，A 未出图）")

	// ---- 对照：请求一个 B 没缓存的文件 → 必须回源 A ----
	img2 := filePut(t, a.Gallery, "no-cache.png", "must-come-from-origin")
	code, body = d.getBytes("/api/net/image?node="+aID+"&p="+url.QueryEscape(img2), 120*time.Second)
	if code != 200 || string(body) != "must-come-from-origin" {
		t.Logf("B日志: %s", tailStr(b.LogBuf.String(), 2000))
		t.Logf("C日志: %s", tailStr(c.LogBuf.String(), 1500))
		t.Fatalf("无缓存文件应回源: %d %q", code, string(body))
	}
	if !strings.Contains(a.LogBuf.String(), "提供加密图片") {
		t.Fatal("无缓存文件应由 A 出图（回源）")
	}
	t.Log("✓ 对照组：B 无缓存的文件正确回源 A")
}

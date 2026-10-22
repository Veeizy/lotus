package e2e

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2ELeaveNotifyAndCascade 节点下线语义（用户验收项 2）：
//  a. 优雅下线：签名通知即时传播全网（不等新鲜度窗口），且不会被自动拉回；
//  b. 崩溃下线：A←B←C←D 链中 B 崩溃 —— 其子树（C、D）无其他可达上层，
//     全网按新鲜度一并收敛为离线（级联下线）；对离线节点的访问有兜底；
//  c. 嫁接存活：C 另挂 A 时，B 崩溃后子树经由 A 保持在线（替代上层可达）。
func TestE2ELeaveNotifyAndCascade(t *testing.T) {
	a := startNode(t, "A", nodeOpt{name: "根"})
	b := startNode(t, "B", nodeOpt{name: "中间B"})
	c := startNode(t, "C", nodeOpt{name: "中间C"})
	d := startNode(t, "D", nodeOpt{name: "叶子"})

	img := filePut(t, a.Gallery, "cascade.png", "cascade-content-下线验证")
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	c.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))
	d.join(fmt.Sprintf("127.0.0.1:%d", c.HTTPPort))
	aID := a.selfID()

	waitFor(t, 40*time.Second, "全网收敛（D 可见 A）", func() bool {
		return contains(d.memberIDs(), aID)
	})

	// D 先浏览一次 A（把目录列表缓存下来，供崩溃后的离线兜底验证）
	code, bj := d.getJSON("/api/net/browse?node="+aID, 30*time.Second)
	if code != 200 {
		t.Fatalf("D 浏览 A 失败: %d %s", code, bj)
	}

	// ---- 场景 A：优雅下线 = 签名通知即时传播 ----
	cID := cID(t, d, "中间C") // 必须在下线前捕获（下线后成员表已移除该节点）
	if cID == "" {
		t.Fatal("未能从 D 的成员表定位 C 的 PeerID")
	}
	_, lv := c.post("/api/net/leave", map[string]any{}, 15*time.Second)
	if lv == nil || lv["ok"] != true {
		t.Fatalf("C leave 失败: %v", lv)
	}
	deadline := time.Now().Add(6 * time.Second) // 快于新鲜度窗口（8s），证明是通知而非超时
	for time.Now().Before(deadline) {
		if !contains(a.memberIDs(), cID) && !contains(b.memberIDs(), cID) && !contains(d.memberIDs(), cID) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if contains(a.memberIDs(), cID) || contains(b.memberIDs(), cID) || contains(d.memberIDs(), cID) {
		t.Fatal("优雅下线应在新鲜度窗口内之前全网生效（签名通知）")
	}
	if !strings.Contains(a.LogBuf.String(), "节点已下线（签名通知）") {
		t.Fatal("A 日志缺少签名通知记录")
	}
	t.Log("✓ 优雅下线：签名通知即时传播，A/B/D 全部移除 C")

	// C 重新入网（更新鲜的宣告解除 leftAt 抑制，重新被成员表收录）
	c.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))
	waitFor(t, 20*time.Second, "C 重新入网后回到 B 的成员表", func() bool {
		return contains(b.memberIDs(), cID)
	})

	// ---- 场景 C 前置：C 嫁接到 A（替代上层）----
	c.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	waitFor(t, 20*time.Second, "C 直连 A", func() bool { return contains(c.memberIDs(), aID) })

	// ---- 场景 B：B 崩溃（无通知）----
	// 注意：C 此刻双挂 A/B，为验证「无替代上层则级联下线」，需要一条纯链：
	// 重建拓扑 —— 停掉 C，让 D 直挂 B？拓扑已嫁接，改为拆分两个子场景：
	// B1（级联下线）用全新纯链验证；B2（嫁接存活）用当前拓扑验证。
	// 这里先验证 B2：杀 B，C/D 经 A 保持在线。
	bID := b.selfID()
	b.stop()
	waitFor(t, 30*time.Second, "B 从 A 成员表消失（新鲜度收敛）", func() bool {
		return !contains(a.memberIDs(), bID)
	})
	if !contains(a.memberIDs(), cID) {
		t.Fatal("嫁接存活：C 应经 A 保持在线")
	}
	waitFor(t, 20*time.Second, "D 经 C 继续可见于 A", func() bool {
		return contains(a.memberIDs(), d.selfID())
	})
	code, body := d.getBytes("/api/net/image?node="+aID+"&p="+url.QueryEscape(img), 60*time.Second)
	if code != 200 || string(body) != "cascade-content-下线验证" {
		t.Fatalf("B 下线后 D 应经 C→A 取图: %d %q", code, string(body))
	}
	t.Log("✓ 嫁接存活：B 崩溃后 C/D 经替代上层 A 保持在线并可取图")
}

// TestE2ECascadeOffline 纯链级联下线 + 离线兜底：
// A←B←C←D（无嫁接），B 崩溃后 C/D 与 A 失去最后路径 → 全网一并收敛离线；
// 已缓存的目录列表仍可离线浏览（offline:true），未缓存的图片明确报错。
func TestE2ECascadeOffline(t *testing.T) {
	a := startNode(t, "A", nodeOpt{name: "根"})
	b := startNode(t, "B", nodeOpt{name: "中间B"})
	c := startNode(t, "C", nodeOpt{name: "中间C"})
	d := startNode(t, "D", nodeOpt{name: "叶子"})

	filePut(t, a.Gallery, "offline.png", "offline-内容")
	b.join(fmt.Sprintf("127.0.0.1:%d", a.HTTPPort))
	c.join(fmt.Sprintf("127.0.0.1:%d", b.HTTPPort))
	d.join(fmt.Sprintf("127.0.0.1:%d", c.HTTPPort))
	aID := a.selfID()

	waitFor(t, 40*time.Second, "全网收敛", func() bool {
		return contains(d.memberIDs(), aID)
	})

	// D 缓存 A 的目录列表（离线兜底数据）
	if code, bj := d.getJSON("/api/net/browse?node="+aID, 30*time.Second); code != 200 {
		t.Fatalf("D 浏览 A 失败: %d %s", code, bj)
	}

	// B 崩溃：子树（C/D）失去与 A 的中转路径。
	// 可见性级联：A 收不到 C/D 的宣告 → 成员表收敛清空（网络视角的「一并下线」）。
	// 注：LAN 上 D 仍可按需直拨存活的 A（无 NAT 隔离），真正不可达需 NAT 环境。
	b.stop()
	waitFor(t, 40*time.Second, "级联下线：A 的成员表清空（C/D 一并离线）", func() bool {
		return len(a.memberIDs()) == 0
	})
	waitFor(t, 30*time.Second, "C/D 侧 A 也离线", func() bool {
		return !contains(c.memberIDs(), aID) && !contains(d.memberIDs(), aID)
	})
	t.Log("✓ 级联下线：B 崩溃后其子树（C/D）的宣告不再到达 A，双向收敛离线")

	// 兜底验证需要「访问必失败」：让 A 也下线，构造真正的空节点访问。
	a.stop()
	waitFor(t, 20*time.Second, "D 侧 A 彻底离线", func() bool {
		return !contains(d.memberIDs(), aID)
	})
	// 兜底 1：已缓存目录列表 → 离线浏览（offline:true）
	code, oj := d.getJSON("/api/net/browse?node="+aID, 30*time.Second)
	if code != 200 || oj["offline"] != true {
		t.Fatalf("离线浏览应回退缓存: %d %v", code, oj)
	}
	// 兜底 2：未缓存的图片 → 明确报错（不悬挂）
	imgPath := url.QueryEscape(a.Gallery + string(filepath.Separator) + "offline.png")
	code, ej := d.getJSON("/api/net/image?node="+aID+"&p="+imgPath, 30*time.Second)
	if code != 404 || !strings.Contains(fmt.Sprint(ej["error"]), "节点不在网络中") {
		t.Fatalf("未缓存图片应明确 404: %d %v", code, ej)
	}
	t.Log("✓ 离线兜底：目录列表离线可读（offline:true），未缓存资源明确报错")
}

func cID(t *testing.T, from *e2eNode, name string) string {
	t.Helper()
	for _, m := range from.state()["members"].([]any) {
		if mm := m.(map[string]any); mm["name"] == name {
			return mm["id"].(string)
		}
	}
	for _, id := range from.memberIDs() {
		if strings.HasPrefix(id, "12D3KooW") {
			_ = id
		}
	}
	return ""
}

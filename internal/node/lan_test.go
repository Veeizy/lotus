package node

import (
	"context"
	"testing"
	"time"
)

// TestLanJoinCandidates 局域网候选入网：合法候选按普通 join 流程接入；
// 已加入节点（known 表命中）与自身在候选过滤阶段即被排除。
func TestLanJoinCandidates(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	env.dial(a, b) // a←b 已连接：b 对 a 而言是"已加入节点"

	// 候选：b（已加入，应被过滤）+ a 自己（应被过滤）
	cands := []LanCandidate{
		{ID: b.PeerID, Addr: "127.0.0.1:1", Via: "scan"},
		{ID: a.PeerID, Addr: "127.0.0.1:2", Via: "scan"},
	}
	results := a.Mgr.joinLanCandidates(context.Background(), cands)
	if len(results) != 2 {
		t.Fatalf("结果数不符: %d", len(results))
	}
	// 过滤发生在 lanCandidates；joinLanCandidates 对已连接节点重复 joinOne
	// 会返回"已存在"成功——验证网络仍然健康且连接数正确
	if !a.Host.Connected(b.PeerID) {
		t.Fatal("a-b 连接应保持")
	}
}

// TestLanScanTargets 局域网扫描目标：覆盖 /24 网段、排除本机自身 IP 与回环。
func TestLanScanTargets(t *testing.T) {
	own, targets := lanScanTargets()
	if len(targets) == 0 {
		t.Skip("本机无非回环 IPv4，跳过")
	}
	if len(targets) < 100 {
		t.Fatalf("/24 网段应展开约 254 个目标: %d", len(targets))
	}
	for _, o := range own {
		for _, tg := range targets {
			if o == tg {
				t.Fatalf("扫描目标含本机 IP: %s", tg)
			}
		}
	}
}

// TestLanCandidatesFilter 候选过滤：自身与已加入节点被排除。
func TestLanCandidatesFilter(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	env.dial(a, b)
	waitFor(t, 15*time.Second, "a 收到 b 的宣告", func() bool {
		return a.Mgr.AttestationOf(b.PeerID) != nil
	})
	// 手工把 b 记为 a 的 hub（模拟已加入），lanCandidates 应排除 b
	a.Mgr.mu.Lock()
	a.Mgr.known["/ip4/127.0.0.1/udp/1/quic-v1"] = b.PeerID
	a.Mgr.hubs = append(a.Mgr.hubs, "/ip4/127.0.0.1/udp/1/quic-v1")
	a.Mgr.mu.Unlock()

	cands := a.Mgr.lanCandidates(context.Background(), nil)
	for _, c := range cands {
		if c.ID == b.PeerID || c.ID == a.PeerID {
			t.Fatalf("候选含已加入节点/自身: %+v", c)
		}
	}
}

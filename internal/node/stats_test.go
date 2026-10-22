package node

import (
	"context"
	"testing"
	"time"

	"lotus/internal/stats"
)

// TestRelayStats 中转观测：A←S←B（S 为纯中转，无共享文件夹也不影响转发），
// B 经 S 取 A 的图后，S 的访问日志出现 relay 事件、按对端流量双向入账。
func TestRelayStats(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	relayStats := stats.New()
	s := env.startNode(2, func(n *nodeProc) { n.Stats = relayStats })
	b := env.startNode(3)
	img := writeFile(t, a.Gallery, "stats.png", "relay-stats-内容-0123456789")

	// 拓扑：A←S←B（与 S 的图库无关：零文件夹节点照常中转）
	env.dial(a, s)
	env.dial(s, b)
	waitFor(t, 10*time.Second, "S-B 连接", func() bool { return s.Host.Connected(b.PeerID) })
	waitFor(t, 15*time.Second, "B 收到 S 的宣告", func() bool {
		return b.Mgr.AttestationOf(s.PeerID) != nil
	})
	waitFor(t, 15*time.Second, "B 知道 A（经 gossip）", func() bool {
		return b.Mgr.AttestationOf(a.PeerID) != nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := b.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true); err != nil {
		t.Fatalf("经中转取图失败: %v", err)
	}

	// 访问日志：S 上有 relay 事件（对端=B，即请求方）
	entries := relayStats.AccessSnapshot(0)
	var relay *stats.AccessEntry
	for i := range entries {
		if entries[i].Kind == stats.KindRelay {
			relay = &entries[i]
			break
		}
	}
	if relay == nil {
		t.Fatalf("中转节点无 relay 访问记录: %+v", entries)
	}
	if relay.Peer != b.PeerID || !relay.OK || relay.Bytes <= 0 {
		t.Fatalf("relay 记录字段不符: %+v", relay)
	}

	// 流量：S 对 A（收+发）与对 B（发）都有量
	rep := relayStats.TrafficSnapshot()
	byPeer := map[string][2]int64{}
	for _, p := range rep.Peers {
		byPeer[p.Peer] = [2]int64{p.In, p.Out}
	}
	sa, okA := byPeer[a.PeerID]
	sb, okB := byPeer[b.PeerID]
	if !okA || sa[0] <= 0 || sa[1] <= 0 {
		t.Fatalf("对源节点 A 的流量缺失: %+v", rep.Peers)
	}
	if !okB || sb[1] <= 0 {
		t.Fatalf("对请求方 B 的流量缺失: %+v", rep.Peers)
	}
	if rep.TotalIn <= 0 || rep.TotalOut <= 0 {
		t.Fatalf("总量缺失: %+v", rep)
	}

	// status 快照聚合正确
	st := relayStats.StatusSnapshot()
	if st.Requests < 1 {
		t.Fatalf("status 请求数不符: %+v", st)
	}
}

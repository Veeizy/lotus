package stats

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCollectorTrafficAndAccess(t *testing.T) {
	c := New()
	if c.Uptime() < 0 || c.StartedAt().IsZero() {
		t.Fatal("启动时间未记录")
	}

	c.AddTraffic("peerA", true, 100)
	c.AddTraffic("peerA", false, 40)
	c.AddTraffic("peerB", false, 7)
	c.AddTraffic("peerB", false, 3) // 同对端累计
	c.AddTraffic("peerC", true, 0)  // 零字节忽略

	rep := c.TrafficSnapshot()
	if rep.TotalIn != 100 || rep.TotalOut != 50 {
		t.Fatalf("总量不符: %+v", rep)
	}
	if len(rep.Peers) != 2 {
		t.Fatalf("对端数不符: %d", len(rep.Peers))
	}
	// 降序：peerA(140) > peerB(10)
	if rep.Peers[0].Peer != "peerA" || rep.Peers[0].In != 100 || rep.Peers[0].Out != 40 {
		t.Fatalf("排序或计数不符: %+v", rep.Peers)
	}
	if rep.Peers[1].Peer != "peerB" || rep.Peers[1].Out != 10 {
		t.Fatalf("对端累计不符: %+v", rep.Peers[1])
	}

	c.LogAccess(AccessEvent{Kind: KindRelay, Peer: "peerB", Path: "/x/a.jpg", Bytes: 2048, OK: true})
	c.LogAccess(AccessEvent{Kind: KindRelayFail, Peer: "peerA", Path: "/x/b.jpg", OK: false, Extra: "超时"})
	c.LogAccess(AccessEvent{Kind: "browse", Peer: "peerA", Path: "/", OK: true})

	entries := c.AccessSnapshot(0)
	if len(entries) != 3 {
		t.Fatalf("记录数不符: %d", len(entries))
	}
	if entries[0].Kind != "browse" { // 新→旧
		t.Fatalf("排序不符: %+v", entries)
	}
	limited := c.AccessSnapshot(1)
	if len(limited) != 1 || limited[0].Kind != "browse" {
		t.Fatalf("limit 截取不符: %+v", limited)
	}

	st := c.StatusSnapshot()
	if st.Requests != 3 || st.UptimeSec < 0 {
		t.Fatalf("status 概览不符: %+v", st)
	}
	if len(st.Kinds) != 3 {
		t.Fatalf("类型计数不符: %+v", st.Kinds)
	}
	// relay: 1 次成功 2048 字节
	for _, k := range st.Kinds {
		switch k.Kind {
		case KindRelay:
			if k.Count != 1 || k.OK != 1 || k.Bytes != 2048 {
				t.Fatalf("relay 计数不符: %+v", k)
			}
		case KindRelayFail:
			if k.Fail != 1 {
				t.Fatalf("relay-fail 计数不符: %+v", k)
			}
		}
	}

	// JSON 快照可序列化（api 层直出）
	if _, err := json.Marshal(st); err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
}

func TestAccessRingBuffer(t *testing.T) {
	c := New()
	for i := 0; i < MaxAccessLog+50; i++ {
		c.LogAccess(AccessEvent{Kind: "stat", Peer: "p", OK: true})
	}
	entries := c.AccessSnapshot(0)
	if len(entries) != MaxAccessLog {
		t.Fatalf("环形容量不符: %d", len(entries))
	}
	// 最旧被淘汰：第一条应为最后一次写入附近（编号顺序无法直接断言，验证条数与排序稳定即可）
	for i := 1; i < len(entries); i++ {
		if entries[i-1].TS < entries[i].TS {
			t.Fatalf("时间戳乱序: %d < %d", entries[i-1].TS, entries[i].TS)
		}
	}
}

func TestNilCollectorSafe(t *testing.T) {
	var c *Collector
	c.AddTraffic("p", true, 1)   // 不应 panic
	c.LogAccess(AccessEvent{})   // 不应 panic
	_ = time.Now()
}

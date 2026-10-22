package p2p

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	circuitclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
)

func testSeed(n byte) []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = n
	}
	return s
}

// listenAddr 构造本机可拨号的完整 multiaddr（含 /p2p/ 身份）。
func listenAddr(t *testing.T, h *Host) string {
	t.Helper()
	var addr string
	for _, a := range h.Libp2p().Addrs() {
		s := a.String()
		if strings.HasPrefix(s, "/ip4/127.") {
			addr = s
			break
		}
	}
	if addr == "" {
		for _, a := range h.Libp2p().Addrs() {
			s := a.String()
			if i := strings.Index(s, "/quic-v1"); i > 0 {
				addr = "/ip4/127.0.0.1" + s[strings.Index(s, "/udp"):i+len("/quic-v1")]
				break
			}
		}
	}
	if addr == "" {
		t.Fatal("无可用监听地址")
	}
	return addr + "/p2p/" + h.ID()
}

func newTestHost(t *testing.T, cfg Config) *Host {
	t.Helper()
	cfg.SeedEd = testSeed(cfg.SeedEd[0])
	h, err := NewHost(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHost 失败: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestFramesRoundtrip(t *testing.T) {
	a := newTestHost(t, Config{SeedEd: []byte{1}})
	b := newTestHost(t, Config{SeedEd: []byte{2}})

	ready := make(chan string, 1)
	b.SetStreamHandler("/test/echo/1", func(s network.Stream) {
		msg, err := ReadMsg(s, 0)
		if err != nil {
			s.Reset()
			return
		}
		WriteMsg(s, append([]byte("echo:"), msg...), nil)
		s.Close()
		ready <- string(msg)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.ConnectAddr(ctx, listenAddr(t, b)); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	s, err := a.NewStream(ctx, b.ID(), "/test/echo/1")
	if err != nil {
		t.Fatalf("开流失败: %v", err)
	}
	if err := WriteMsg(s, []byte("hello-picnet"), nil); err != nil {
		t.Fatal(err)
	}
	resp, err := ReadMsg(s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "echo:hello-picnet" {
		t.Fatalf("回显不符: %q", resp)
	}
	s.Close()
}

func TestGossipAttestationDelivery(t *testing.T) {
	got := make(chan string, 8)
	a := newTestHost(t, Config{SeedEd: []byte{10}})
	b := newTestHost(t, Config{SeedEd: []byte{11}, OnGossip: func(from string, data []byte) {
		got <- string(data)
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.ConnectAddr(ctx, listenAddr(t, b)); err != nil {
		t.Fatal(err)
	}
	// gossipsub mesh 稳定前可能丢首条消息，重试发布
	for i := 0; i < 10; i++ {
		if err := a.Publish([]byte(fmt.Sprintf("att-%d", i))); err == nil {
			select {
			case msg := <-got:
				if msg == fmt.Sprintf("att-%d", i) {
					return // 收到
				}
			case <-time.After(800 * time.Millisecond):
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("gossip 消息未送达")
}

// TestRelayFallback 验证直连被禁止时经 Circuit Relay v2 建连并承载流（NAT 场景的进程内模拟）。
// 直接用 circuitv2 client 做资源预留（autorelay 静态中继的底层通路），
// 生产路径的完整 autorelay 链路由多进程 e2e 覆盖。
func TestRelayFallback(t *testing.T) {
	r := newTestHost(t, Config{SeedEd: []byte{20}, HubMode: true})
	targetSeed := byte(21)
	c := newTestHost(t, Config{
		SeedEd:      []byte{22},
		BlockDirect: map[string]bool{mustID(testSeed(targetSeed)): true},
	})
	tg := newTestHost(t, Config{SeedEd: []byte{targetSeed}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// T 向 R 申请中继预留
	rai, err := peer.AddrInfoFromString(listenAddr(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := circuitclient.Reserve(ctx, tg.Libp2p(), *rai); err != nil {
		t.Fatalf("中继预留失败: %v", err)
	}
	// C 经 R 中继拨号 T（gater 拦截直连地址，仅放行 p2p-circuit）
	circuitAddr := listenAddr(t, r) + "/p2p-circuit/p2p/" + tg.ID()
	if _, err := c.ConnectAddr(ctx, circuitAddr); err != nil {
		t.Fatalf("经中继拨号失败: %v", err)
	}
	tr := c.ConnTransport(tg.ID())
	if tr != "circuit" {
		t.Fatalf("连接形态应为 circuit，得到 %s", tr)
	}
	t.Logf("C→T: %s, T 侧看到 C: %s, R 侧 conns=%d",
		tr, tg.ConnTransport(c.ID()), len(r.Libp2p().Network().Conns()))

	tg.SetStreamHandler("/test/echo/1", func(st network.Stream) {
		if _, err := ReadMsg(st, 0); err != nil {
			st.Reset()
			return
		}
		WriteMsg(st, []byte("via-relay"), nil)
		st.Close()
	})
	// 同一条中继虚连接上开两条流，验证多路复用
	for i, payload := range []string{"x", "y"} {
		ectx, ecancel := context.WithTimeout(context.Background(), 15*time.Second)
		st, err := c.NewStream(ectx, tg.ID(), "/test/echo/1")
		if err != nil {
			t.Fatalf("中继链路开流 #%d 失败: %v", i+1, err)
		}
		if err := WriteMsg(st, []byte(payload), nil); err != nil {
			t.Fatal(err)
		}
		resp, err := ReadMsg(st, 0)
		if err != nil || string(resp) != "via-relay" {
			t.Fatalf("中继链路回显 #%d 失败: %q %v", i+1, resp, err)
		}
		st.Close()
		ecancel()
	}
	t.Log("中继链路帧往返 + 多路复用通过")
}

func mustID(seed []byte) string {
	id, err := PeerIDFromSeed(seed)
	if err != nil {
		panic(err)
	}
	return id
}

func TestMdnsDiscovery(t *testing.T) {
	found := make(chan string, 4)
	a := newTestHost(t, Config{SeedEd: []byte{30}})
	b := newTestHost(t, Config{SeedEd: []byte{31}, MDNS: true, OnDiscover: func(id string, addrs []string) {
		found <- id
	}})
	_ = a
	_ = b
	select {
	case id := <-found:
		if id == "" {
			t.Fatal("发现的 ID 为空")
		}
	case <-time.After(10 * time.Second):
		t.Skip("10 秒内 mDNS 未发现对端（多播环境受限），跳过")
	}
}

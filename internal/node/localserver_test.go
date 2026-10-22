package node

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAutoJoinLocalServer 本机服务自动组网：客户端对 127.0.0.1 上的 lotus 服务
// /api/net/info 探测成功 → 按普通入网 join；该 hub 为回环地址 → IsLocalService 判真
//（前端「本机」分区依据）。同时验证不可达时静默跳过。
func TestAutoJoinLocalServer(t *testing.T) {
	env := newEnv(t)
	srv := env.startNode(1)
	client := env.startNode(2)

	// 伪造 /api/net/info（api 层同构应答），QUIC 端口取服务节点真实监听端口
	quicPort := 0
	for _, a := range srv.Host.Libp2p().Addrs() {
		s := a.String()
		if strings.HasPrefix(s, "/ip4/127.0.0.1/udp/") {
			var p int
			if _, err := fmt.Sscanf(s, "/ip4/127.0.0.1/udp/%d/", &p); err == nil {
				quicPort = p
				break
			}
		}
	}
	if quicPort == 0 {
		t.Fatal("未找到服务节点 QUIC 监听端口")
	}
	infoSrv := http.NewServeMux()
	infoSrv.HandleFunc("/api/net/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "picnet", "v": 2, "id": srv.PeerID, "name": "本机服务", "quicPort": quicPort,
		})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: infoSrv}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	httpPort := ln.Addr().(*net.TCPAddr).Port

	// 自动加入：joinOne 走完整流程（重探测 → TOFU → 拨号 → 持久化）
	res := client.Mgr.tryJoinLocalServer("127.0.0.1", httpPort)
	if !res.OK {
		t.Fatalf("自动加入失败: %+v", res)
	}
	if res.Node != srv.PeerID {
		t.Fatalf("加入的节点身份不符: %s != %s", res.Node, srv.PeerID)
	}
	waitFor(t, 10*time.Second, "服务端确认连接", func() bool {
		return srv.Host.Connected(client.PeerID)
	})

	// hub 落盘 + 回环判定（前端「本机」分区依据）
	found := false
	for _, h := range client.Mgr.Hubs() {
		if h == fmt.Sprintf("127.0.0.1:%d", httpPort) {
			found = true
		}
	}
	if !found {
		t.Fatalf("hub 未记录本机服务地址: %+v", client.Mgr.Hubs())
	}
	if !client.Mgr.IsLocalService(srv.PeerID) {
		t.Fatal("本机服务未被判定为 local（前端会归入网络分区）")
	}
	// 非回环 hub 不误判：手工记一个假外网 hub
	client.Mgr.mu.Lock()
	client.Mgr.known["192.168.1.9:3456"] = srv.PeerID
	client.Mgr.hubs = append(client.Mgr.hubs, "192.168.1.9:3456")
	client.Mgr.mu.Unlock()
	if !client.Mgr.IsLocalService(srv.PeerID) {
		t.Fatal("存在回环 hub 时应仍判真")
	}

	// 不可达端口：静默失败，不产生 hub
	res2 := client.Mgr.tryJoinLocalServer("127.0.0.1", closedPort(t))
	if res2.OK {
		t.Fatal("不可达端口不应加入成功")
	}
	for _, h := range client.Mgr.Hubs() {
		if h == fmt.Sprintf("127.0.0.1:%d", closedPort(t)) {
			t.Fatal("失败探测不应留下 hub 记录")
		}
	}
}

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // 端口已释放：探测应失败
	return port
}

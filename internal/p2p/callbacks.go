package p2p

import (
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

func peerDecode(pid string) (peer.ID, error) { return peer.Decode(pid) }

// Callbacks 运行期回调（可在 NewHost 之后注入）。
type Callbacks struct {
	OnConn      func(pid string, connected bool)
	OnGossip    func(from string, data []byte)
	OnDiscover  func(id string, addrs []string)
	OnReserve   func(active bool)
	PublishSelf func() error
}

// SetCallbacks 注入/覆盖运行期回调（与 Config 中的同名字段效果一致）。
func (h *Host) SetCallbacks(cb Callbacks) {
	if cb.OnConn != nil {
		h.cfg.OnConn = cb.OnConn
	}
	if cb.OnGossip != nil {
		h.cfg.OnGossip = cb.OnGossip
	}
	if cb.OnDiscover != nil {
		h.cfg.OnDiscover = cb.OnDiscover
	}
	if cb.OnReserve != nil {
		h.cfg.OnReserve = cb.OnReserve
	}
	if cb.PublishSelf != nil {
		h.cfg.PublishSelf = cb.PublishSelf
	}
}

// Disconnect 断开与某对端的全部连接并同步等待消失（否则后续拨号会复用半死连接）。
func (h *Host) Disconnect(pid string) {
	p, err := peerDecode(pid)
	if err != nil {
		return
	}
	for _, c := range h.h.Network().ConnsToPeer(p) {
		c.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.h.Network().ConnsToPeer(p)) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// DisconnectAll 断开全部连接。
func (h *Host) DisconnectAll() {
	for _, c := range h.h.Network().Conns() {
		c.Close()
	}
}

// WaitForConnect 等待与对端建立连接（轮询，测试辅助）。
func (h *Host) WaitForConnect(pid string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.Connected(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

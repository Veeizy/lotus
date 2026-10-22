package node

import (
	"github.com/libp2p/go-libp2p/core/network"

	"lotus/internal/stats"
)

/* ---------------- 运行观测：流量计数 + 访问埋点（lotus-server 查询面数据源） ----------------
 * deps.Stats 为 nil 时全部退化为直通（lotus 单机版零开销）。
 * 流量口径：五协议节点流的应用层字节（gossipsub/mDNS/中继控制面不计）。
 * 计数按【传输对端】归属：转发场景中「收自下一跳」与「发给请求方」分别入账，
 * 两跳之和即为该图经本节点的总吞吐。 */

// countStream 包装 libp2p 流，按方向把应用层字节计入对端流量。
type countStream struct {
	network.Stream
	m    *Manager
	peer string
}

func (c *countStream) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	if n > 0 {
		c.m.deps.Stats.AddTraffic(c.peer, true, int64(n))
	}
	return n, err
}

func (c *countStream) Write(p []byte) (int, error) {
	n, err := c.Stream.Write(p)
	if n > 0 {
		c.m.deps.Stats.AddTraffic(c.peer, false, int64(n))
	}
	return n, err
}

// countWrap 为入站流套上计数壳（deps.Stats 为 nil 时原样返回）。
func (m *Manager) countWrap(s network.Stream) network.Stream {
	if m.deps.Stats == nil {
		return s
	}
	return &countStream{Stream: s, m: m, peer: s.Conn().RemotePeer().String()}
}

// logAccess 记录访问事件（deps.Stats 为 nil 时为空操作）。
func (m *Manager) logAccess(e stats.AccessEvent) {
	if m.deps.Stats == nil {
		return
	}
	m.deps.Stats.LogAccess(e)
}

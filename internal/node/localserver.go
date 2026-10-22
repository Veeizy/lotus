package node

import (
	"context"
	"net"
	"strings"
	"time"

	"lotus/internal/core"
)

/* ---------------- 本机服务自动组网（客户端 + 本机 lotus-server 双进程共存） ----------------
 * 客户端与服务端用两套默认端口（core.Server*Port / core.Node*Port），同一台机器
 * 可以同时跑两个进程。客户端启动时对 127.0.0.1:ServerHTTPPort 做一次 /api/net/info
 * 探测，发现本机服务端就按普通入网流程 join（TOFU/持久化/重连全部复用现有机制）。
 * 前端把「hub 地址是回环」的成员视为本机服务：与客户端自身内容同属一个信源
 * （同一台机器），根视图里显示在「本机」分区而非「网络·其他人」。 */

// autoJoinLocalServer 启动时后台探测本机服务端并加入（net.autoJoinLocal 控制）。
// 静默语义：本机没有服务端是常态，探测失败不算错误。
func (m *Manager) autoJoinLocalServer() JoinResult {
	return m.tryJoinLocalServer("127.0.0.1", core.ServerHTTPPort)
}

// tryJoinLocalServer 探测 host:httpPort 上的 lotus 服务并按普通入网流程加入
// （探测身份 → TOFU → 持久化 hub）。已在该地址或对方是自己时静默跳过。
func (m *Manager) tryJoinLocalServer(host string, httpPort int) JoinResult {
	addr := net_JoinHostPort(host, httpPort)
	m.mu.RLock()
	for _, h := range m.hubs {
		if h == addr {
			m.mu.RUnlock()
			return JoinResult{Addr: addr, OK: true, Note: "已存在"}
		}
	}
	m.mu.RUnlock()
	// 快速探测（3s）：本机没有服务端是常态，慢失败直接放弃
	ctx, cancel := context.WithTimeout(m.deps.Ctx, 3*time.Second)
	defer cancel()
	pid, _, err := m.probePeerID(ctx, host, httpPort)
	if err != nil {
		m.log.Debug("本机服务探测失败", "addr", addr, "err", err.Error())
		return JoinResult{Addr: addr, OK: false, Error: "本机服务不可达"}
	}
	if pid == m.selfID {
		// 探测目标是自己的身份 = 两进程共用了状态文件（部署错误），提示排查
		m.log.Warn("本机 3456 上的节点身份与自身相同：服务端与客户端共用了身份文件，自动组网跳过", "addr", addr)
		return JoinResult{Addr: addr, OK: false, Error: "对方是自己的身份"}
	}
	// 本会话内被显式断开的节点不回连（RemoveHub 会记 leftAt）
	m.mu.RLock()
	_, suppressed := m.leftAt[pid]
	m.mu.RUnlock()
	if suppressed {
		return JoinResult{Addr: addr, OK: false, Error: "已显式断开"}
	}
	return m.joinOne(addr, false)
}

// LocalServicePeer 返回本机服务（回环 hub）的 PeerID；无则空串。
// 供 app 层周期拉取其文件夹列表（服务根并入本地图库，直接读盘）。
func (m *Manager) LocalServicePeer() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, h := range m.hubs {
		if isLoopbackHubAddr(h) {
			if pid := m.known[h]; pid != "" {
				return pid
			}
		}
	}
	return ""
}

// IsLocalService 判定某节点是否本机服务：作为 hub 记录且地址为回环
// （127.0.0.1/::1/localhost）。自动加入的本机服务端与手工加入的本机节点都算——
// 它们都与客户端同机，属同一个信源。
func (m *Manager) IsLocalService(pid string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, h := range m.hubs {
		if m.known[h] == pid && isLoopbackHubAddr(h) {
			return true
		}
	}
	return false
}

// isLoopbackHubAddr hub 地址是否回环：host:port（join 落盘的 HTTP 形式）
// 或 /ip4|ip6/<addr>/udp/… multiaddr 形式。
func isLoopbackHubAddr(addr string) bool {
	if strings.HasPrefix(addr, "/") {
		parts := strings.Split(addr, "/")
		if len(parts) >= 3 {
			switch parts[1] {
			case "ip4", "ip6", "ip":
				if ip := net.ParseIP(parts[2]); ip != nil {
					return ip.IsLoopback()
				}
			}
		}
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

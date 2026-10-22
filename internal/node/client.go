package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/libp2p/go-libp2p/core/network"

	"lotus/internal/crypto"
	"lotus/internal/p2p"
)

// callRaw 打开协议流：写请求 → 读应答头 → 返回头字节与剩余 body（image 密文体）。
// 非 image 协议 body 为已关闭的空 reader（调用方无需读）。
func (m *Manager) callRaw(ctx context.Context, pid, proto string, req *NodeRequest) ([]byte, io.ReadCloser, error) {
	head, body, err := m.callRawOnce(ctx, pid, proto, req)
	if err != nil {
		// 对端重启后本端可能仍握着僵尸连接（QUIC 靠无状态重置才能发现），
		// 首试失败即强制断开（同步等待消失）重连再试一次
		m.deps.Host.Disconnect(pid)
		head, body, err = m.callRawOnce(ctx, pid, proto, req)
	}
	return head, body, err
}

func (m *Manager) callRawOnce(ctx context.Context, pid, proto string, req *NodeRequest) ([]byte, io.ReadCloser, error) {
	// 确保存在连接（swarm 按 peerstore 地址拨号；不限制 limited——中继是合法路径）
	if err := m.deps.Host.ConnectPeer(ctx, pid); err != nil {
		return nil, nil, fmt.Errorf("拨号失败: %w", err)
	}
	s, err := m.deps.Host.NewStream(ctx, pid, proto)
	if err != nil {
		return nil, nil, fmt.Errorf("开流失败: %w", err)
	}
	s = m.countWrap(s) // 请求写出 + 应答读入计入对端流量
	reqBytes, err := json.Marshal(req)
	if err != nil {
		s.Reset()
		return nil, nil, err
	}
	m.log.Debug("出站请求", "to", shortID(pid), "proto", proto, "path", req.Path, "target", shortID(req.Target))
	if err := p2p.WriteMsg(s, reqBytes, m.delay); err != nil {
		s.Reset()
		return nil, nil, fmt.Errorf("写请求失败: %w", err)
	}
	headBytes, err := p2p.ReadMsg(s, 0)
	if err != nil {
		s.Reset()
		return nil, nil, fmt.Errorf("读应答失败: %w", err)
	}
	if proto == ProtoImage {
		return headBytes, &streamCloser{Stream: s}, nil
	}
	s.Close()
	return headBytes, io.NopCloser(&emptyReader{}), nil
}

type emptyReader struct{}

func (e *emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

type streamCloser struct {
	network.Stream
	closed bool
}

func (sc *streamCloser) Close() error {
	if !sc.closed {
		sc.closed = true
		sc.Stream.Reset()
	}
	return nil
}

// callSimple 调用 roots/browse/times/stat，解析 JSON 头。
func (m *Manager) callSimple(ctx context.Context, pid, proto string, req *NodeRequest) (*NodeRespHead, error) {
	headBytes, body, err := m.callRaw(ctx, pid, proto, req)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var head NodeRespHead
	if err := json.Unmarshal(headBytes, &head); err != nil {
		return nil, fmt.Errorf("应答格式错误: %w", err)
	}
	return &head, nil
}

// RequestRoots / RequestBrowse / RequestTimes / RequestStat 的便捷封装。
func (m *Manager) RequestRoots(ctx context.Context, pid string) (*NodeRespHead, error) {
	return m.callSimple(ctx, pid, ProtoRoots, &NodeRequest{})
}

func (m *Manager) RequestBrowse(ctx context.Context, pid, rpath string) (*NodeRespHead, error) {
	return m.callSimple(ctx, pid, ProtoBrowse, &NodeRequest{Path: rpath})
}

func (m *Manager) RequestTimes(ctx context.Context, pid, rpath string) (*NodeRespHead, error) {
	return m.callSimple(ctx, pid, ProtoTimes, &NodeRequest{Path: rpath})
}

func (m *Manager) RequestStat(ctx context.Context, pid, rpath string) (*NodeRespHead, error) {
	return m.callSimple(ctx, pid, ProtoStat, &NodeRequest{Path: rpath})
}

// requestImage 按 §3.5 路由请求目标图片（发起方：Via 以自身起始，无上一跳排除）。
func (m *Manager) requestImage(ctx context.Context, target, rpath, pubX string) ([]byte, io.ReadCloser, error) {
	return m.routeImage(ctx, &NodeRequest{Path: rpath, PubX: pubX, Target: target, Via: []string{m.selfID}}, target, "")
}

// routeImage 请求路由：按已知地址拨号直连目标（短预算）→ 失败交给已连接的
// 其他节点（hub 优先，排除 exclude=上一跳）以 target 触发代答/递归转发。
// 环路防护由 Via 节点记忆承担（各转发节点自检），此处不做跳数限制。
func (m *Manager) routeImage(ctx context.Context, req *NodeRequest, target, exclude string) ([]byte, io.ReadCloser, error) {
	// 路由优先级：
	//  1. 已与目标存在连接 → 直连请求（一跳、最新鲜）；
	//  2. 链路转发（hub 优先，排除上一跳）——途经节点可用缓存代答，
	//     这是链路缓存生效的机会窗口（NAT 隔离形态下的主路径）；
	//  3. 兜底：按已知地址（attestation 宣告）按需拨号直连（5s 预算）。
	// 环路防护由 Via 节点记忆承担：链路阶段的任何绕环都会被途经节点立即拒绝。
	if m.deps.Host.Connected(target) {
		head, body, err := m.callRaw(ctx, target, ProtoImage, req)
		if err == nil {
			return head, body, nil
		}
	}
	vias := m.forwardCandidates(target, exclude)
	var lastErr error
	for _, via := range vias {
		head, body, err := m.callRaw(ctx, via, ProtoImage, req)
		if err == nil {
			return head, body, nil
		}
		lastErr = err
	}
	dialCtx, cancel := context.WithTimeout(ctx, DirectDialTimeout)
	dialErr := m.deps.Host.ConnectPeer(dialCtx, target)
	cancel()
	if dialErr == nil {
		head, body, err := m.callRaw(ctx, target, ProtoImage, req)
		if err == nil {
			return head, body, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无可用路径到达目标")
	}
	return nil, nil, lastErr
}

// ForwardCandidates 已连接且可作为转发者的节点：hub 优先、直连优先。
func (m *Manager) ForwardCandidates(exclude string) []string { return m.forwardCandidates(exclude, "") }

func (m *Manager) forwardCandidates(target, prevHop string) []string {
	m.mu.RLock()
	hubSet := map[string]bool{}
	for _, addr := range m.hubs {
		if pid := m.known[addr]; pid != "" {
			hubSet[pid] = true
		}
	}
	var cands []string
	for pid := range m.members {
		if pid == m.selfID || pid == target || pid == prevHop {
			continue
		}
		if m.deps.Host.Connected(pid) {
			cands = append(cands, pid)
		}
	}
	m.mu.RUnlock()
	sort.Slice(cands, func(i, j int) bool {
		hi, hj := hubSet[cands[i]], hubSet[cands[j]]
		if hi != hj {
			return hi
		}
		ti, tj := m.deps.Host.ConnTransport(cands[i]) == "direct", m.deps.Host.ConnTransport(cands[j]) == "direct"
		if ti != tj {
			return ti
		}
		return cands[i] < cands[j]
	})
	if len(cands) > MaxForwardCandidates {
		cands = cands[:MaxForwardCandidates]
	}
	return cands
}

// PubXSelf 本机明钥的 b64url（image 请求必附）。
func (m *Manager) PubXSelf() string {
	return crypto.B64uEnc(m.deps.ID.PubX)
}

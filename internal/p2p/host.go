// Package p2p：libp2p 的集中封装（架构边界：全项目唯一允许 import libp2p 的包）。
//
// 覆盖：QUIC-only host、身份（seedEd → ed25519 identity → PeerID）、
// mDNS 发现、Circuit Relay v2（hub 服务端 + leaf 静态中继客户端）、DCUtR 打洞、
// gossipsub 宣告通道、rcmgr 资源预算、连接事件。
package p2p

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/multiformats/go-multiaddr"
)
// GossipTopic 单一宣告主题（PROTOCOL §7）。
const GossipTopic = "picnet/attest/v1"

const MdnsService = "_picnet._udp.local"

type Config struct {
	SeedEd       []byte // Ed25519 种子（= libp2p identity 种子）
	ListenPort   int    // QUIC/UDP 端口；0 = 随机
	HubMode      bool   // 启用 Circuit Relay v2 服务（hub 节点）
	StaticRelays []string
	MDNS         bool
	Advertise    string          // 对外宣告 multiaddr（空 = 自动选择）
	BlockDirect  map[string]bool // 测试：禁止与这些 PeerID 直拨（强制走 relay）
	Logger       *slog.Logger

	// 回调（node 层注入）
	OnConn      func(pid string, connected bool)
	OnGossip    func(from string, data []byte)
	OnDiscover  func(id string, addrs []string)
	OnReserve   func(active bool) // 有无经本节点中继的连接（hub 标志）
	HeartbeatMS int               // 测试可调；0 = 默认
	PublishSelf func() error      // node 层提供「宣告我的 attestation」载荷发布
}

type Host struct {
	cfg   Config
	log   *slog.Logger
	h     host.Host
	ps    *pubsub.PubSub
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	ctx   context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	mdnsSvc  mdnsRunner
	relaySvc *relay.Relay
	closing  bool
}

// PeerIDString 计算种子对应的 PeerID（不启动网络）。
func PeerIDFromSeed(seedEd []byte) (string, error) {
	priv, err := privFromSeed(seedEd)
	if err != nil {
		return "", err
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return "", err
	}
	return pid.String(), nil
}

func privFromSeed(seedEd []byte) (crypto.PrivKey, error) {
	if len(seedEd) != 32 {
		return nil, errors.New("ed25519 种子长度错误")
	}
	priv64 := ed25519.NewKeyFromSeed(seedEd)
	return crypto.UnmarshalEd25519PrivateKey(priv64)
}

// NewHost 构造并启动 libp2p host（QUIC-only）。
func NewHost(ctx context.Context, cfg Config) (*Host, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	priv, err := privFromSeed(cfg.SeedEd)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithCancel(ctx)
	h := &Host{cfg: cfg, log: cfg.Logger, ctx: hctx, cancel: cancel}

	listen := fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", cfg.ListenPort)
	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.Transport(libp2pquic.NewTransport), // QUIC-only：显式指定传输后默认 TCP/WS 不再启用
		libp2p.ListenAddrs(mustMultiaddr(listen)),
		libp2p.NATPortMap(),
		libp2p.EnableHolePunching(),
	}
	if len(cfg.StaticRelays) > 0 {
		infos, err := parseAddrInfos(cfg.StaticRelays)
		if err != nil {
			cancel()
			return nil, err
		}
		opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(infos))
	}
	if len(cfg.BlockDirect) > 0 {
		opts = append(opts, libp2p.ConnectionGater(&blockDirectGater{deny: cfg.BlockDirect}))
	}

	hh, err := libp2p.New(opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	h.h = hh

	// hub：无条件启动 Circuit Relay v2 服务（不等待 autonat 公网可达性判定——
	// 我们的拓扑中 hub 由用户显式指定，必须始终可中继）。
	// 预留限额：单方向 512MB / 1 小时，最多 64 个预留。
	if cfg.HubMode {
		res := relay.DefaultResources()
		res.MaxReservations = 64
		res.MaxCircuits = 16
		res.Limit = &relay.RelayLimit{Data: 512 << 20, Duration: time.Hour}
		rs, err := relay.New(hh, relay.WithResources(res))
		if err != nil {
			hh.Close()
			cancel()
			return nil, fmt.Errorf("中继服务启动失败: %w", err)
		}
		h.relaySvc = rs
	}

	// 连接事件
	hh.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(n network.Network, c network.Conn) {
			if h.cfg.OnConn != nil {
				h.cfg.OnConn(c.RemotePeer().String(), true)
			}
			_ = h.publishSelf() // 连接建立即宣告（PROTOCOL §4.1）
		},
		DisconnectedF: func(n network.Network, c network.Conn) {
			if h.cfg.OnConn != nil {
				// 只有当对端再无任何连接时才算断开
				if len(hh.Network().ConnsToPeer(c.RemotePeer())) == 0 {
					h.cfg.OnConn(c.RemotePeer().String(), false)
				}
			}
		},
	})

	// gossipsub
	ps, err := pubsub.NewGossipSub(hctx, hh)
	if err != nil {
		hh.Close()
		cancel()
		return nil, fmt.Errorf("gossipsub 启动失败: %w", err)
	}
	h.ps = ps
	topic, err := ps.Join(GossipTopic)
	if err != nil {
		hh.Close()
		cancel()
		return nil, fmt.Errorf("gossip topic 加入失败: %w", err)
	}
	h.topic = topic
	sub, err := topic.Subscribe()
	if err != nil {
		hh.Close()
		cancel()
		return nil, err
	}
	h.sub = sub
	go h.gossipPump()

	// 周期宣告
	interval := 30 * time.Second
	if cfg.HeartbeatMS > 0 {
		interval = time.Duration(cfg.HeartbeatMS) * time.Millisecond
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				_ = h.publishSelf()
			}
		}
	}()

	// mDNS
	if cfg.MDNS {
		svc := mdns.NewMdnsService(hh, MdnsService, &mdnsNotifee{h: h})
		if err := svc.Start(); err != nil {
			h.log.Warn("mDNS 启动失败（局域网发现不可用）", "err", err)
		} else {
			h.mu.Lock()
			h.mdnsSvc = svc
			h.mu.Unlock()
		}
	}

	// hub 标志：周期检查是否存在经本节点中继的入站连接
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		was := false
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				now := h.hasRelayedConns()
				if now != was && cfg.OnReserve != nil {
					cfg.OnReserve(now)
				}
				was = now
			}
		}
	}()

	h.log.Info("libp2p host 就绪", "peer", hh.ID().String(), "listen", listen)
	return h, nil
}

func (h *Host) hasRelayedConns() bool {
	for _, c := range h.h.Network().Conns() {
		if c.Stat().Direction == network.DirInbound && strings.Contains(c.RemoteMultiaddr().String(), "p2p-circuit") {
			return true
		}
	}
	return false
}

type mdnsNotifee struct{ h *Host }

func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.h.h.ID() {
		return
	}
	var addrs []string
	for _, a := range pi.Addrs {
		addrs = append(addrs, a.String())
	}
	n.h.h.Peerstore().AddAddrs(pi.ID, pi.Addrs, peerstore.TempAddrTTL)
	if n.h.cfg.OnDiscover != nil {
		n.h.cfg.OnDiscover(pi.ID.String(), addrs)
	}
}

type mdnsRunner interface {
	Start() error
	Close() error
}

func (h *Host) gossipPump() {
	for {
		msg, err := h.sub.Next(h.ctx)
		if err != nil {
			return
		}
		if h.cfg.OnGossip != nil {
			h.cfg.OnGossip(msg.ReceivedFrom.String(), msg.Data)
		}
	}
}

// Publish 发布宣告载荷。
func (h *Host) Publish(data []byte) error {
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	return h.topic.Publish(ctx, data)
}

func (h *Host) publishSelf() error {
	if h.cfg.PublishSelf != nil {
		return h.cfg.PublishSelf()
	}
	return nil
}

func (h *Host) ID() string { return h.h.ID().String() }

func (h *Host) Libp2pID() peer.ID { return h.h.ID() }

func (h *Host) Libp2p() host.Host { return h.h }

// AdvertiseAddr 计算对外宣告地址：优先显式配置，其次内网 IPv4，再次经中继地址，最后回环。
func (h *Host) AdvertiseAddr() string {
	if h.cfg.Advertise != "" {
		return h.cfg.Advertise
	}
	var loopback, relayed, private string
	for _, a := range h.h.Addrs() {
		s := a.String()
		switch {
		case strings.Contains(s, "p2p-circuit"):
			if relayed == "" {
				relayed = s
			}
		case strings.HasPrefix(s, "/ip4/127.") || strings.HasPrefix(s, "/ip6/::1"):
			if loopback == "" {
				loopback = s
			}
		default:
			if ip4 := v4Private(s); ip4 && private == "" {
				private = s
			}
		}
	}
	if private != "" {
		return private
	}
	if relayed != "" {
		return relayed
	}
	return loopback
}

func v4Private(maStr string) bool {
	if !strings.HasPrefix(maStr, "/ip4/") {
		return false
	}
	ip := net.ParseIP(strings.Split(strings.TrimPrefix(maStr, "/ip4/"), "/")[0])
	if ip == nil {
		return false
	}
	return ip.IsPrivate()
}

// ConnTransport 判定与对端的连接形态："none" / "direct" / "circuit"。
func (h *Host) ConnTransport(pid string) string {
	p, err := peer.Decode(pid)
	if err != nil {
		return "none"
	}
	conns := h.h.Network().ConnsToPeer(p)
	if len(conns) == 0 {
		return "none"
	}
	for _, c := range conns {
		if !strings.Contains(c.RemoteMultiaddr().String(), "p2p-circuit") {
			return "direct"
		}
	}
	return "circuit"
}

// Connected 对端是否有任何活跃连接。
func (h *Host) Connected(pid string) bool { return h.ConnTransport(pid) != "none" }

// ConnectAddr 拨号一个地址（multiaddr 字符串），返回对端 PeerID。
func (h *Host) ConnectAddr(ctx context.Context, addr string) (string, error) {
	ma, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		return "", fmt.Errorf("地址格式错误: %w", err)
	}
	transport, idPart := peer.SplitAddr(ma)
	ai := peer.AddrInfo{ID: idPart}
	if transport != nil {
		ai.Addrs = []multiaddr.Multiaddr{transport}
	}
	if err := h.h.Connect(ctx, ai); err != nil {
		return "", err
	}
	if idPart == "" {
		// 无 /p2p/ 分量：从刚建立的连接取实际身份
		var last peer.ID
		for _, c := range h.h.Network().Conns() {
			last = c.RemotePeer()
		}
		if last != "" {
			return last.String(), nil
		}
		return "", errors.New("连接已建立但未识别对端身份")
	}
	return idPart.String(), nil
}

func stripP2p(ma multiaddr.Multiaddr) multiaddr.Multiaddr {
	s := ma.String()
	if i := strings.Index(s, "/p2p/"); i > 0 {
		if stripped, err := multiaddr.NewMultiaddr(s[:i]); err == nil {
			return stripped
		}
	}
	return ma
}

// ConnectPeer 确保与指定对端存在连接（用 peerstore 已知地址拨号）。
func (h *Host) ConnectPeer(ctx context.Context, pid string) error {
	p, err := peer.Decode(pid)
	if err != nil {
		return err
	}
	if len(h.h.Network().ConnsToPeer(p)) > 0 {
		return nil
	}
	return h.h.Connect(ctx, peer.AddrInfo{ID: p})
}

// AddAddrs 记录对端地址（来自 attestation 宣告）。
func (h *Host) AddAddrs(pid, addr string, ttl time.Duration) {
	p, err := peer.Decode(pid)
	if err != nil {
		return
	}
	ma, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		return
	}
	h.h.Peerstore().AddAddr(p, ma, ttl)
}

// PeerPubKey 提取对端身份公钥（ed25519 identity multihash 内嵌）。
func (h *Host) PeerPubKey(pid string) (ed25519.PublicKey, error) {
	p, err := peer.Decode(pid)
	if err != nil {
		return nil, err
	}
	pk, err := p.ExtractPublicKey()
	if err != nil {
		// 非内嵌公钥的 PeerID：退回 peerstore
		pk = h.h.Peerstore().PubKey(p)
		if pk == nil {
			return nil, errors.New("无法获取对端公钥")
		}
	}
	raw, err := pk.Raw()
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("对端公钥不是 ed25519")
	}
	return ed25519.PublicKey(raw), nil
}

// SetStreamHandler 注册协议处理器。
func (h *Host) SetStreamHandler(proto string, handler func(s network.Stream)) {
	h.h.SetStreamHandler(protocol.ID(proto), handler)
}

// NewStream 打开协议流。
// 必须带 WithAllowLimitedConn：中继连接在 swarm 里标记为 limited，
// 默认情况下 swarm 会拒绝在其上开流并一直等待直连——那会让中继回退永远超时。
// 本项目的拓扑中中继是一等路径（PROTOCOL §3.5），全部协议流均放行 limited 连接。
func (h *Host) NewStream(ctx context.Context, pid string, proto string) (network.Stream, error) {
	p, err := peer.Decode(pid)
	if err != nil {
		return nil, err
	}
	return h.h.NewStream(network.WithAllowLimitedConn(ctx, "picnet"), p, protocol.ID(proto))
}

// DiscoverPeers 返回 mDNS 最近发现的节点（供 /api/net/discover 由 node 层合并 att）。
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil
	}
	h.closing = true
	h.mu.Unlock()
	if h.sub != nil {
		h.sub.Cancel()
	}
	h.cancel()
	if h.mdnsSvc != nil {
		h.mdnsSvc.Close()
	}
	if h.relaySvc != nil {
		h.relaySvc.Close()
	}
	return h.h.Close()
}

func mustMultiaddr(s string) multiaddr.Multiaddr {
	ma, err := multiaddr.NewMultiaddr(s)
	if err != nil {
		panic(err)
	}
	return ma
}

func parseAddrInfos(addrs []string) ([]peer.AddrInfo, error) {
	var out []peer.AddrInfo
	for _, s := range addrs {
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("地址格式错误: %s", s)
		}
		ai, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			return nil, fmt.Errorf("地址缺少 /p2p/ 身份: %s", s)
		}
		out = append(out, *ai)
	}
	return out, nil
}

// blockDirectGater 测试用：拒绝与指定对端的非中继拨号（模拟不可直连的 NAT 环境）。
type blockDirectGater struct{ deny map[string]bool }

func (g *blockDirectGater) InterceptPeerDial(p peer.ID) bool { return true }

func (g *blockDirectGater) InterceptAddrDial(p peer.ID, a multiaddr.Multiaddr) bool {
	if !g.deny[p.String()] {
		return true
	}
	return strings.Contains(a.String(), "p2p-circuit")
}

func (g *blockDirectGater) InterceptAccept(c network.ConnMultiaddrs) bool { return true }

func (g *blockDirectGater) InterceptSecured(_ network.Direction, p peer.ID, c network.ConnMultiaddrs) bool {
	return true
}

func (g *blockDirectGater) InterceptUpgraded(c network.Conn) (bool, control.DisconnectReason) {
	return true, 0
}

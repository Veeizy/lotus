// Package node：组网核心（WP4-5）。把 p2p 传输、成员表、宣告、五协议服务、
// 链路缓存代答/转发、下载队列粘合成一个可运行的网络节点。
// 本包 import p2p 与 core，不 import net/http（管理面 HTTP 在 api 包）。
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lotus/internal/core"
	"lotus/internal/crypto"
	"lotus/internal/p2p"
	"lotus/internal/stats"
)

func net_JoinHostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
func http_NewRequestWithContext(ctx context.Context, method, url string, body any) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, url, nil)
}
const http_GET = http.MethodGet
func httpClient() *http.Client { return &http.Client{Timeout: JoinTimeout} }
func netParseIP(s string) net.IP { return net.ParseIP(s) }
func net_LookupHost(s string) ([]string, error) { return net.LookupHost(s) }
func json_NewDecoder(r io_Reader) jsonDecoder { return json.NewDecoder(r) }
type jsonDecoder interface{ Decode(any) error }
type io_Reader interface{ Read([]byte) (int, error) }

// 协议 ID（PROTOCOL §3.2，冻结）。
const (
	ProtoRoots  = "/picnet/roots/1"
	ProtoBrowse = "/picnet/browse/1"
	ProtoTimes  = "/picnet/times/1"
	ProtoStat   = "/picnet/stat/1"
	ProtoImage  = "/picnet/image/1"
)

// 契约常量（PROTOCOL §7）。
const (
	StatTimeout        = 8 * time.Second   // 链路缓存核对 stat 超时
	DirectDialTimeout  = 5 * time.Second   // 直连拨号预算（超时回退转发，同旧版语义）
	ReqTimeout         = 20 * time.Second  // browse/times/roots/stat 客户端超时
	ImageTimeout       = 300 * time.Second // 图片客户端超时
	JoinTimeout        = 5 * time.Second
	MaxImageSize       = 100 << 20 // 单图大小上限
	MaxForwardCandidates = 3       // 每级转发的候选节点数上限（扇出限制，非跳数限制）
	AttestFreshWindow  = 90 * time.Second
	AutoDialCap        = 32
)

// Settings 可热更的设置（name/share/cacheServe/concurrency/advertise/lanDiscovery/scanPorts）。
type Settings struct {
	Name         string
	Share        bool
	CacheServe   bool
	Concurrency  int
	Advertise    string
	LanDiscovery bool   // 局域网自动发现并入网（本机服务连接不受此开关控制）
	ScanPorts    []int  // 局域网扫描附加端口（默认 3456 恒在）
}

// Member 成员表条目。
type Member struct {
	Att       *crypto.Attestation
	LastFresh time.Time // 最近一次有效宣告时间
	Connected bool
}

// DiscoverInfo mDNS 发现条目。
type DiscoverInfo struct {
	ID       string
	Addrs    []string
	LastSeen time.Time
}

// JoinResult /api/net/join results[] 的单项。
type JoinResult struct {
	Addr  string `json:"addr"`
	OK    bool   `json:"ok"`
	Node  string `json:"node,omitempty"`
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
}

// EventBus 供 api 层 SSE 推送（成员变化/进度）。
type EventBus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

type Event struct {
	Kind    string // "members" | "progress"
	Payload any
}

func NewEventBus() *EventBus {
	return &EventBus{subs: map[chan Event]struct{}{}}
}

func (b *EventBus) Subscribe() chan Event {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *EventBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

func (b *EventBus) Publish(kind string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- Event{kind, payload}:
		default: // 慢消费者丢事件（SSE 有周期全量兜底）
		}
	}
}

// Deps 组装依赖（app 层注入）。
type Deps struct {
	Ctx       context.Context
	Lib       *core.Library
	Cache     *core.Cache
	Queue     *core.Queue
	Host      *p2p.Host
	ID        *crypto.Identity
	StatePath string
	Cfg       *core.Config
	SaveCfg   func() error // 持久化配置（join/leave/settings 后回调）
	Logger    *slog.Logger
	Events      *EventBus
	Stats       *stats.Collector // 运行观测（nil = 不采集，lotus 单机版）
	NoAutoJoin  bool             // 测试开关：关闭全部自动入网（本机服务 + 局域网发现）
	DelayMS     int              // 测试：流 I/O 模拟延迟
	FreshSec    int              // 测试：在线新鲜度窗口（秒）；0 = 默认
}

// Manager 网络节点管理器。
type Manager struct {
	deps Deps
	log  *slog.Logger

	selfID string

	mu        sync.RWMutex
	settings  Settings
	hubs      []string // 规范化 multiaddr 列表
	known     map[string]string
	atts      map[string]*crypto.Attestation // PeerID → att
	members   map[string]*Member
	discover  map[string]*DiscoverInfo
	hubActive bool // 存在经本节点中继的连接（hub 标志）

	dialMu     sync.Mutex
	lastDial   map[string]time.Time
	dialFails  map[string]int
	leftAt     map[string]int64 // 已收到下线通知的节点 → 通知 ts（抑制自动拨号；更新鲜的宣告解除）

	lastLocalJoinTry time.Time // 本机服务自动组网的上次尝试（重试节流）
	lastLanScan      time.Time // 上次局域网扫描触发时间（60s 节流）

	delay p2p.ChunkDelay
}

func NewManager(d Deps) (*Manager, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	m := &Manager{
		deps:      d,
		log:       d.Logger,
		selfID:    d.Host.ID(),
		settings: Settings{
			Name:         d.Cfg.Net.Name,
			Share:        d.Cfg.Net.Share,
			CacheServe:   d.Cfg.Net.CacheServe,
			Concurrency:  d.Cfg.Net.Concurrency,
			Advertise:    d.Cfg.Net.Advertise,
			LanDiscovery: d.Cfg.Net.LanDiscovery,
			ScanPorts:    append([]int(nil), d.Cfg.Net.ScanPorts...),
		},
		hubs:      append([]string{}, d.Cfg.Net.Hubs...),
		known:     map[string]string{},
		atts:      map[string]*crypto.Attestation{},
		members:   map[string]*Member{},
		discover:  map[string]*DiscoverInfo{},
		lastDial:  map[string]time.Time{},
		dialFails: map[string]int{},
		leftAt:    map[string]int64{},
	}
	for k, v := range d.Cfg.Net.Known {
		m.known[k] = v
	}
	m.delay = p2p.NewChunkDelay(d.DelayMS)

	// p2p 回调
	d.Host.SetCallbacks(p2p.Callbacks{
		OnConn:     m.onConn,
		OnGossip:   m.onGossip,
		OnDiscover: m.onDiscover,
		OnReserve:  func(active bool) { m.mu.Lock(); m.hubActive = active; m.mu.Unlock() },
		PublishSelf: m.publishAttestation,
	})

	// 注册五协议服务
	d.Host.SetStreamHandler(ProtoRoots, m.serveSimple(ProtoRoots))
	d.Host.SetStreamHandler(ProtoBrowse, m.serveSimple(ProtoBrowse))
	d.Host.SetStreamHandler(ProtoTimes, m.serveSimple(ProtoTimes))
	d.Host.SetStreamHandler(ProtoStat, m.serveSimple(ProtoStat))
	d.Host.SetStreamHandler(ProtoImage, m.serveImage)

	// 重连已配置的 hubs（后台，不阻塞启动）
	if len(m.hubs) > 0 {
		go func() {
			time.Sleep(300 * time.Millisecond)
			for _, addr := range m.hubs {
				m.joinOne(addr, false)
			}
		}()
	}

	// 自动加入本机服务端（同机「常驻服务 + 客户端」双进程形态；静默探测）。
	// 本机连接默认且始终开启，不受 lanDiscovery 控制；NoAutoJoin 仅供测试全局关闭。
	if !m.deps.NoAutoJoin {
		go func() {
			time.Sleep(800 * time.Millisecond) // 让 hub 重连先走，避免重复 join 竞态
			if res := m.autoJoinLocalServer(); res.OK {
				m.log.Info("已自动加入本机服务端", "addr", res.Addr, "node", shortID(res.Node))
			}
		}()
		// 局域网自动发现：启动后先扫一轮，之后由维护循环每 60s 补扫
		go func() {
			time.Sleep(10 * time.Second)
			m.lanDiscoveryCycle()
		}()
	}

	// 成员表维护 + 自动拨号 + 进度事件
	go m.maintainLoop()
	return m, nil
}

func (m *Manager) SelfID() string { return m.selfID }

// SetLibrary / SetCache 运行时热切换图库与缓存索引（配置面板用）。
func (m *Manager) SetLibrary(l *core.Library) {
	m.mu.Lock()
	m.deps.Lib = l
	m.mu.Unlock()
}

func (m *Manager) SetCache(c *core.Cache) {
	m.mu.Lock()
	m.deps.Cache = c
	m.mu.Unlock()
}

func (m *Manager) lib() *core.Library {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.deps.Lib
}

func (m *Manager) cache() *core.Cache {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.deps.Cache
}

func (m *Manager) freshWindow() time.Duration {
	if m.deps.FreshSec > 0 {
		return time.Duration(m.deps.FreshSec) * time.Second
	}
	return AttestFreshWindow
}

/* ---------------- 宣告（PROTOCOL §4.1） ---------------- */

func (m *Manager) attestation() *crypto.Attestation {
	m.mu.RLock()
	adv := m.settings.Advertise
	name := m.settings.Name
	m.mu.RUnlock()
	addr := adv
	if addr == "" {
		addr = m.deps.Host.AdvertiseAddr()
	}
	if name == "" {
		name = defaultNodeName()
	}
	return m.deps.ID.SignAttestation(m.selfID, name, addr, time.Now().UnixMilli())
}

func (m *Manager) publishAttestation() error {
	att := m.attestation()
	err := m.deps.Host.Publish(crypto.AttestationJSON(att))
	if err != nil {
		m.log.Debug("宣告发布失败", "err", err)
	} else {
		m.log.Debug("宣告已发布", "name", att.Name, "addr", att.Addr)
	}
	return err
}

func (m *Manager) onGossip(from string, data []byte) {
	// 消息分派：leave 通知 / attestation 宣告（PROTOCOL §9-11）
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &probe); err == nil && probe.Kind == "leave" {
		m.onLeaveNotice(data)
		return
	}
	att, err := crypto.ParseAttestation(data)
	if err != nil {
		m.log.Debug("gossip 解析失败", "err", err)
		return
	}
	if att.ID == m.selfID {
		return
	}
	// 注：不做 att.ID == from 校验——多跳 gossip 的 ReceivedFrom 是上一跳而非作者；
	// 身份真实性由「签名对 att.ID 内嵌公钥验证」承担（PROTOCOL §4.1），签名即身份证明。
	if !withinTolerance(att.TS, 60*time.Second) {
		return
	}
	pub, err := m.deps.Host.PeerPubKey(att.ID)
	if err != nil {
		return
	}
	if !crypto.VerifyAttestation(att, pub) {
		m.log.Warn("宣告验签失败，丢弃", "peer", shortID(att.ID))
		return
	}
	m.deps.Host.AddAddrs(att.ID, att.Addr, 24*time.Hour)

	m.log.Debug("宣告已接受", "peer", shortID(att.ID), "name", att.Name)
	m.mu.Lock()
	if leftTs, ok := m.leftAt[att.ID]; ok && att.TS > leftTs {
		delete(m.leftAt, att.ID) // 节点带着更新鲜的宣告回来了
	}
	m.mu.Unlock()
	m.mu.Lock()
	mem, ok := m.members[att.ID]
	if !ok {
		mem = &Member{}
		m.members[att.ID] = mem
	}
	mem.Att = att
	mem.LastFresh = time.Now()
	mem.Connected = m.deps.Host.Connected(att.ID)
	m.mu.Unlock()

	if idx := m.cache().Lookup(att.ID); idx != nil {
		m.mu.RLock()
		idx.Name = att.Name
		m.mu.RUnlock()
		m.cache().Mark(att.ID)
	}
	m.eventsMembers()
}

// onLeaveNotice 收到签名下线通知：验签后立即移除成员并抑制自动拨号。
// 级联语义无需显式处理——被孤立的子树宣告不再到达，全网按新鲜度收敛（§9-12）。
func (m *Manager) onLeaveNotice(data []byte) {
	var n crypto.LeaveNotice
	if err := json.Unmarshal(data, &n); err != nil || n.ID == "" || n.ID == m.selfID {
		return
	}
	if !withinTolerance(n.TS, 120*time.Second) {
		return
	}
	pub, err := m.deps.Host.PeerPubKey(n.ID)
	if err != nil || !crypto.VerifyLeaveNotice(&n, pub) {
		m.log.Warn("下线通知验签失败，忽略", "peer", shortID(n.ID))
		return
	}
	m.mu.Lock()
	if ts, ok := m.leftAt[n.ID]; ok && n.TS <= ts {
		m.mu.Unlock()
		return // 重复通知
	}
	m.leftAt[n.ID] = n.TS
	_, had := m.members[n.ID]
	delete(m.members, n.ID)
	m.mu.Unlock()
	if had {
		m.log.Info("节点已下线（签名通知）", "peer", shortID(n.ID))
	}
	m.eventsMembers()
}

func withinTolerance(ts int64, tol time.Duration) bool {
	d := time.Since(time.UnixMilli(ts))
	if d < 0 {
		d = -d
	}
	return d <= tol
}

/* ---------------- 连接与发现 ---------------- */

func (m *Manager) onConn(pid string, connected bool) {
	m.mu.Lock()
	if mem, ok := m.members[pid]; ok {
		mem.Connected = connected
		if connected {
			mem.LastFresh = time.Now()
		}
	}
	m.mu.Unlock()
	if connected {
		_ = m.publishAttestation()
		// gossipsub mesh 成形（约 1-2s）前发布的宣告会丢，重发脉冲兜底
		go func() {
			for _, d := range []time.Duration{3 * time.Second, 8 * time.Second} {
				select {
				case <-m.deps.Ctx.Done():
					return
				case <-time.After(d):
				}
				if m.deps.Host.Connected(pid) {
					_ = m.publishAttestation()
				}
			}
		}()
	}
	m.eventsMembers()
}

func (m *Manager) onDiscover(id string, addrs []string) {
	m.mu.Lock()
	di, ok := m.discover[id]
	if !ok {
		di = &DiscoverInfo{ID: id}
		m.discover[id] = di
	}
	di.Addrs = addrs
	di.LastSeen = time.Now()
	m.mu.Unlock()
	m.eventsMembers()
}

/* ---------------- 成员表维护 ---------------- */

func (m *Manager) maintainLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.deps.Ctx.Done():
			return
		case <-t.C:
			m.maintainOnce()
		}
	}
}

func (m *Manager) maintainOnce() {
	now := time.Now()
	// 自动拨号仅维护 hubs（hubs=bootstrap 语义）：普通成员由请求按需拨号，
	// 显式 leave 的节点不会被拉回。
	hubSet := map[string]bool{}
	for _, h := range m.hubs {
		if pid := m.known[h]; pid != "" {
			hubSet[pid] = true
		}
	}
	var toDial []string
	m.mu.Lock()
	for pid, mem := range m.members {
		if pid == m.selfID {
			continue
		}
		mem.Connected = m.deps.Host.Connected(pid)
		// 剪枝只看宣告新鲜度：QUIC 僵尸连接的检测需要 30s+，
		// 而宣告停更（节点死亡）在 freshWindow 内即可判定（PROTOCOL §4.3）
		if now.Sub(mem.LastFresh) > m.freshWindow() {
			delete(m.members, pid)
			continue
		}
		if _, left := m.leftAt[pid]; left {
			continue // 已宣告下线的节点不再自动拉回（更新鲜的宣告会解除）
		}
		if hubSet[pid] && !mem.Connected && now.Sub(m.lastDial[pid]) > backoffFor(m.dialFails[pid]) {
			toDial = append(toDial, pid)
		}
	}
	for id, di := range m.discover {
		if now.Sub(di.LastSeen) > 30*time.Second {
			delete(m.discover, id)
		}
	}
	changed := true
	_ = changed
	m.mu.Unlock()

	// 局域网自动发现（每 60s；开关与测试开关判定在 lanDiscoveryCycle 内）
	if now.Sub(m.lastLanScan) >= time.Minute {
		m.lastLanScan = now
		if !m.deps.NoAutoJoin {
			go m.lanDiscoveryCycle()
		}
	}

	// 本机服务自动组网重试：启动后的一次性探测有先后顺序缺口（桌面版先启动、
	// 服务后起，或首次探测偶发失败），这里周期性补试直到连上；已在 hubs 或
	// 本会话被显式断开（leftAt）则不再尝试。
	// 本机连接始终开启，仅 NoAutoJoin（测试）可关。
	if !m.deps.NoAutoJoin && now.Sub(m.lastLocalJoinTry) > 30*time.Second {
		m.lastLocalJoinTry = now
		m.mu.RLock()
		target := net_JoinHostPort("127.0.0.1", core.ServerHTTPPort)
		exists := false
		for _, h := range m.hubs {
			if h == target {
				exists = true
			}
		}
		m.mu.RUnlock()
		if !exists {
			go func() {
				if res := m.tryJoinLocalServer("127.0.0.1", core.ServerHTTPPort); res.OK {
					m.log.Info("已自动加入本机服务端", "addr", res.Addr, "node", shortID(res.Node))
				}
			}()
		}
	}

	if len(toDial) > 0 {
		for _, pid := range toDial {
			m.mu.Lock()
			m.lastDial[pid] = now
			m.mu.Unlock()
			go func(pid string) {
				ctx, cancel := context.WithTimeout(m.deps.Ctx, JoinTimeout)
				defer cancel()
				if err := m.deps.Host.ConnectPeer(ctx, pid); err != nil {
					m.mu.Lock()
					m.dialFails[pid]++
					m.mu.Unlock()
				} else {
					m.mu.Lock()
					m.dialFails[pid] = 0
					m.mu.Unlock()
				}
			}(pid)
		}
	}
}

func backoffFor(fails int) time.Duration {
	if fails <= 0 {
		return 0
	}
	d := time.Duration(1<<min(fails, 5)) * 2 * time.Second // 4s..64s 指数退避
	if d > 64*time.Second {
		d = 64 * time.Second
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m *Manager) memberCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.members)
}

func (m *Manager) eventsMembers() {
	m.deps.Events.Publish("members", nil)
}

func jsonMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

func defaultNodeName() string {
	// 计算机名的获取在 app 层注入 cfg.Net.Name；此处兜底
	return "node"
}

/* ---------------- Join / Leave / Settings ---------------- */

// NormalizeAddr 把用户输入的地址规范化为 multiaddr 字符串。
// 支持 /…/quic-v1[/p2p/ID] 与 host:port（省略端口取 3457）。
func NormalizeAddr(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "/") {
		return s, strings.Contains(s, "/quic-v1")
	}
	host := s
	port := 3457
	if strings.HasPrefix(s, "[") { // [::1]:3457
		i := strings.Index(s, "]")
		if i < 0 {
			return "", false
		}
		host = s[1:i]
		rest := s[i+1:]
		if strings.HasPrefix(rest, ":") {
			if p, err := strconv.Atoi(rest[1:]); err == nil && p > 0 && p < 65536 {
				port = p
			}
		}
		if strings.Contains(host, ":") {
			return fmt.Sprintf("/ip6/%s/udp/%d/quic-v1", host, port), true
		}
		return fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", host, port), true
	}
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s, "]") {
		host = s[:i]
		if p, err := strconv.Atoi(s[i+1:]); err == nil && p > 0 && p < 65536 {
			port = p
		}
	}
	if strings.Contains(host, ":") {
		return fmt.Sprintf("/ip6/%s/udp/%d/quic-v1", host, port), true
	}
	return fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", host, port), true
}

// Join 输入一个或多个 hub 地址入网。
// 地址形式：QUIC multiaddr（需含 /p2p/ 身份）或 HTTP 地址 host[:port]（默认 3456，
// 先经对方 /api/net/info 探测身份再拨号——与旧版输入体验一致）。
func (m *Manager) Join(addrs []string) []JoinResult {
	results := make([]JoinResult, 0, len(addrs))
	for _, raw := range addrs {
		s := strings.TrimSpace(raw)
		if s == "" {
			results = append(results, JoinResult{Addr: raw, OK: false, Error: "地址格式错误"})
			continue
		}
		results = append(results, m.joinOne(s, true))
	}
	return results
}

func (m *Manager) joinOne(addr string, report bool) JoinResult {
	res := JoinResult{Addr: addr}
	wasHub := false
	for _, h := range m.Hubs() {
		if h == addr {
			wasHub = true // 不早退：显式 join 必须真正确认连接与身份（防僵尸连接假阳性）
			break
		}
	}
	ctx, cancel := context.WithTimeout(m.deps.Ctx, JoinTimeout)
	defer cancel()

	// QUIC multiaddr 必须自带 /p2p/ 身份；HTTP 地址（host[:port]）先探测身份
	dialAddr := addr
	var httpNorm string
	if !strings.HasPrefix(addr, "/") {
		host, port, err := splitHostPortDefault(addr, 3456)
		if err != nil {
			res.Error = "地址格式错误"
			return res
		}
		pid, quicAddr, perr := m.probePeerID(ctx, host, port)
		if perr != nil {
			res.Error = "节点不可达: " + perr.Error()
			return res
		}
		if pid == m.selfID {
			res.Error = "不能连接自己"
			return res
		}
		httpNorm = fmt.Sprintf("%s:%d", host, port)
		addr = httpNorm // hub 记录 HTTP 地址（重启后自动重连用）
		dialAddr = quicAddr
		res.Addr = httpNorm
	}
	// 拨号退避重试：对端重启瞬间存在竞态（套接字重绑/旧连接清理），且本端
	// 宕机期间的自动拨号失败会触发 swarm 对端拨号退避（立即拒绝）。
	// 分多次退避重拨以跨过退避窗口——与 callRaw 的僵尸连接重试同理。
	pid, err := m.deps.Host.ConnectAddr(ctx, dialAddr)
	for retry, wait := 0, time.Second; err != nil && retry < 3; retry, wait = retry+1, wait*2 {
		if ctx.Err() != nil {
			ctx, cancel = context.WithTimeout(m.deps.Ctx, JoinTimeout)
			defer cancel()
		}
		time.Sleep(wait)
		pid, err = m.deps.Host.ConnectAddr(ctx, dialAddr)
	}
	if err != nil {
		res.Error = "节点不可达: " + err.Error()
		return res
	}
	if pid == m.selfID {
		m.deps.Host.Disconnect(pid)
		res.Error = "不能连接自己"
		return res
	}
	// TOFU：地址背后的身份变化即拒绝
	if old, ok := m.known[addr]; ok && old != "" && old != pid {
		m.deps.Host.Disconnect(pid)
		res.Error = "该地址的节点身份已变化（TOFU 保护），已拒绝"
		m.log.Warn("TOFU 身份漂移", "addr", addr, "old", shortID(old), "new", shortID(pid))
		return res
	}
	m.mu.Lock()
	delete(m.leftAt, pid) // 显式入网 = 用户明确要连接
	m.known[addr] = pid
	dup := false
	for _, h := range m.hubs {
		if h == addr {
			dup = true
		}
	}
	if !dup {
		m.hubs = append(m.hubs, addr)
	}
	m.mu.Unlock()

	// 僵尸连接恢复：本端显示已连接、但成员表没有该节点 = 对端很可能已重启
	// （连接只剩本端的僵尸）。强制断开重拨一次，触发双方重新宣告与收录。
	if wasHub && !m.IsOnline(pid) && m.deps.Host.Connected(pid) {
		m.deps.Host.Disconnect(pid)
		time.Sleep(200 * time.Millisecond)
		if pid2, err := m.deps.Host.ConnectAddr(ctx, dialAddr); err == nil {
			pid = pid2
		}
	}
	_ = m.publishAttestation()
	m.persist()
	res.OK = true
	if wasHub {
		res.Note = "已存在"
	}
	res.Node = pid
	if mem := m.member(pid); mem != nil && mem.Att != nil && mem.Att.Name != "" {
		res.Node = mem.Att.Name
	}
	return res
}

func (m *Manager) persist() {
	m.mu.RLock()
	m.deps.Cfg.Net.Hubs = append([]string{}, m.hubs...)
	m.deps.Cfg.Net.Known = map[string]string{}
	for k, v := range m.known {
		m.deps.Cfg.Net.Known[k] = v
	}
	m.mu.RUnlock()
	if m.deps.SaveCfg != nil {
		if err := m.deps.SaveCfg(); err != nil {
			m.log.Warn("配置保存失败", "err", err)
		}
	}
}

// splitHostPortDefault 解析 host[:port]，缺省端口用 def。
func splitHostPortDefault(s string, def int) (host string, port int, err error) {
	if strings.HasPrefix(s, "[") { // [::1]:3456
		i := strings.Index(s, "]")
		if i < 0 {
			return "", 0, fmt.Errorf("地址格式错误")
		}
		host = s[1:i]
		port = def
		if rest := s[i+1:]; strings.HasPrefix(rest, ":") {
			if p, e := strconv.Atoi(rest[1:]); e == nil && p > 0 && p < 65536 {
				port = p
			}
		}
		return host, port, nil
	}
	if i := strings.LastIndex(s, ":"); i > 0 {
		host = s[:i]
		if p, e := strconv.Atoi(s[i+1:]); e == nil && p > 0 && p < 65536 {
			return host, p, nil
		}
		return "", 0, fmt.Errorf("地址格式错误")
	}
	return s, def, nil
}

// probePeerID 通过对方 HTTP 端点的 /api/net/info 引导探测身份。
// 返回 PeerID 与可直接拨号的 QUIC multiaddr（用对方宣告的 QUIC 端口 + 探测所用主机地址）。
func (m *Manager) probePeerID(ctx context.Context, host string, port int) (pid, quicAddr string, err error) {
	u := fmt.Sprintf("http://%s/api/net/info", net_JoinHostPort(host, port))
	req, err := http_NewRequestWithContext(ctx, http_GET, u, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var info struct {
		App      string `json:"app"`
		ID       string `json:"id"`
		QuicPort int    `json:"quicPort"`
	}
	if err := json_NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", "", err
	}
	if info.App != "picnet" || info.ID == "" {
		return "", "", fmt.Errorf("对方不是 picnet 节点")
	}
	ip := host
	if netParseIP(host) == nil {
		ips, err := net_LookupHost(host)
		if err != nil || len(ips) == 0 {
			return "", "", fmt.Errorf("主机解析失败: %s", host)
		}
		ip = ips[0]
	}
	fam := "ip4"
	if strings.Contains(ip, ":") {
		fam = "ip6"
	}
	return info.ID, fmt.Sprintf("/%s/%s/udp/%d/quic-v1/p2p/%s", fam, ip, info.QuicPort, info.ID), nil
}

// RemoveHub 断开并移除单个 hub（lotus-server CLI 逐网络离开用）：
// 清 hubs/known 记录、广播下线通知并断开该连接、持久化。
// 返回是否确有此 hub。与 Leave 的全网退出不同，其余连接保持。
func (m *Manager) RemoveHub(addr string) bool {
	norm, ok := NormalizeAddr(addr)
	if ok {
		m.mu.RLock()
		for _, h := range m.hubs {
			if h == norm {
				addr = norm
			}
		}
		m.mu.RUnlock()
	}
	m.mu.Lock()
	found := false
	hubs := m.hubs[:0]
	for _, h := range m.hubs {
		if h == addr {
			found = true
			continue
		}
		hubs = append(hubs, h)
	}
	m.hubs = hubs
	pid := ""
	if found {
		pid = m.known[addr]
		delete(m.known, addr)
	}
	m.mu.Unlock()
	if !found {
		return false
	}
	if pid != "" {
		notice := m.deps.ID.SignLeaveNotice(m.selfID, time.Now().UnixMilli())
		if err := m.deps.Host.Publish(jsonMarshal(notice)); err != nil {
			m.log.Warn("下线通知广播失败", "err", err)
		}
		m.mu.Lock()
		m.leftAt[pid] = notice.TS
		m.mu.Unlock()
		m.deps.Host.Disconnect(pid)
		m.mu.Lock()
		delete(m.members, pid)
		m.mu.Unlock()
	}
	m.persist()
	m.eventsMembers()
	return true
}

// Leave 退出网络：先向全网广播签名下线通知（立即生效，无需等新鲜度窗口），
// 再清空 hubs、断开全部连接、清空成员表。
func (m *Manager) Leave() {
	notice := m.deps.ID.SignLeaveNotice(m.selfID, time.Now().UnixMilli())
	if err := m.deps.Host.Publish(jsonMarshal(notice)); err != nil {
		m.log.Warn("下线通知广播失败", "err", err)
	}
	time.Sleep(500 * time.Millisecond) // gossip 扩散宽限
	m.mu.Lock()
	m.leftAt[m.selfID] = notice.TS
	m.mu.Unlock()
	m.mu.Lock()
	m.hubs = nil
	m.atts = map[string]*crypto.Attestation{}
	m.members = map[string]*Member{}
	m.mu.Unlock()
	m.deps.Host.DisconnectAll()
	m.persist()
	m.eventsMembers()
}

// Settings 更新设置（部分字段）。
type SettingsUpdate struct {
	Name         *string
	Share        *bool
	CacheServe   *bool
	Concurrency  *int
	Advertise    *string
	LanDiscovery *bool
	ScanPorts    []int // 非 nil = 整体替换（自动去重、钳制 1-65535）
}

func (m *Manager) UpdateSettings(u SettingsUpdate) {
	m.mu.Lock()
	if u.Name != nil {
		n := strings.TrimSpace(*u.Name)
		if len(n) > 40 {
			n = n[:40]
		}
		if n != "" {
			m.settings.Name = n
		}
	}
	if u.Share != nil {
		m.settings.Share = *u.Share
	}
	if u.CacheServe != nil {
		m.settings.CacheServe = *u.CacheServe
	}
	if u.Concurrency != nil {
		m.settings.Concurrency = clampConcurrency(*u.Concurrency)
		m.deps.Queue.SetLimit(m.settings.Concurrency)
	}
	if u.Advertise != nil {
		m.settings.Advertise = strings.TrimSpace(*u.Advertise)
	}
	if u.LanDiscovery != nil {
		m.settings.LanDiscovery = *u.LanDiscovery
	}
	if u.ScanPorts != nil {
		seen := map[int]bool{core.ServerHTTPPort: true} // 默认端口恒在，去重
		ports := []int{}
		for _, p := range u.ScanPorts {
			if p >= 1 && p <= 65535 && !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
		m.settings.ScanPorts = ports
	}
	snap := m.settings
	m.mu.Unlock()

	m.deps.Cfg.Net.Name = snap.Name
	m.deps.Cfg.Net.Share = snap.Share
	m.deps.Cfg.Net.CacheServe = snap.CacheServe
	m.deps.Cfg.Net.Concurrency = snap.Concurrency
	m.deps.Cfg.Net.Advertise = snap.Advertise
	m.deps.Cfg.Net.LanDiscovery = snap.LanDiscovery
	m.deps.Cfg.Net.ScanPorts = append([]int(nil), snap.ScanPorts...)
	m.persist()
	_ = m.publishAttestation()
	m.eventsMembers()
}

func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > 16 {
		return 16
	}
	return n
}

/* ---------------- 只读快照（api 层用） ---------------- */

func (m *Manager) Snapshot() SnapshotData {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := SnapshotData{
		Share:        m.settings.Share,
		CacheServe:   m.settings.CacheServe,
		Concurrency:  m.settings.Concurrency,
		Joined:       false,
		Hub:          m.hubActive,
		LanDiscovery: m.settings.LanDiscovery,
		ScanPorts:    append([]int(nil), m.settings.ScanPorts...),
	}
	if s.Concurrency < 1 {
		s.Concurrency = 5
	}
	for _, addr := range m.hubs {
		st := "backoff"
		pid := m.known[addr]
		if pid != "" && m.deps.Host.Connected(pid) {
			st = "connected"
			s.Joined = true
		}
		s.Uplinks = append(s.Uplinks, UplinkStatus{Addr: addr, Status: st})
	}
	for pid, mem := range m.members {
		if pid == m.selfID {
			continue
		}
		name := ""
		if mem.Att != nil {
			name = mem.Att.Name
		}
		s.Members = append(s.Members, MemberInfo{
			ID: pid, Name: name,
			Addr:   attAddr(mem.Att),
			Direct: m.deps.Host.ConnTransport(pid) == "direct",
		})
	}
	sort.Slice(s.Members, func(i, j int) bool { return s.Members[i].ID < s.Members[j].ID })
	return s
}

func attAddr(a *crypto.Attestation) string {
	if a == nil {
		return ""
	}
	return a.Addr
}

// SnapshotData /api/net/state 的数据部分（api 层补 me/cached/hostLoopback）。
type SnapshotData struct {
	Share        bool
	CacheServe   bool
	Concurrency  int
	Uplinks      []UplinkStatus
	Members      []MemberInfo
	Hub          bool
	Joined       bool
	LanDiscovery bool
	ScanPorts    []int
}

type UplinkStatus struct {
	Addr   string
	Status string
}

type MemberInfo struct {
	ID     string
	Name   string
	Addr   string
	Direct bool
}

func (m *Manager) Hubs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string{}, m.hubs...)
}

func (m *Manager) KnownByAddr(addr string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.known[addr]
}

func (m *Manager) member(pid string) *Member {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.members[pid]
}

// AttestationOf 返回某节点的最近有效宣告（无则 nil）。
func (m *Manager) AttestationOf(pid string) *crypto.Attestation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mem, ok := m.members[pid]; ok {
		return mem.Att
	}
	return nil
}

// IsOnline 目标是否在线 = 成员表内（成员表由维护循环按宣告新鲜度收敛；
// 僵尸连接不影响本判定——宣告停更即视为下线，与旧版 members.get 语义一致）。
func (m *Manager) IsOnline(pid string) bool {
	m.mu.RLock()
	_, ok := m.members[pid]
	m.mu.RUnlock()
	return ok
}

// Discovered mDNS 发现列表（合并宣告缓存中的名字）。
func (m *Manager) Discovered() []DiscoverOut {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []DiscoverOut{}
	for id, di := range m.discover {
		name := ""
		if mem, ok := m.members[id]; ok && mem.Att != nil {
			name = mem.Att.Name
		}
		known := m.deps.Host.Connected(id)
		if !known {
			for _, h := range m.hubs {
				if m.known[h] == id {
					known = true
					break
				}
			}
		}
		addr := ""
		if len(di.Addrs) > 0 {
			addr = di.Addrs[0]
		}
		out = append(out, DiscoverOut{ID: id, Name: name, Addr: addr, TS: di.LastSeen.UnixMilli(), Known: known})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out
}

type DiscoverOut struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Addr  string `json:"addr"`
	TS    int64  `json:"ts"`
	Known bool   `json:"known"`
}

// RpathExt 取远程路径扩展名（小写、含点）。
func RpathExt(rpath string) string {
	return strings.ToLower(filepath.Ext(rpath))
}

// Package stats：lotus-server 的运行观测（查询面的数据源）。
// 访问记录（谁在什么时间访问了什么、结果与耗时）、按对端流量计数、服务时间。
// 全内存 + 互斥锁：块级计数为每 512KB 一次的量级，锁开销可忽略；访问日志为环形。
package stats

import (
	"sync"
	"time"
)

// MaxAccessLog 访问日志容量（超出淘汰最旧）。
const MaxAccessLog = 1024

// AccessKind 访问类型（node 层埋点用）：
// image=本机出图；cache=链路缓存代答；relay=转发；relay-fail=转发失败；
// roots/browse/times/stat=简单协议；download=本机向远端取图完成。
const (
	KindImage     = "image"
	KindCache     = "cache"
	KindRelay     = "relay"
	KindRelayFail = "relay-fail"
	KindDownload  = "download"
)

// AccessEvent 埋点事件（node 层构造，Collector 补时间戳）。
type AccessEvent struct {
	Kind  string // AccessKind 或协议名 roots/browse/times/stat
	Peer  string // 对端 PeerID（完整）
	Path  string // 文件/目录路径（roots 为空）
	Bytes int64  // 本次业务字节数（明文或密文，按场景）
	DurMS int64  // 服务耗时（毫秒）
	OK    bool
	Extra string // 补充信息（转发目标 / 失败原因）
}

// AccessEntry 访问记录（查询输出）。
type AccessEntry struct {
	TS    int64  `json:"ts"`              // UnixMilli
	Kind  string `json:"kind"`            // image/cache/relay/relay-fail/download/roots/browse/times/stat
	Peer  string `json:"peer"`            // 对端 PeerID（完整）
	Path  string `json:"path,omitempty"`  // 文件/目录路径
	Bytes int64  `json:"bytes,omitempty"` // 业务字节数
	DurMS int64  `json:"durMs,omitempty"` // 服务耗时（毫秒）
	OK    bool   `json:"ok"`
	Extra string `json:"extra,omitempty"` // 转发目标 / 失败原因
}

// PeerTraffic 单对端流量（字节；app 层五协议收发合计）。
type PeerTraffic struct {
	Peer string `json:"peer"`
	In   int64  `json:"in"`
	Out  int64  `json:"out"`
}

// TrafficReport 流量快照（Peers 按收发总量降序）。
type TrafficReport struct {
	Peers   []PeerTraffic `json:"peers"`
	TotalIn int64         `json:"totalIn"`
	TotalOut int64        `json:"totalOut"`
}

// KindCount 单类型访问计数。
type KindCount struct {
	Kind   string `json:"kind"`
	Count  int64  `json:"count"`
	OK     int64  `json:"ok"`
	Fail   int64  `json:"fail"`
	Bytes  int64  `json:"bytes"`
	DurMS  int64  `json:"durMs"` // 累计耗时（均值 = DurMS/Count）
}

// StatusReport 服务时间与请求概览（status 查询输出的一部分）。
type StatusReport struct {
	StartedAt int64                `json:"startedAt"` // UnixMilli
	UptimeSec int64                `json:"uptimeSec"`
	Requests  int64                `json:"requests"` // 全部访问事件数
	Kinds     []KindCount          `json:"kinds"`
	Traffic   TrafficReport        `json:"traffic"`
}

type collector struct {
	mu        sync.Mutex
	startedAt time.Time
	access    []AccessEntry
	kinds     map[string]*KindCount
	peers     map[string]*PeerTraffic
	total     TrafficReport
}

// Collector 观测采集器（并发安全；nil 指针安全由调用方保证）。
type Collector struct {
	c collector
}

func New() *Collector {
	return &Collector{c: collector{
		startedAt: time.Now(),
		kinds:     map[string]*KindCount{},
		peers:     map[string]*PeerTraffic{},
	}}
}

// StartedAt 服务启动时刻。
func (s *Collector) StartedAt() time.Time { return s.c.startedAt }

// Uptime 已运行时长。
func (s *Collector) Uptime() time.Duration { return time.Since(s.c.startedAt) }

// AddTraffic 累计对端流量（in=收自对端，否则发给对端）。
func (s *Collector) AddTraffic(peer string, in bool, n int64) {
	if s == nil || n <= 0 {
		return
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	pt := s.c.peers[peer]
	if pt == nil {
		pt = &PeerTraffic{Peer: peer}
		s.c.peers[peer] = pt
	}
	if in {
		pt.In += n
		s.c.total.TotalIn += n
	} else {
		pt.Out += n
		s.c.total.TotalOut += n
	}
}

// LogAccess 记录一次访问（补时间戳入环形日志 + 类型计数）。
func (s *Collector) LogAccess(e AccessEvent) {
	if s == nil {
		return
	}
	entry := AccessEntry{
		TS: time.Now().UnixMilli(), Kind: e.Kind, Peer: e.Peer, Path: e.Path,
		Bytes: e.Bytes, DurMS: e.DurMS, OK: e.OK, Extra: e.Extra,
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.access = append(s.c.access, entry)
	if len(s.c.access) > MaxAccessLog {
		s.c.access = s.c.access[len(s.c.access)-MaxAccessLog:]
	}
	k := s.c.kinds[e.Kind]
	if k == nil {
		k = &KindCount{Kind: e.Kind}
		s.c.kinds[e.Kind] = k
	}
	k.Count++
	if e.OK {
		k.OK++
	} else {
		k.Fail++
	}
	k.Bytes += e.Bytes
	k.DurMS += e.DurMS
}

// AccessSnapshot 最近 limit 条访问记录（新→旧；limit<=0 取全部）。
func (s *Collector) AccessSnapshot(limit int) []AccessEntry {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	n := len(s.c.access)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]AccessEntry, n)
	for i := 0; i < n; i++ {
		out[i] = s.c.access[len(s.c.access)-1-i]
	}
	return out
}

// TrafficSnapshot 流量快照（按对端总量降序）。
func (s *Collector) TrafficSnapshot() TrafficReport {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	out := TrafficReport{
		Peers:    make([]PeerTraffic, 0, len(s.c.peers)),
		TotalIn:  s.c.total.TotalIn,
		TotalOut: s.c.total.TotalOut,
	}
	for _, pt := range s.c.peers {
		out.Peers = append(out.Peers, *pt)
	}
	sortPeers(out.Peers)
	return out
}

// StatusSnapshot 服务时间与访问概览。
func (s *Collector) StatusSnapshot() StatusReport {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	out := StatusReport{
		StartedAt: s.c.startedAt.UnixMilli(),
		UptimeSec: int64(time.Since(s.c.startedAt) / time.Second),
		Kinds:     make([]KindCount, 0, len(s.c.kinds)),
	}
	for _, k := range s.c.kinds {
		out.Requests += k.Count
		out.Kinds = append(out.Kinds, *k)
	}
	sortKinds(out.Kinds)
	out.Traffic = TrafficReport{
		Peers:    make([]PeerTraffic, 0, len(s.c.peers)),
		TotalIn:  s.c.total.TotalIn,
		TotalOut: s.c.total.TotalOut,
	}
	for _, pt := range s.c.peers {
		out.Traffic.Peers = append(out.Traffic.Peers, *pt)
	}
	sortPeers(out.Traffic.Peers)
	return out
}

func sortPeers(ps []PeerTraffic) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0; j-- {
			a, b := ps[j-1], ps[j]
			if a.In+a.Out >= b.In+b.Out {
				break
			}
			ps[j-1], ps[j] = ps[j], ps[j-1]
		}
	}
}

func sortKinds(ks []KindCount) {
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0; j-- {
			if ks[j-1].Count >= ks[j].Count {
				break
			}
			ks[j-1], ks[j] = ks[j], ks[j-1]
		}
	}
}

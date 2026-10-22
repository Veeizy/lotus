package node

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"lotus/internal/core"
)

/* ---------------- 局域网自动发现并入网 ----------------
 * 候选两个来源：
 *  1. mDNS 发现列表（已在对端同机时也适用；Addr 为可拨号 multiaddr）；
 *  2. 子网端口探测：对本机各 IPv4 网段展开 /24 主机，探测默认端口
 *     （ServerHTTPPort，恒在）与用户附加端口上的 /api/net/info。
 * 候选过滤：排除自身（PeerID 相同）、排除本机自身 IP、排除已加入节点
 * （known 表任一 hub 的 PeerID）。命中即按普通流程 joinOne（TOFU/持久化）。
 * 触发：启动后 10s 一轮 + 维护循环每 60s 一轮 + /api/net/lan/discover 手动。
 * 开关：net.lanDiscovery（仅控制局域网部分；本机服务连接始终开启）。
 * NoAutoJoin（测试开关）关闭全部自动入网。 */

// LanCandidate 局域网发现的一个候选节点。
type LanCandidate struct {
	ID   string // PeerID
	Addr string // 可拨号地址（HTTP 形式 ip:port 或 multiaddr）
	Name string // 对端宣告名（可为空）
	Via  string // 来源：mdns / scan
}

// LanJoinResult 单个候选的入网结果（手动发现接口返回）。
type LanJoinResult struct {
	LanCandidate
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var lanScanning sync.Mutex // 防止扫描重入（周期与手动并发）

// lanDiscoveryCycle 一轮发现并入网（异步安全：重入直接跳过）。
func (m *Manager) lanDiscoveryCycle() {
	if !lanScanning.TryLock() {
		return
	}
	defer lanScanning.Unlock()
	ctx, cancel := context.WithTimeout(m.deps.Ctx, 20*time.Second)
	defer cancel()
	results := m.DiscoverAndJoinLAN(ctx)
	for _, r := range results {
		if r.OK {
			m.log.Info("局域网发现并入网", "via", r.Via, "addr", r.Addr, "node", shortID(r.ID))
		}
	}
}

// MaxLanJoinsPerCycle 单轮发现最多自动入网的节点数（防拥挤局域网瞬时大爆连）。
const MaxLanJoinsPerCycle = 8

// DiscoverAndJoinLAN 扫描局域网并自动入网（手动接口同此路径），返回逐候选结果。
func (m *Manager) DiscoverAndJoinLAN(ctx context.Context) []LanJoinResult {
	return m.joinLanCandidates(ctx, m.lanCandidates(ctx, m.settings.ScanPorts))
}

// joinLanCandidates 逐候选入网（最多 MaxLanJoinsPerCycle 个/轮）。
func (m *Manager) joinLanCandidates(ctx context.Context, cands []LanCandidate) []LanJoinResult {
	out := make([]LanJoinResult, 0, len(cands))
	joins := 0
	for _, c := range cands {
		if joins >= MaxLanJoinsPerCycle {
			out = append(out, LanJoinResult{
				LanCandidate: LanCandidate{ID: c.ID, Addr: c.Addr, Name: c.Name, Via: c.Via},
				OK:           false, Error: "本轮入网数已达上限，待下轮",
			})
			continue
		}
		res := m.joinOne(c.Addr, false)
		joins++
		out = append(out, LanJoinResult{
			LanCandidate: LanCandidate{ID: res.Node, Addr: c.Addr, Name: c.Name, Via: c.Via},
			OK:           res.OK,
			Error:        res.Error,
		})
		m.eventsMembers()
	}
	return out
}

// lanCandidates 汇总 mDNS 与子网扫描候选（去重、过滤自身与已加入节点）。
func (m *Manager) lanCandidates(ctx context.Context, extraPorts []int) []LanCandidate {
	m.mu.RLock()
	knownPIDs := map[string]bool{}
	for _, pid := range m.known {
		knownPIDs[pid] = true
	}
	m.mu.RUnlock()
	knownPIDs[m.selfID] = true

	var cands []LanCandidate
	seen := map[string]bool{} // 候选去重：PeerID 优先，其次地址

	add := func(c LanCandidate) bool {
		if c.ID == "" || knownPIDs[c.ID] || seen[c.ID] {
			return false
		}
		seen[c.ID] = true
		cands = append(cands, c)
		return true
	}

	// 1) mDNS 发现列表
	for _, d := range m.Discovered() {
		if knownPIDs[d.ID] || seen[d.ID] {
			continue
		}
		if d.Addr == "" || !strings.HasPrefix(d.Addr, "/") || !strings.Contains(d.Addr, "/quic-v1") {
			continue
		}
		add(LanCandidate{ID: d.ID, Addr: d.Addr, Name: d.Name, Via: "mdns"})
	}

	// 2) 子网端口探测（排除自身 IP）
	ports := append([]int{core.ServerHTTPPort}, extraPorts...)
	for _, c := range m.scanSubnet(ctx, ports, knownPIDs) {
		add(c)
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
	return cands
}

// scanSubnet 对本机各 IPv4 /24 网段探测 picnet 节点（HTTP /api/net/info）。
func (m *Manager) scanSubnet(ctx context.Context, ports []int, knownPIDs map[string]bool) []LanCandidate {
	own, targets := lanScanTargets()
	if len(targets) == 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) > 8*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
	}

	type task struct {
		ip   string
		port int
	}
	var tasks []task
	seen := map[string]bool{}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			continue
		}
		for _, ip := range targets {
			key := ip + ":" + fmt.Sprint(port)
			if !seen[key] {
				seen[key] = true
				tasks = append(tasks, task{ip: ip, port: port})
			}
		}
	}

	const workers = 64
	tasksCh := make(chan task)
	outCh := make(chan LanCandidate, len(tasks))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasksCh {
				select {
				case <-ctx.Done():
					return
				default:
				}
				pctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
				pid, _, err := m.probePeerID(pctx, t.ip, t.port)
				cancel()
				if err != nil || pid == "" || pid == m.selfID || knownPIDs[pid] {
					continue
				}
				outCh <- LanCandidate{ID: pid, Addr: net_JoinHostPort(t.ip, t.port), Via: "scan"}
			}
		}()
	}
	for _, t := range tasks {
		tasksCh <- t
	}
	close(tasksCh)
	wg.Wait()
	close(outCh)

	cands := make([]LanCandidate, 0, len(outCh))
	seenPID := map[string]bool{}
	for c := range outCh {
		if seenPID[c.ID] {
			continue
		}
		seenPID[c.ID] = true
		cands = append(cands, c)
	}
	_ = own
	return cands
}

// lanScanTargets 收集本机非回环 IPv4 所在 /24 网段的全部主机地址（排除自身 IP）。
func lanScanTargets() (own []string, targets []string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}
	ownSet := map[string]bool{}
	var bases []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			ownSet[ip.String()] = true
			bases = append(bases, ip.Mask(net.CIDRMask(24, 32)))
		}
	}
	if len(bases) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, base := range bases {
		for i := 1; i < 255; i++ {
			c := net.IP(append([]byte(nil), base...))
			c[3] = byte(i)
			cs := c.String()
			if !ownSet[cs] && !seen[cs] {
				seen[cs] = true
				targets = append(targets, cs)
			}
		}
	}
	keys := make([]string, 0, len(ownSet))
	for k := range ownSet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, targets
}

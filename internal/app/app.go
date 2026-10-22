// Package app：组装 core/p2p/node（组合根）。api 层依赖本包暴露的引用。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"lotus/internal/core"
	"lotus/internal/crypto"
	"lotus/internal/node"
	"lotus/internal/p2p"
	"lotus/internal/stats"
)

// Options 启动参数（命令行 > 配置文件的覆盖已由 main 完成后传入）。
type Options struct {
	Cfg       *core.Config
	CfgPath   string // config.json 绝对路径（其所在目录为 appDir）
	StateFile string // 身份状态文件名，空 = net-state.json（lotus-server 用 server-state.json，
	// 与同目录的客户端区分——同机双进程必须各自持有独立身份）
	NoMDNS      bool
	DelayMS     int // 测试：节点流 I/O 模拟延迟
	FreshSec    int // 测试：在线新鲜度窗口
	NoAutoJoin  bool
	MaxMsgDelay bool // 预留
	FixedRoots  bool // 移动端：图库根与缓存目录使用应用默认位置，禁止编辑
}

type App struct {
	Cfg       *core.Config
	CfgPath   string
	StatePath string
	AppDir    string

	libP       atomic.Pointer[core.Library]
	cacheP     atomic.Pointer[core.Cache]
	svcRoots   atomic.Pointer[[]string] // 本机服务的根（绝对路径；并入本地浏览图库）
	libVer     atomic.Int64             // 图库版本（每次重建 +1；前端据此自动刷新根视图）
	Queue      *core.Queue
	ID         *crypto.Identity
	PeerID     string
	Host       *p2p.Host
	Net        *node.Manager
	Events     *node.EventBus
	Stats      *stats.Collector // 运行观测（访问/流量/服务时间）
	NetReady   bool             // false = 加密自检失败，网络功能禁用
	FixedRoots bool             // 移动端：根与缓存目录固定（api 层拒绝编辑）

	log         *slog.Logger
	Warns       []string
	cacheDirAbs string
}

// New 构建并启动全部子系统。netDisabled（加密自检失败）时仅提供本地服务。
func New(ctx context.Context, opt Options, log *slog.Logger) (*App, error) {
	netOK := crypto.SelfTest() == nil
	if !netOK {
		log.Error("加密模块自检失败，网络功能不可用")
		// 与旧实现一致：自检失败禁网不禁站
	}
	a := &App{Cfg: opt.Cfg, CfgPath: opt.CfgPath, FixedRoots: opt.FixedRoots, log: log}
	a.AppDir = filepath.Dir(opt.CfgPath)
	stateFile := opt.StateFile
	if stateFile == "" {
		stateFile = "net-state.json"
	}
	a.StatePath = filepath.Join(a.AppDir, stateFile)

	a.Warns = a.rebuildLibrary()

	// 身份（v1 兼容，写回 v2；不可写目录降级内存态）
	secret, name, existed, err := core.LoadState(a.StatePath)
	if err != nil {
		log.Warn("状态文件读取失败，将生成新身份", "err", err)
		existed = false
	}
	if !existed || len(secret) != 16 {
		secret, err = crypto.RandomSecret16()
		if err != nil {
			return nil, err
		}
		name = ""
	}
	if serr := core.SaveState(a.StatePath, secret, name); serr != nil {
		// 移动端等场景写失败 = 每次重启身份都变：必须可见（Warns → StartResult → UI）
		a.Warns = append(a.Warns, "状态文件写入失败（身份每次启动都会变化）: "+serr.Error())
		log.Error("状态文件写入失败", "path", a.StatePath, "err", serr.Error())
	}
	id, err := crypto.DeriveIdentity(secret)
	if err != nil {
		return nil, err
	}
	a.ID = id
	if name != "" && opt.Cfg.Net.Name == "" {
		opt.Cfg.Net.Name = name
	}
	peerID, err := p2p.PeerIDFromSeed(id.SeedEd)
	if err != nil {
		return nil, err
	}
	a.PeerID = peerID

	// 缓存与队列
	cacheDir := opt.Cfg.Net.CacheDir
	if !filepath.IsAbs(cacheDir) {
		cacheDir = filepath.Join(a.AppDir, cacheDir)
	}
	cacheC, err := core.OpenCache(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("缓存目录打开失败: %w", err)
	}
	a.cacheP.Store(cacheC)
	a.cacheDirAbs = cacheDir
	a.Queue = core.NewQueue(opt.Cfg.Net.Concurrency)
	a.Events = node.NewEventBus()
	a.Stats = stats.New()

	// p2p + 网络节点（自检失败则整体跳过）
	if netOK {
		hubAddr := ""
		if opt.Cfg.Net.Advertise != "" {
			hubAddr = opt.Cfg.Net.Advertise
		}
		host, err := p2p.NewHost(ctx, p2p.Config{
			SeedEd:       id.SeedEd,
			ListenPort:   opt.Cfg.Net.Port,
			HubMode:      true, // 所有节点皆可中继（无层级拓扑，见 PROTOCOL §8.9）
			StaticRelays: nil,
			MDNS:         !opt.NoMDNS,
			Advertise:    hubAddr,
			Logger:       log,
		})
		if err != nil {
			return nil, fmt.Errorf("网络启动失败: %w", err)
		}
		a.Host = host
		a.NetReady = true
		net, err := node.NewManager(node.Deps{
			Ctx:        ctx,
			Lib:        a.Lib(),
			Cache:      a.Cache(),
			Queue:      a.Queue,
			Host:       host,
			ID:         id,
			StatePath:  a.StatePath,
			Cfg:        opt.Cfg,
			SaveCfg:    func() error { return core.SaveConfig(opt.CfgPath, opt.Cfg) },
			Logger:     log,
			Events:     a.Events,
			Stats:      a.Stats,
			NoAutoJoin: opt.NoAutoJoin, // 测试开关：关闭全部自动入网
			DelayMS:    opt.DelayMS,
			FreshSec:   opt.FreshSec,
		})
		if err != nil {
			host.Close()
			return nil, err
		}
		a.Net = net
		// 本机服务的根并入本地图库（直接读盘；不经过网络、不缓存）
		go a.serviceRootsLoop(ctx)
	}
	return a, nil
}

// Lib 当前图库（可在运行时整体替换）。
func (a *App) Lib() *core.Library { return a.libP.Load() }

// Cache 当前网络缓存索引。
func (a *App) Cache() *core.Cache { return a.cacheP.Load() }

// CacheDirAbs 缓存目录绝对路径。
func (a *App) CacheDirAbs() string { return a.cacheDirAbs }

// rebuildLibrary 重建图库（双份语义）：
//   - 对外共享（node 五协议服务）= 仅自身根——本机服务的根不借客户端身份外泄；
//   - 本地浏览（api）= 自身根 + 本机服务根（Service 标记，前端 🖥 图标区分）。
func (a *App) rebuildLibrary() (warns []string) {
	own, warns := core.NewLibrary(a.Cfg.Paths)
	if a.Net != nil {
		a.Net.SetLibrary(own)
	}
	merged := own.Clone()
	if svc := a.serviceRoots(); len(svc) > 0 {
		merged.AddServiceRoots(svc)
	}
	a.libP.Store(merged)
	a.libVer.Add(1)
	return warns
}

// LibVersion 当前图库版本号（rebuildLibrary 每次递增）。
func (a *App) LibVersion() int64 { return a.libVer.Load() }

func (a *App) serviceRoots() []string {
	if p := a.svcRoots.Load(); p != nil {
		return *p
	}
	return nil
}

// setServiceRoots 更新本机服务根（有变化才重建图库）。
func (a *App) setServiceRoots(paths []string) {
	old := a.serviceRoots()
	if samePaths(old, paths) {
		return
	}
	a.svcRoots.Store(&paths)
	for _, w := range a.rebuildLibrary() {
		a.log.Warn(w)
	}
	a.log.Info("本机服务根已同步", "roots", len(paths))
}

// serviceRootsLoop 周期拉取本机服务的文件夹列表（只取列表，不取图片）：
// 服务的根并入本地图库后，浏览与出图全部走本地磁盘。
func (a *App) serviceRootsLoop(ctx context.Context) {
	refresh := func() {
		if a.Net == nil {
			return
		}
		pid := a.Net.LocalServicePeer()
		if pid == "" {
			a.setServiceRoots(nil)
			return
		}
		rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		head, err := a.Net.RequestRoots(rctx, pid)
		if err != nil || !head.OK {
			return // 保留上次结果
		}
		var data struct {
			Roots []struct {
				Path string `json:"path"`
			} `json:"roots"`
		}
		if err := json.Unmarshal(head.Data, &data); err != nil {
			return
		}
		paths := make([]string, 0, len(data.Roots))
		for _, r := range data.Roots {
			paths = append(paths, r.Path)
		}
		a.setServiceRoots(paths)
	}
	time.Sleep(2 * time.Second) // 等首次 join / hub 重连
	refresh()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

func samePaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, p := range a {
		set[strings.ToLower(p)] = true
	}
	for _, p := range b {
		if !set[strings.ToLower(p)] {
			return false
		}
	}
	return true
}

// SetLibrary 用新的根路径集合整体替换图库（配置文件夹热切换）。
func (a *App) SetLibrary(paths []string) (warns []string) {
	a.Cfg.Paths = paths
	return a.rebuildLibrary()
}

// SetCacheDir 切换网络缓存目录：新索引立即生效，旧索引延后落盘关闭。
func (a *App) SetCacheDir(dir string) error {
	c, err := core.OpenCache(dir)
	if err != nil {
		return err
	}
	old := a.cacheP.Swap(c)
	oldDir := a.cacheDirAbs
	a.cacheDirAbs = dir
	a.Cfg.Net.CacheDir = dir
	if a.Net != nil {
		a.Net.SetCache(c)
	}
	if old != nil {
		time.AfterFunc(3*time.Second, func() { old.Close() })
	}
	_ = oldDir
	return nil
}

// Close 优雅停机：落盘索引、关停网络。
func (a *App) Close() {
	if c := a.cacheP.Load(); c != nil {
		c.Close()
	}
	if a.Host != nil {
		a.Host.Close()
	}
}

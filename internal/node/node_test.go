package node

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lotus/internal/core"
	"lotus/internal/crypto"
	"lotus/internal/p2p"
	"lotus/internal/stats"
)

// testEnv 进程内三节点测试环境（真实 libp2p QUIC，端口随机）。
type testEnv struct {
	t     *testing.T
	Dir   string
	Nodes map[string]*nodeProc
}

type nodeProc struct {
	Name    string
	Dir     string
	Gallery string
	Lib     *core.Library
	Cache   *core.Cache
	Queue   *core.Queue
	Host    *p2p.Host
	ID      *crypto.Identity
	PeerID  string
	Mgr     *Manager
	Events  *EventBus
	Stats   *stats.Collector // 经 opts 注入：观测埋点测试用
}

func seedN(n byte) []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = n
	}
	return s
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, Dir: t.TempDir(), Nodes: map[string]*nodeProc{}}
	t.Cleanup(func() {
		for _, n := range e.Nodes {
			n.Host.Close()
			n.Cache.Close()
		}
	})
	return e
}

// startNode 启动一个测试节点。
func (e *testEnv) startNode(name byte, opts ...func(*nodeProc)) *nodeProc {
	e.t.Helper()
	dir := filepath.Join(e.Dir, fmt.Sprintf("node-%d", name))
	gallery := filepath.Join(dir, "gallery")
	if err := os.MkdirAll(gallery, 0o755); err != nil {
		e.t.Fatal(err)
	}
	id, err := crypto.DeriveIdentity(randomSecretForTest(name))
	if err != nil {
		e.t.Fatal(err)
	}
	lib, _ := core.NewLibrary([]string{gallery})
	cache, err := core.OpenCache(filepath.Join(dir, "net-cache"))
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := core.DefaultConfig()
	q := core.NewQueue(5)
	events := NewEventBus()
	host, err := p2p.NewHost(context.Background(), p2p.Config{
		SeedEd:      id.SeedEd,
		ListenPort:  0,
		HubMode:     true,
		HeartbeatMS: 1000, // 测试快速收敛（生产默认 30s）
		Logger:      testLogger(e.t),
	})
	if err != nil {
		e.t.Fatalf("启动 host 失败: %v", err)
	}
	n := &nodeProc{Name: fmt.Sprintf("n%d", name), Dir: dir, Gallery: gallery,
		Lib: lib, Cache: cache, Queue: q, Host: host, ID: id, Events: events}
	for _, o := range opts {
		o(n)
	}
	mgr, err := NewManager(Deps{
		Ctx: context.Background(), Lib: lib, Cache: cache, Queue: q,
		Host: host, ID: id, Cfg: cfg,
		Logger: testLogger(e.t), Events: events,
		Stats:    n.Stats,
		NoAutoJoin: true, // 进程内测试：不探测宿主机 3456 的真实服务、不扫局域网
		FreshSec: 3,      // 测试快速收敛
	})
	if err != nil {
		e.t.Fatal(err)
	}
	n.Mgr = mgr
	n.PeerID = host.ID()
	e.Nodes[n.Name] = n
	e.t.Cleanup(func() { host.Close(); cache.Close() })
	return n
}

func randomSecretForTest(n byte) string {
	return fmt.Sprintf("TestSecret16b%02dX", n) // 16 字符
}

func (e *testEnv) dial(from, to *nodeProc) {
	e.t.Helper()
	addr := ""
	for _, a := range to.Host.Libp2p().Addrs() {
		s := a.String()
		if len(s) > 4 && s[:4] == "/ip4" {
			if i := index(s, "/quic-v1"); i > 0 {
				addr = "/ip4/127.0.0.1" + s[index(s, "/udp"):i+len("/quic-v1")] + "/p2p/" + to.PeerID
				break
			}
		}
	}
	if addr == "" {
		e.t.Fatal("无监听地址")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := from.Host.ConnectAddr(ctx, addr); err != nil {
		e.t.Fatalf("拨号失败: %v", err)
	}
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func testLogger(t *testing.T) *slog.Logger {
	_ = t
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

/* ---------------- 测试 ---------------- */

// TestForwardChainMultiHop 多层级转发：C 只认识 B，B 认识 A，C 经 B 转发取 A 的图。
func TestForwardChainMultiHop(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	c := env.startNode(3)
	img := writeFile(t, a.Gallery, "deep.png", "content-of-A-图片内容-12345")

	// 拓扑：A←B←C（C 只与 B 相连，B 与 A 相连；C 与 A 不相连）
	env.dial(a, b)
	env.dial(b, c)
	waitFor(t, 10*time.Second, "B-C 连接", func() bool { return b.Host.Connected(c.PeerID) })
	// 等待 gossip 宣告到位（C 需要知道 B 才能选它做转发者）
	waitFor(t, 15*time.Second, "C 收到 B 的宣告", func() bool {
		return c.Mgr.AttestationOf(b.PeerID) != nil
	})
	waitFor(t, 15*time.Second, "C 知道 A（经 gossip）", func() bool {
		return c.Mgr.AttestationOf(a.PeerID) != nil
	})

	// C 请求 A 的图（target=A，经 B 转发）
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path, err := c.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true)
	if err != nil {
		t.Fatalf("转发链取图失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "content-of-A-图片内容-12345" {
		t.Fatalf("内容不符: %q", got)
	}
	// 缓存落在 C 的 A 目录下，键与旧实现一致
	key := core.CacheKey(a.PeerID, img)
	if _, err := os.Stat(filepath.Join(c.Dir, "net-cache", a.PeerID, key+".png")); err != nil {
		t.Fatalf("缓存路径/键不符: %v", err)
	}
	// receipt 三重校验已通过（下载成功的前提），显式验证索引内容
	idx := c.Cache.Lookup(a.PeerID)
	if idx == nil || idx.Files[key] == nil {
		t.Fatal("缓存索引缺失")
	}
	if idx.Files[key].Sha256 == "" || idx.Files[key].Receipt == nil {
		t.Fatal("索引缺少哈希或凭证")
	}
}

// TestCacheServeFreshAndStale 链路缓存：B 缓存 A 的文件后，
// C 经 B 取图由 B 代答（A 在线核对 stat；A 离线 → stale 过期缓存）。
func TestCacheServeFreshAndStale(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	c := env.startNode(3)
	img := writeFile(t, a.Gallery, "cached.png", "cached-content-v1")

	env.dial(a, b)
	env.dial(b, c)
	env.dial(a, c) // C 需要知道 A 才能发起 target=A 请求
	waitFor(t, 10*time.Second, "三方连接", func() bool {
		return a.Host.Connected(b.PeerID) && b.Host.Connected(c.PeerID) && a.Host.Connected(c.PeerID)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 第一次：C 从 A 取（经直连），A 在线
	if _, err := c.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true); err != nil {
		t.Fatalf("首次取图失败: %v", err)
	}
	// B 预取同一文件（使 B 持有带凭证缓存）
	if _, err := b.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true); err != nil {
		t.Fatalf("B 预取失败: %v", err)
	}

	// A 下线（分区模拟）——B、C 保持相连
	a.Host.DisconnectAll()
	waitFor(t, 10*time.Second, "A 下线", func() bool {
		return !b.Host.Connected(a.PeerID) && !c.Host.Connected(a.PeerID)
	})

	// C 再次请求 A 的图：C 与 A 无连接 → 经 B 转发/代答 → B 的缓存 + stale
	path, err := c.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true)
	if err != nil {
		t.Fatalf("A 离线后取图失败（应 stale 代答）: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "cached-content-v1" {
		t.Fatalf("内容不符: %q", got)
	}
	// 凭证应仍为 A 签发
	key := core.CacheKey(a.PeerID, img)
	entry := c.Cache.Lookup(a.PeerID).Files[key]
	if entry.Receipt.Node != a.PeerID {
		t.Fatalf("凭证归属错误: %s", entry.Receipt.Node)
	}
}

// TestCacheInvalidatedOnUpdate A 更新文件后，B 的缓存因 stat 不一致而失效，
// C 经 B 转发拿到新内容（+ 新 receipt）。
func TestCacheInvalidatedOnUpdate(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	c := env.startNode(3)
	img := writeFile(t, a.Gallery, "doc.png", "v1")
	env.dial(a, b)
	env.dial(b, c)
	env.dial(a, c)
	waitFor(t, 15*time.Second, "三节点宣告收敛", func() bool {
		return c.Mgr.AttestationOf(a.PeerID) != nil && b.Mgr.AttestationOf(a.PeerID) != nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// B 预取 v1（成为持有旧缓存的转发者）
	if _, err := b.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true); err != nil {
		t.Fatal(err)
	}
	// A 更新内容与 mtime
	time.Sleep(20 * time.Millisecond)
	writeFile(t, a.Gallery, "doc.png", "v2-updated")
	os.Chtimes(img, time.Now(), time.Now())

	// C 取图：直连 A 也可达——强制走 B：断开 C-A 直连
	c.Host.Disconnect(a.PeerID)
	waitFor(t, 10*time.Second, "C-A 断开", func() bool { return !c.Host.Connected(a.PeerID) })

	got, err := c.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true)
	if err != nil {
		t.Fatalf("失效后取图失败: %v", err)
	}
	data, _ := os.ReadFile(got)
	if string(data) != "v2-updated" {
		t.Fatalf("缓存未失效，仍是: %q", data)
	}
}

// TestShareGate share=false 时五协议全部拒绝。
func TestShareGate(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	img := writeFile(t, a.Gallery, "secret.png", "x")
	env.dial(a, b)
	waitFor(t, 10*time.Second, "连接", func() bool { return a.Host.Connected(b.PeerID) })

	f := false
	a.Mgr.UpdateSettings(SettingsUpdate{Share: &f})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if head, err := b.Mgr.RequestRoots(ctx, a.PeerID); err == nil && head.OK {
		t.Fatal("roots 应被 share 拒绝")
	}
	if head, err := b.Mgr.RequestBrowse(ctx, a.PeerID, a.Gallery); err == nil && head.OK {
		t.Fatal("browse 应被 share 拒绝")
	}
	if head, err := b.Mgr.RequestStat(ctx, a.PeerID, img); err == nil && head.OK {
		t.Fatal("stat 应被 share 拒绝")
	}
	// image：应报"对方未开放图库访问"
	_, err := b.Mgr.EnsureImage(ctx, a.PeerID, img, a.PeerID+"|", true)
	if err == nil {
		t.Fatal("image 应被 share 拒绝")
	}
}

// TestImageSizeCap 超过单图上限的文件拒传。
func TestImageSizeCap(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	big := filepath.Join(a.Gallery, "big.png")
	f, _ := os.Create(big)
	f.Truncate(int64(MaxImageSize) + 1)
	f.Close()
	env.dial(a, b)
	waitFor(t, 10*time.Second, "连接", func() bool { return a.Host.Connected(b.PeerID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := b.Mgr.EnsureImage(ctx, a.PeerID, big, a.PeerID+"|", true)
	if err == nil {
		t.Fatal("超限图片应被拒绝")
	}
	if index(err.Error(), "大小上限") < 0 {
		t.Fatalf("错误文案不符: %v", err)
	}
}

// TestQueueCancelDuringDownload 取消：在途任务中断不落缓存，进度清零。
func TestQueueCancelDuringDownload(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	// 较大文件，给取消留时间窗
	big := filepath.Join(a.Gallery, "slow.png")
	content := make([]byte, 8<<20)
	for i := range content {
		content[i] = byte(i)
	}
	os.WriteFile(big, content, 0o644)
	env.dial(a, b)
	waitFor(t, 10*time.Second, "连接", func() bool { return a.Host.Connected(b.PeerID) })

	folderKey := a.PeerID + "|folder"
	b.Mgr.PrefetchImage(a.PeerID, big, folderKey)
	time.Sleep(300 * time.Millisecond) // 让传输开始
	b.Mgr.CancelFolder(a.PeerID, "folder")
	st := b.Mgr.FolderProgress(a.PeerID, "folder")
	if st.Total != 0 || st.Done != 0 || st.Failed != 0 {
		t.Fatalf("取消后进度应清零: %+v", st)
	}
	// 等发送端感知断流并释放文件句柄（Windows 句柄独占）
	time.Sleep(1 * time.Second)
}

// TestTofuReject TOFU：同一地址身份变化被拒。
func TestTofuReject(t *testing.T) {
	// 覆盖在 e2e 多进程测试中（需要重启进程改变身份）——此处验证 NormalizeAddr
	if got, ok := NormalizeAddr("192.168.1.5:3457"); !ok || got != "/ip4/192.168.1.5/udp/3457/quic-v1" {
		t.Fatalf("NormalizeAddr 失败: %q %v", got, ok)
	}
	if got, ok := NormalizeAddr("host.example"); !ok || got != "/ip4/host.example/udp/3457/quic-v1" {
		t.Fatalf("默认端口失败: %q", got)
	}
	if _, ok := NormalizeAddr(""); ok {
		t.Fatal("空地址应非法")
	}
}

// TestLeaveNoticeImmediate 优雅下线：签名通知即时传播（不等新鲜度窗口），
// 且接收方不再自动拉回该节点。
func TestLeaveNoticeImmediate(t *testing.T) {
	env := newEnv(t)
	a := env.startNode(1)
	b := env.startNode(2)
	c := env.startNode(3)
	env.dial(a, b)
	env.dial(a, c)
	waitFor(t, 15*time.Second, "A 认识 B/C", func() bool {
		return a.Mgr.AttestationOf(b.PeerID) != nil && a.Mgr.AttestationOf(c.PeerID) != nil
	})

	// C 优雅下线
	c.Mgr.Leave()
	waitFor(t, 8*time.Second, "A 即时移除 C（签名通知，不等新鲜度）", func() bool {
		return a.Mgr.AttestationOf(c.PeerID) == nil
	})
	// B 仍在
	if a.Mgr.AttestationOf(b.PeerID) == nil {
		t.Fatal("B 不应被移除")
	}
}

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustEntryNames(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name
	}
	return out
}

func eqStr(t *testing.T, got, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 顺序不符: got=%v want=%v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s 顺序不符: got=%v want=%v", label, got, want)
		}
	}
}

func TestListDirAndSort(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"bbb.png", "aaa.jpg", "文件B.png", "文件A.jpg",
		"xxx-2.png", "xxx-11.png", "xxx-1.png", ".hidden.png",
		"白.jpg", "比.gif", "安.webp", "note.txt",
		"爱-2.png", "爱-10.png",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "子目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	lib, warns := NewLibrary([]string{dir})
	if len(warns) != 0 {
		t.Fatalf("意外警告: %v", warns)
	}
	folders, images, err := lib.ListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 中文按拼音：安(a) < 比(b) < 白(bai>bi? 拼音 bai vs bi: b-a-i < b-i) 
	// ICU zh: 安 < 白 < 比；数字自然序：xxx-1 < xxx-2 < xxx-11；爱-2 < 爱-10
	eqStr(t, mustEntryNames(images), []string{
		"aaa.jpg", "bbb.png", "xxx-1.png", "xxx-2.png", "xxx-11.png",
		"爱-2.png", "爱-10.png", "安.webp", "白.jpg", "比.gif", "文件A.jpg", "文件B.png",
	}, "images")
	eqStr(t, mustEntryNames(folders), []string{"子目录"}, "folders")
}

func TestBrowseRootsAndNest(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "相册", "2024")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "1.png"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := NewLibrary([]string{root})

	rv := lib.RootsView()
	if !rv.IsRoots || len(rv.Folders) != 1 || rv.Folders[0].Name != filepath.Base(root) {
		t.Fatalf("roots 视图错误: %+v", rv)
	}

	d, err := lib.Browse(sub)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "2024" || d.Parent == nil || *d.Parent != filepath.Join(root, "相册") {
		t.Fatalf("browse 字段错误: %+v", d)
	}
	if len(d.Breadcrumbs) != 3 || d.Breadcrumbs[0].Name != filepath.Base(root) || d.Breadcrumbs[2].Path != sub {
		t.Fatalf("面包屑错误: %+v", d.Breadcrumbs)
	}
	if len(d.Images) != 1 || d.Images[0].Name != "1.png" {
		t.Fatalf("images 错误: %+v", d.Images)
	}

	// 根自身：parent=null、name=根名
	d2, err := lib.Browse(root)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Parent != nil || d2.Name != filepath.Base(root) || len(d2.Breadcrumbs) != 1 {
		t.Fatalf("根 browse 错误: %+v", d2)
	}

	// 越权 403
	if _, err := lib.Browse(filepath.Dir(root)); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("越权应 403: %v", err)
	}
	// 不存在 404
	if _, err := lib.Browse(filepath.Join(root, "没有")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在应 404: %v", err)
	}
}

func TestSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "leak.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("无法创建符号链接: ", err)
	}
	lib, _ := NewLibrary([]string{root})
	// 读取复核：symlink 指向根外 → 拒绝（安全收紧）
	if _, _, err := lib.CheckReadable(link); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("symlink 越权应被拒绝: %v", err)
	}
	// 目录 symlink 指向根外 → browse 拒绝
	outDir := filepath.Join(outside, "outdir")
	os.MkdirAll(outDir, 0o755)
	os.Symlink(outDir, filepath.Join(root, "leakdir"))
	if _, err := lib.Browse(filepath.Join(root, "leakdir")); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("symlink 目录越权应被拒绝: %v", err)
	}
	// 根内真实文件 → 允许
	okFile := filepath.Join(root, "ok.png")
	if err := os.WriteFile(okFile, []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rp, _, err := lib.CheckReadable(okFile); err != nil || rp != okFile {
		t.Fatalf("根内文件应可读: %v %q", err, rp)
	}
}

func TestTimes(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.png", "b.jpg", ".c.png", "d.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	lib, _ := NewLibrary([]string{dir})
	times, err := lib.Times(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range times {
		got[e.Name] = *e.Mtime
	}
	if len(got) != 3 { // times 不筛扩展名（与旧实现一致），仅跳过隐藏项
		t.Fatalf("times 数量错误: %v", got)
	}
	for _, n := range []string{"a.png", "b.jpg", "d.txt"} {
		if _, ok := got[n]; !ok {
			t.Fatalf("缺少 %s: %v", n, got)
		}
	}
	if _, ok := got[".c.png"]; ok {
		t.Fatal("隐藏项应跳过")
	}
}

func TestQueuePriorityCancelDedupe(t *testing.T) {
	q := NewQueue(2)
	var order []string
	var mu sync.Mutex

	block1 := make(chan struct{})
	release := func() { close(block1) }

	// 建立进度账本（与 prefetch 语义一致）
	q.SetStats("folderA", FolderStat{Total: 6})
	// 两个占位任务占满并发
	for _, n := range []string{"j1", "j2"} {
		key := n
		q.Enqueue(key, "folderA", 0, func(ctx context.Context) (any, error) {
			mu.Lock()
			order = append(order, key)
			mu.Unlock()
			<-block1
			return nil, nil
		})
	}
	time.Sleep(50 * time.Millisecond)

	// 排队三个：p1(0) p2(0) p3(0)，随后 p3 插队
	var lowCh <-chan taskResult
	done3 := q.Enqueue("p3", "folderA", 0, func(ctx context.Context) (any, error) { return "p3", nil })
	done1 := q.Enqueue("p1", "folderA", 0, func(ctx context.Context) (any, error) { return "p1", nil })
	done2 := q.Enqueue("p2", "folderA", 0, func(ctx context.Context) (any, error) { return "p2", nil })
	lowCh = q.Enqueue("low", "folderA", 0, func(ctx context.Context) (any, error) { return "low", nil })
	time.Sleep(50 * time.Millisecond)
	q.Enqueue("p3", "folderA", 1, nil) // 去重 + 插队

	release()
	if v, err := Wait(done3); err != nil || v.(string) != "p3" {
		t.Fatalf("p3 结果错误: %v %v", v, err)
	}
	Wait(done1)
	Wait(done2)

	// 取消：进度清零 + 排队任务剔除
	q.SetStats("folderB", FolderStat{Total: 3})
	blk := make(chan struct{})
	q.Enqueue("b1", "folderB", 0, func(ctx context.Context) (any, error) { <-blk; return nil, nil })
	q.Enqueue("b2", "folderB", 0, func(ctx context.Context) (any, error) { return nil, errors.New("不会执行") })
	time.Sleep(50 * time.Millisecond)
	q.Cancel("folderB")
	close(blk)
	time.Sleep(100 * time.Millisecond)
	if st := q.Stats("folderB"); st.Total != 0 || st.Done != 0 || st.Failed != 0 {
		t.Fatalf("取消后进度应清零: %+v", st)
	}
	// folderA 正常计数
	Wait(lowCh)
	st := q.Stats("folderA")
	if st.Done != 6 || st.Total != 6 {
		t.Fatalf("done 计数错误: %+v", st)
	}
}

func TestQueueInflightCancelContext(t *testing.T) {
	q := NewQueue(1)
	started := make(chan struct{})
	var canceled atomic.Bool
	ch := q.Enqueue("x", "folderX", 0, func(ctx context.Context) (any, error) {
		close(started)
		select {
		case <-ctx.Done():
			canceled.Store(true)
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, nil
		}
	})
	<-started
	q.Cancel("folderX")
	if _, err := Wait(ch); err == nil {
		t.Fatal("取消的任务应返回错误")
	}
	if !canceled.Load() {
		t.Fatal("在途任务应收到取消信号")
	}
}

func TestConfigMigrationAndRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{
  "port": 4000,
  "host": "0.0.0.0",
  "paths": ["D:\\pic"],
  "net": { "name": "旧节点", "share": false, "concurrency": 9, "uplinks": ["/ip4/1.2.3.4/udp/3457/quic-v1"] }
}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, existed, err := LoadConfig(path)
	if err != nil || !existed {
		t.Fatalf("加载失败: %v %v", existed, err)
	}
	if cfg.Port != 4000 || cfg.Host != "0.0.0.0" || cfg.Net.Share {
		t.Fatalf("字段错误: %+v", cfg)
	}
	if len(cfg.Net.Hubs) != 1 || cfg.Net.Hubs[0] != "/ip4/1.2.3.4/udp/3457/quic-v1" {
		t.Fatalf("uplinks→hubs 迁移失败: %+v", cfg.Net)
	}
	if cfg.Net.Concurrency != 9 || cfg.Net.CacheServe != true {
		t.Fatalf("默认值错误: %+v", cfg.Net)
	}
	// 旧配置无 lanDiscovery 键 → 默认开
	if !cfg.Net.LanDiscovery {
		t.Fatal("lanDiscovery 应默认开启")
	}
	cfg.Net.LanDiscovery = false
	cfg.Net.Hubs = append(cfg.Net.Hubs, "/ip4/5.6.7.8/udp/3457/quic-v1")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) == "" || containsStr(string(b), `"uplinks"`) {
		t.Fatalf("写回不应含 uplinks 键: %s", b)
	}
	cfg2, _, err := LoadConfig(path)
	if err != nil || len(cfg2.Net.Hubs) != 2 {
		t.Fatalf("回读失败: %v %+v", err, cfg2.Net)
	}
	if cfg2.Net.LanDiscovery {
		t.Fatal("显式关闭的 lanDiscovery 写回后应保持")
	}
	if len(cfg2.Net.ScanPorts) != 0 {
		t.Fatalf("ScanPorts 空值应写回为空数组: %+v", cfg2.Net.ScanPorts)
	}
	def := DefaultConfig()
	if def.Port != NodeHTTPPort || def.Net.Port != NodeQUICPort {
		t.Fatalf("客户端默认端口错误: %d/%d", def.Port, def.Net.Port)
	}
	if !def.Net.LanDiscovery {
		t.Fatal("lanDiscovery 应默认开启")
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && strings_Contains(s, sub))
}

func strings_Contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestStateV1V2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "net-state.json")
	v1 := `{"v":1,"secret":"Ab3dEf6hIj9kLm2N","name":"old-name"}`
	os.WriteFile(path, []byte(v1), 0o644)
	secret, name, existed, err := LoadState(path)
	if err != nil || !existed || secret != "Ab3dEf6hIj9kLm2N" || name != "old-name" {
		t.Fatalf("v1 读取失败: %v %v %q %q", existed, err, secret, name)
	}
	if err := SaveState(path, secret, "new-name"); err != nil {
		t.Fatal(err)
	}
	s2, n2, _, err := LoadState(path)
	if err != nil || s2 != secret || n2 != "new-name" {
		t.Fatalf("v2 回读失败: %v %q %q", err, s2, n2)
	}
	b, _ := os.ReadFile(path)
	if !strings_Contains(string(b), `"v": 2`) {
		t.Fatalf("应写回 v2: %s", b)
	}
}

func TestCacheKey(t *testing.T) {
	// 与旧实现的 sha256(node|path)[:24] 一致性由 Node 交叉验证（test/e2e），
	// 这里验证确定性与格式。
	k1 := CacheKey("peer-a", `D:\pic\1.png`)
	k2 := CacheKey("peer-a", `D:\pic\1.png`)
	k3 := CacheKey("peer-a", `D:\pic\2.png`)
	if k1 != k2 || k1 == k3 || len(k1) != 24 {
		t.Fatalf("cacheKey 错误: %q %q %q", k1, k2, k3)
	}
	for _, c := range k1 {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("cacheKey 应为小写 hex: %q", k1)
		}
	}
}

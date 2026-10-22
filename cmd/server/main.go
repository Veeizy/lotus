// lotus-server：常驻中转服务节点（CLI，Windows/Linux）。
//
// 与 lotus（单机图库服务）复用同一套组网核心；差异在定位：
//   - 默认入网服务：启动即监听（QUIC + HTTP），被动等待其他 lotus 节点握手接入；
//   - 显式配置文件（--config，默认 ./server.json）决定共享文件夹与启动时加入的网络；
//   - 零文件夹亦可运行（纯中转：转发链 + 链路缓存代答，不受 share 门控）；
//   - 查询面：访问记录 / 按对端流量 / 服务时间（status、access、traffic 子命令）；
//   - 可注册为系统服务（Windows 服务管理器 / systemd）。
//
// 用法:
//	lotus-server <命令> [参数]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lotus/internal/api"
	"lotus/internal/app"
	"lotus/internal/core"
)

const defaultCfgName = "server.json"

func usage() {
	fmt.Println(`lotus-server：图片网络常驻中转服务（CLI）

用法:
  lotus-server <命令> [参数] [--config <文件>]

命令:
  run                   启动服务（默认命令；常驻，等待其他节点接入）
  status                查询运行状态与服务时间
  access [-n <条数>]    查看访问记录（默认 50 条）
  traffic               查看按对端流量统计
  folders               查看共享文件夹
  folders add <路径>...      添加共享文件夹（热生效）
  folders remove <路径>...   移除共享文件夹（热生效）
  join <地址>...        加入网络（热生效并写入配置）
  leave [<地址>...]     离开网络（无地址 = 全部退出）
  install               注册为系统服务（Windows 服务 / systemd，开机自启）
  uninstall             移除系统服务
  start | stop          启动 / 停止系统服务
  version               显示版本

通用选项:
  --config <文件>       显式配置文件路径，默认 ./server.json

run 选项:
  --port <端口>         HTTP 端口，默认 3456（其他节点经此加入：IP:端口）
  --host <地址>         HTTP 监听地址，默认 0.0.0.0（服务节点需对外可达）
  --quic-port <端口>    节点互联 QUIC/UDP 端口，默认 3457
  --name <名称>         节点显示名，默认取状态文件或计算机名
  --no-mdns             关闭局域网自动发现
  --delay-ms <毫秒>     [测试] 节点流 I/O 模拟延迟
  --fresh-sec <秒>      [测试] 在线新鲜度窗口

配置文件（server.json，与桌面版 config.json 同构）:
  { "port": 3456, "host": "0.0.0.0", "paths": ["D:\\图片库"],
    "net": { "name": "家里服务器", "hubs": ["192.168.1.10:3456"], ... } }
  paths = 共享给网络的文件夹；net.hubs = 启动时加入的网络地址列表。

示例:
  lotus-server run --config /etc/lotus/server.json
  lotus-server folders add "D:\图片库"
  lotus-server join 192.168.1.10:3456
  lotus-server status`)
}

func version() {
	fmt.Println("lotus-server（picnet v2 协议）")
}

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	// Windows 服务控制管理器拉起：走 SCM 生命周期
	if isWindowsService() {
		runAsService(args)
		return
	}
	if err := dispatch(cmd, args); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(cmd string, args []string) error {
	switch cmd {
	case "-h", "--help", "help":
		usage()
	case "version", "-v", "--version":
		version()
	case "run":
		return cmdRun(args)
	case "status", "traffic":
		return cmdQuery(cmd, args)
	case "access":
		return cmdAccess(args)
	case "folders":
		return cmdFolders(args)
	case "join":
		return cmdJoin(args)
	case "leave":
		return cmdLeave(args)
	case "install":
		return cmdInstall(args)
	case "uninstall":
		return cmdUninstall(args)
	case "start":
		return cmdStart(args)
	case "stop":
		return cmdStop(args)
	default:
		usage()
		return fmt.Errorf("未知命令: %s", cmd)
	}
	return nil
}

/* ---------------- daemon（run） ---------------- */

type daemonOpts struct {
	cfgPath  string
	port     int
	host     string
	quicPort int
	name     string
	noMDNS   bool
	delayMS  int
	freshSec int
	svcLog   bool // 系统服务模式：日志写文件（无控制台可看）
}

func newRunFlags(o *daemonOpts) *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&o.cfgPath, "config", defaultCfgName, "")
	fs.IntVar(&o.port, "port", 0, "")
	fs.StringVar(&o.host, "host", "", "")
	fs.IntVar(&o.quicPort, "quic-port", 0, "")
	fs.StringVar(&o.name, "name", "", "")
	fs.BoolVar(&o.noMDNS, "no-mdns", false, "")
	fs.IntVar(&o.delayMS, "delay-ms", 0, "")
	fs.IntVar(&o.freshSec, "fresh-sec", 0, "")
	fs.Usage = usage
	return fs
}

func cmdRun(args []string) error {
	var o daemonOpts
	fs := newRunFlags(&o)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runDaemon(o, nil) // nil stop = 等 Ctrl+C
}

// runDaemon 启动全部子系统并阻塞到 stop 关闭（或收到 Ctrl+C/SIGTERM）。
// stop 为 nil 时自建信号通道；服务模式由 SCM 处理器传入。
func runDaemon(o daemonOpts, stop <-chan struct{}) error {
	cfgPathAbs, err := filepath.Abs(o.cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(o)
	cfg, existed, err := core.LoadConfig(cfgPathAbs)
	if err != nil {
		return fmt.Errorf("配置文件解析失败: %v", err)
	}
	if !existed {
		// 服务节点默认：对外可达 + 服务端端口套（客户端默认 3466/3467 已分离）。
		// 局域网自动发现对本服务无意义（纯中转，被他人发现即可），默认关闭。
		cfg.Host = "0.0.0.0"
		cfg.Port = core.ServerHTTPPort
		cfg.Net.Port = core.ServerQUICPort
		cfg.Net.LanDiscovery = false
		if err := core.SaveConfig(cfgPathAbs, cfg); err != nil {
			return fmt.Errorf("默认配置写入失败: %v", err)
		}
		fmt.Printf("已生成默认配置 %s（编辑后重启生效；也可用 folders add / join 热配置）\n", cfgPathAbs)
	}
	// 命令行参数 > server.json
	if o.port != 0 {
		cfg.Port = o.port
	}
	if o.host != "" {
		cfg.Host = o.host
	}
	if o.quicPort != 0 {
		cfg.Net.Port = o.quicPort
	}
	if o.name != "" {
		cfg.Net.Name = o.name
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := app.New(ctx, app.Options{
		Cfg:       cfg,
		CfgPath:   cfgPathAbs,
		StateFile: "server-state.json", // 与同目录客户端的 net-state.json 区分：同机双进程各自持独立身份
		NoMDNS:    o.noMDNS,
		DelayMS:   o.delayMS,
		FreshSec:  o.freshSec,
	}, log)
	if err != nil {
		return err
	}
	for _, w := range a.Warns {
		fmt.Fprintln(os.Stderr, w)
	}
	_ = core.SaveConfig(cfgPathAbs, cfg) // 默认字段落盘

	srv := api.New(a)
	httpServer := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler: srv.Handler(),
	}
	ln, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "listen" {
			return fmt.Errorf("端口 %d 已被占用，请用 --port 或配置文件换一个端口", cfg.Port)
		}
		return fmt.Errorf("监听失败: %w", err)
	}
	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP 服务异常退出", "err", err)
			os.Exit(1)
		}
	}()

	printBanner(a, cfg, ln.Addr().String())
	shutdown := func() {
		fmt.Println("\n正在退出…")
		shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shCancel()
		_ = httpServer.Shutdown(shCtx)
		cancel()
		a.Close()
		fmt.Println("已退出")
	}
	if stop != nil {
		<-stop
		shutdown()
		return nil
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	shutdown()
	return nil
}

func newLogger(o daemonOpts) *slog.Logger {
	lvl := slog.LevelInfo
	if os.Getenv("PIC_LOG") == "debug" {
		lvl = slog.LevelDebug
	}
	var w io.Writer = os.Stderr
	if o.svcLog {
		// 服务模式无控制台：日志落在配置同目录（持续追加）
		if f, err := os.OpenFile(filepath.Join(filepath.Dir(o.cfgPath), "lotus-server.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			w = f
		}
	}
	return slog.New(app.NewMdnsSpamFilter(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})))
}

func printBanner(a *app.App, cfg *core.Config, addr string) {
	fmt.Println("lotus-server 已启动（中转模式）")
	fmt.Printf("  节点 ID:    %s\n", a.PeerID)
	if cfg.Net.Name != "" {
		fmt.Printf("  节点名:     %s\n", cfg.Net.Name)
	}
	shown := cfg.Host
	if shown == "" || shown == "0.0.0.0" || shown == "::" {
		if ip := lanIP(); ip != "" {
			shown = ip
		} else {
			shown = "本机IP"
		}
	}
	fmt.Printf("  加入本网络: 在其他节点填入  %s:%d\n", shown, cfg.Port)
	fmt.Printf("  QUIC:       0.0.0.0:%d\n", cfg.Net.Port)
	roots := a.Lib().Roots()
	if len(roots) == 0 {
		fmt.Println("  共享文件夹: （无 —— 纯中转模式）")
	} else {
		fmt.Printf("  共享文件夹: %d 个\n", len(roots))
		for _, r := range roots {
			fmt.Printf("    - %s\n", r.Dir)
		}
	}
	if hubs := a.Net.Hubs(); len(hubs) > 0 {
		fmt.Printf("  已加入网络: %d 个（%s）\n", len(hubs), strings.Join(hubs, ", "))
	}
	fmt.Println("  查询:       lotus-server status | access | traffic")
	fmt.Println("被动等待其他节点接入…（Ctrl+C 退出）")
}

// lanIP 取一个对外的局域网 IPv4（展示用，失败为空）。
func lanIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

/* ---------------- 查询面（CLI → 运行中服务的本地 HTTP） ---------------- */

// daemonTarget 从配置文件解析运行中服务的查询地址与 token。
func daemonTarget(cfgPath string) (baseURL, token string, err error) {
	cfgPathAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(cfgPathAbs); err != nil {
		return "", "", fmt.Errorf("配置文件不存在: %s", cfgPathAbs)
	}
	cfg, _, err := core.LoadConfig(cfgPathAbs)
	if err != nil {
		return "", "", fmt.Errorf("配置文件解析失败: %v", err)
	}
	host := cfg.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Port)), cfg.API.Token, nil
}

func adminGet(baseURL, token, path string, out any) error {
	return adminCall(http.MethodGet, baseURL, token, path, nil, out)
}

func adminPost(baseURL, token, path string, body any, out any) error {
	return adminCall(http.MethodPost, baseURL, token, path, body, out)
}

func adminCall(method, baseURL, token, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("服务未运行或不可达（%s）——请先启动 lotus-server run", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return errors.New("鉴权失败：配置文件中的 api.token 与运行中服务不一致")
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type statusReply struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StartedAt int64  `json:"startedAt"`
	UptimeSec int64  `json:"uptimeSec"`
	HTTPPort  int    `json:"httpPort"`
	QUICPort  int    `json:"quicPort"`
	Joined    bool   `json:"joined"`
	Hub       bool   `json:"hub"`
	Requests  int64  `json:"requests"`
	Folders   []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"folders"`
	Uplinks []struct {
		Addr   string `json:"addr"`
		Status string `json:"status"`
	} `json:"uplinks"`
	Members []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Direct bool   `json:"direct"`
	} `json:"members"`
	Kinds []struct {
		Kind  string `json:"kind"`
		Count int64  `json:"count"`
		OK    int64  `json:"ok"`
		Fail  int64  `json:"fail"`
		Bytes int64  `json:"bytes"`
		DurMS int64  `json:"durMs"`
	} `json:"kinds"`
	Traffic struct {
		TotalIn  int64 `json:"totalIn"`
		TotalOut int64 `json:"totalOut"`
	} `json:"traffic"`
}

func cmdStatus(cfgPath string) error {
	base, token, err := daemonTarget(cfgPath)
	if err != nil {
		return err
	}
	var st statusReply
	if err := adminGet(base, token, "/api/v1/admin/status", &st); err != nil {
		return err
	}
	name := st.Name
	if name == "" {
		name = "（未命名）"
	}
	fmt.Println("lotus-server 运行状态")
	fmt.Printf("  节点 ID:    %s（%s）\n", st.ID, name)
	started := time.UnixMilli(st.StartedAt).Format("2006-01-02 15:04:05")
	fmt.Printf("  服务时间:   启动于 %s，已运行 %s\n", started, fmtDur(time.Duration(st.UptimeSec)*time.Second))
	fmt.Printf("  端口:       HTTP %d / QUIC %d\n", st.HTTPPort, st.QUICPort)
	fmt.Printf("  共享文件夹: %d 个\n", len(st.Folders))
	for _, f := range st.Folders {
		fmt.Printf("    - %s\n", f.Path)
	}
	connUplinks := 0
	for _, u := range st.Uplinks {
		if u.Status == "connected" {
			connUplinks++
		}
	}
	fmt.Printf("  网络:       已加入 %d 个（连通 %d），成员 %d，中转服务中: %t\n",
		len(st.Uplinks), connUplinks, len(st.Members), st.Hub)
	fmt.Printf("  流量:       收 %s / 发 %s\n", fmtBytes(st.Traffic.TotalIn), fmtBytes(st.Traffic.TotalOut))
	fmt.Printf("  访问:       共 %d 次\n", st.Requests)
	for _, k := range st.Kinds {
		avg := int64(0)
		if k.Count > 0 {
			avg = k.DurMS / k.Count
		}
		line := fmt.Sprintf("    %-10s %5d 次（成功 %d / 失败 %d）", k.Kind, k.Count, k.OK, k.Fail)
		if k.Bytes > 0 {
			line += fmt.Sprintf("，%s", fmtBytes(k.Bytes))
		}
		if k.Count > 0 {
			line += fmt.Sprintf("，均耗 %dms", avg)
		}
		fmt.Println(line)
	}
	return nil
}

func cmdQuery(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", defaultCfgName, "")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cmd == "status" {
		return cmdStatus(*cfgPath)
	}
	return cmdTraffic(*cfgPath)
}

func cmdTraffic(cfgPath string) error {
	base, token, err := daemonTarget(cfgPath)
	if err != nil {
		return err
	}
	var rep struct {
		Peers []struct {
			Peer string `json:"peer"`
			In   int64  `json:"in"`
			Out  int64  `json:"out"`
		} `json:"peers"`
		TotalIn  int64 `json:"totalIn"`
		TotalOut int64 `json:"totalOut"`
	}
	if err := adminGet(base, token, "/api/v1/admin/traffic", &rep); err != nil {
		return err
	}
	fmt.Println("lotus-server 流量统计（五协议节点流，按对端）")
	fmt.Printf("  合计: 收 %s / 发 %s\n\n", fmtBytes(rep.TotalIn), fmtBytes(rep.TotalOut))
	if len(rep.Peers) == 0 {
		fmt.Println("  （暂无流量）")
		return nil
	}
	fmt.Printf("  %-20s %12s %12s\n", "对端", "收", "发")
	for _, p := range rep.Peers {
		fmt.Printf("  %-20s %12s %12s\n", shortPeer(p.Peer), fmtBytes(p.In), fmtBytes(p.Out))
	}
	return nil
}

func cmdAccess(args []string) error {
	fs := flag.NewFlagSet("access", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultCfgName, "")
	n := fs.Int("n", 50, "")
	fs.IntVar(n, "limit", 50, "")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}
	base, token, err := daemonTarget(*cfgPath)
	if err != nil {
		return err
	}
	var rep struct {
		Entries []struct {
			TS    int64  `json:"ts"`
			Kind  string `json:"kind"`
			Peer  string `json:"peer"`
			Path  string `json:"path"`
			Bytes int64  `json:"bytes"`
			DurMS int64  `json:"durMs"`
			OK    bool   `json:"ok"`
			Extra string `json:"extra"`
		} `json:"entries"`
	}
	if err := adminGet(base, token, fmt.Sprintf("/api/v1/admin/access?limit=%d", *n), &rep); err != nil {
		return err
	}
	fmt.Printf("lotus-server 访问记录（最近 %d 条，新→旧）\n", len(rep.Entries))
	for _, e := range rep.Entries {
		mark := "✓"
		if !e.OK {
			mark = "✗"
		}
		line := fmt.Sprintf("  %s %s %-10s %-16s %s",
			time.UnixMilli(e.TS).Format("01-02 15:04:05"), mark, e.Kind, shortPeer(e.Peer), e.Path)
		if e.Bytes > 0 {
			line += fmt.Sprintf(" %s", fmtBytes(e.Bytes))
		}
		if e.DurMS > 0 {
			line += fmt.Sprintf(" %dms", e.DurMS)
		}
		if e.Extra != "" {
			line += "  " + e.Extra
		}
		fmt.Println(line)
	}
	return nil
}

func shortPeer(id string) string {
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	days := int64(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	h := int64(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int64(d / time.Minute)
	if days > 0 {
		return fmt.Sprintf("%dd%dh%dm", days, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

/* ---------------- folders / join / leave 管理 ---------------- */

func cmdFolders(args []string) error {
	op := "list"
	if len(args) > 0 && (args[0] == "add" || args[0] == "remove" || args[0] == "rm") {
		op = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("folders", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultCfgName, "")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfgPathAbs, _ := filepath.Abs(*cfgPath)
	cfg, existed, err := core.LoadConfig(cfgPathAbs)
	if err != nil {
		return fmt.Errorf("配置文件解析失败: %v", err)
	}
	switch op {
	case "list":
		if !existed {
			fmt.Println("（配置文件不存在，尚无共享文件夹）")
			return nil
		}
		fmt.Printf("共享文件夹（%s）：%d 个\n", cfgPathAbs, len(cfg.Paths))
		for _, p := range cfg.Paths {
			fmt.Printf("  - %s\n", p)
		}
		return nil

	case "add", "remove":
		paths := map[string]bool{}
		for _, p := range cfg.Paths {
			paths[filepath.Clean(p)] = true
		}
		changed := false
		for _, arg := range fs.Args() {
			p := filepath.Clean(strings.TrimSpace(arg))
			if p == "" || p == "." {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(cfgPathAbs), p)
			}
			if op == "add" {
				if st, err := os.Stat(p); err != nil || !st.IsDir() {
					fmt.Fprintf(os.Stderr, "跳过（不是文件夹）: %s\n", p)
					continue
				}
				if !paths[p] {
					paths[p] = true
					changed = true
				}
			} else if paths[p] {
				delete(paths, p)
				changed = true
			} else {
				// Windows 大小写不敏感兜底
				for k := range paths {
					if strings.EqualFold(k, p) {
						delete(paths, k)
						changed = true
					}
				}
			}
		}
		if !changed {
			fmt.Println("没有变化")
			return nil
		}
		cfg.Paths = sortedKeys(paths)
		if err := core.SaveConfig(cfgPathAbs, cfg); err != nil {
			return fmt.Errorf("配置保存失败: %v", err)
		}
		// 服务运行中则热生效（daemon 自己再落盘一次，结果一致）
		if base, token, err := daemonTarget(*cfgPath); err == nil {
			var out struct {
				OK    bool     `json:"ok"`
				Warns []string `json:"warns"`
			}
			if err := adminPost(base, token, "/api/config/paths", map[string]any{"paths": cfg.Paths}, &out); err != nil {
				fmt.Printf("已写入配置（服务未运行，下次启动生效）：%s\n", err)
				return nil
			}
			for _, w := range out.Warns {
				fmt.Fprintln(os.Stderr, w)
			}
		}
		fmt.Printf("已%s，当前 %d 个文件夹（已热生效）\n", map[string]string{"add": "添加", "remove": "移除"}[op], len(cfg.Paths))
		for _, p := range cfg.Paths {
			fmt.Printf("  - %s\n", p)
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultCfgName, "")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}
	addrs := fs.Args()
	if len(addrs) == 0 {
		return errors.New("用法: lotus-server join <地址>...（如 192.168.1.10:3456）")
	}
	base, token, err := daemonTarget(*cfgPath)
	if err != nil {
		return err
	}
	var out struct {
		OK      bool `json:"ok"`
		Results []struct {
			Addr  string `json:"addr"`
			OK    bool   `json:"ok"`
			Node  string `json:"node"`
			Error string `json:"error"`
			Note  string `json:"note"`
		} `json:"results"`
	}
	if err := adminPost(base, token, "/api/net/join", map[string]any{"addrs": addrs}, &out); err != nil {
		// 服务未运行：写入配置，下次启动自动加入
		cfgPathAbs, _ := filepath.Abs(*cfgPath)
		cfg, existed, err := core.LoadConfig(cfgPathAbs)
		if err != nil {
			return err
		}
		if !existed {
			return fmt.Errorf("服务未运行且配置文件不存在，先运行 lotus-server run 生成")
		}
		added := 0
		for _, raw := range addrs {
			if norm, ok := normalizeJoin(raw); ok && !contains(cfg.Net.Hubs, norm) {
				cfg.Net.Hubs = append(cfg.Net.Hubs, norm)
				added++
			}
		}
		if err := core.SaveConfig(cfgPathAbs, cfg); err != nil {
			return err
		}
		fmt.Printf("服务未运行：已写入配置，下次启动将自动加入（新增 %d 个）\n", added)
		return nil
	}
	for _, r := range out.Results {
		if r.OK {
			note := ""
			if r.Note != "" {
				note = "（" + r.Note + "）"
			}
			fmt.Printf("  ✓ %s → %s%s\n", r.Addr, r.Node, note)
		} else {
			fmt.Printf("  ✗ %s → %s\n", r.Addr, r.Error)
		}
	}
	return nil
}

func cmdLeave(args []string) error {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultCfgName, "")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}
	addrs := fs.Args()
	base, token, err := daemonTarget(*cfgPath)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		if err := adminPost(base, token, "/api/net/leave", nil, nil); err != nil {
			// 离线兜底：清空配置里的 hubs
			return leaveOffline(*cfgPath, nil)
		}
		fmt.Println("已退出全部网络")
		return nil
	}
	for _, addr := range addrs {
		var out map[string]any
		err := adminPost(base, token, "/api/v1/admin/hub/remove", map[string]any{"addr": addr}, &out)
		if err == nil {
			fmt.Printf("  ✓ 已离开 %s\n", addr)
			continue
		}
		if strings.Contains(err.Error(), "服务未运行") {
			return leaveOffline(*cfgPath, addrs)
		}
		fmt.Printf("  ✗ %s → %s\n", addr, err)
	}
	return nil
}

// leaveOffline 服务未运行时直接改配置（下次启动生效）。
func leaveOffline(cfgPath string, addrs []string) error {
	cfgPathAbs, _ := filepath.Abs(cfgPath)
	cfg, existed, err := core.LoadConfig(cfgPathAbs)
	if err != nil || !existed {
		return fmt.Errorf("服务未运行且配置不可用")
	}
	if addrs == nil {
		cfg.Net.Hubs = nil
	} else {
		var kept []string
		for _, h := range cfg.Net.Hubs {
			drop := false
			for _, a := range addrs {
				if h == a || h == normalizeOrSelf(a) {
					drop = true
				}
			}
			if !drop {
				kept = append(kept, h)
			}
		}
		cfg.Net.Hubs = kept
	}
	if err := core.SaveConfig(cfgPathAbs, cfg); err != nil {
		return err
	}
	if addrs == nil {
		fmt.Println("服务未运行：已清空配置中的网络列表，下次启动生效")
	} else {
		fmt.Printf("服务未运行：已从配置移除 %d 个地址，下次启动生效\n", len(addrs))
	}
	return nil
}

func normalizeOrSelf(a string) string {
	if n, ok := normalizeJoin(a); ok {
		return n
	}
	return a
}

// normalizeJoin 复用 node 层的地址规范化（避免 CLI 依赖 p2p 全家桶的轻量重复：
// host:port → /ip4|ip6/…/udp/N/quic-v1；multiaddr 原样）。
func normalizeJoin(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "/") {
		return s, strings.Contains(s, "/quic-v1")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = s, "3456"
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return "", false
	}
	if strings.Contains(host, ":") {
		return fmt.Sprintf("/ip6/%s/udp/%d/quic-v1", host, p), true
	}
	return fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", host, p), true
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

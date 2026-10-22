// lotus-desktop：Windows 桌面前端（TypeScript 编译产物 + WebView2 原生窗口；原二进制名 picd-desktop）。
//
// 构建：
//
//	cd desktop/frontend && npm install && npm run build   # TS → dist/
//	go build -ldflags "-H windowsgui" -o lotus-desktop.exe ./desktop   # 前端嵌入单 exe
//
// -H windowsgui 使 exe 以 GUI 子系统构建：Windows 不再为它分配黑色控制台窗口；
// 需要看日志时用 lotus-desktop.exe --debug 启动（复用当前终端，双击启动则新开控制台）。
//
// 依赖系统 WebView2 运行时（Windows 10/11 通常自带）。
// HTTP 服务仅绑定 127.0.0.1（窗口内嵌浏览器加载；节点互联走 QUIC，不受影响）。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jchv/go-webview2"

	"lotus/internal/api"
	"lotus/internal/app"
	"lotus/internal/core"
	"lotus/internal/firewall"
)

//go:embed dist
var distFS embed.FS

func usage() {
	fmt.Println(`图片浏览服务（lotus 桌面版）

用法:
  lotus-desktop [选项] <路径1> [路径2 ...]

选项:
      --config <文件>      配置文件路径，默认 ./config.json
      --title <标题>       窗口标题
      --width <px>         窗口宽度，默认 1360
      --height <px>        窗口高度，默认 860
      --debug              显示控制台与日志输出（双击启动时新开控制台窗口）
      --headless           不开窗口，仅启动本机服务（调试用，配合 --debug 看日志）
      --no-mdns            关闭局域网自动发现
      --no-auto-join       [测试] 关闭全部自动入网（本机服务 + 局域网自动发现）
      --delay-ms <毫秒>    [测试] 节点流 I/O 模拟延迟
      --fresh-sec <秒>     [测试] 在线新鲜度窗口
  -h, --help               显示帮助

不传路径时读取 config.json。`)
}

func main() {
	// --firewall-install：UAC 提权子进程入口（firewall.Elevate 经 runas 复用自身）。
	// 添加规则后直接退出，不走下面的 GUI 流程。
	if firewall.HandleInstallArgs(os.Args[1:]) {
		return
	}
	fs2 := flag.NewFlagSet("lotus-desktop", flag.ContinueOnError)
	cfgPath := fs2.String("config", "config.json", "")
	title := fs2.String("title", "图片浏览服务 - lotus", "")
	width := fs2.Int("width", 1360, "")
	height := fs2.Int("height", 860, "")
	headless := fs2.Bool("headless", false, "")
	noMDNS := fs2.Bool("no-mdns", false, "")
	noAutoJoin := fs2.Bool("no-auto-join", false, "")
	debug := fs2.Bool("debug", false, "")
	delayMS := fs2.Int("delay-ms", 0, "")
	freshSec := fs2.Int("fresh-sec", 0, "")
	fs2.Usage = usage
	if err := fs2.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}
	if *debug {
		setupDebugConsole() // 必须先于日志初始化，os.Stderr 才指向控制台
	}

	slog.SetDefault(slog.New(app.NewMdnsSpamFilter(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	log := slog.Default()

	cfg, existed, err := core.LoadConfig(*cfgPath)
	if err != nil {
		fatalf(*debug, "配置文件解析失败: %v", err)
	}
	cfgPathAbs, _ := filepath.Abs(*cfgPath)
	if existed && cfg.Host != "" && !isLoopbackHost(cfg.Host) {
		log.Warn("桌面版 HTTP 仅绑定本机（cfg.host 已忽略）", "host", cfg.Host)
	}
	if fs2.NArg() > 0 {
		cfg.Paths = fs2.Args()
	}
	if len(cfg.Paths) == 0 {
		fmt.Fprintln(os.Stderr, "提示: 尚未配置根路径。请编辑 config.json 的 paths，或启动时传入路径参数。")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := app.New(ctx, app.Options{
		Cfg:        cfg,
		CfgPath:    cfgPathAbs,
		NoMDNS:     *noMDNS,
		NoAutoJoin: *noAutoJoin,
		DelayMS:    *delayMS,
		FreshSec:   *freshSec,
	}, log)
	if err != nil {
		fatalf(*debug, "%v", err)
	}
	for _, w := range a.Warns {
		fmt.Fprintln(os.Stderr, w)
	}
	_ = core.SaveConfig(cfgPathAbs, cfg)

	// 桌面资产（TS 编译产物）替换内嵌 Web 版首页
	distSub, err := fs.Sub(distFS, "dist")
	if err != nil {
		fatalf(*debug, "桌面资产损坏: %v", err)
	}
	api.SetStaticAssets(distSub)
	// 防火墙一键放行：本进程在用户会话内，可弹 UAC；目标是自身 exe（QUIC/UDP 入站）
	api.FirewallAllowHook = func() error {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		return firewall.Elevate(self, self, "lotus 桌面版入站放行", []string{"udp"})
	}
	srv := api.New(a)

	// 仅本机：优先配置端口，占用则退回随机端口
	var ln net.Listener
	for _, port := range []int{cfg.Port, 0} {
		ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			break
		}
	}
	if ln == nil {
		fatalf(*debug, "本机端口监听失败")
	}
	port := ln.Addr().(*net.TCPAddr).Port
	httpSrv := &http.Server{Handler: srv.Handler()}
	go func() { _ = httpSrv.Serve(ln) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	fmt.Println("图片浏览服务（桌面版）已启动")
	fmt.Printf("  窗口地址: %s\n", url)
	fmt.Printf("  节点: %s（QUIC 端口 %d）\n", a.PeerID, cfg.Net.Port)
	for _, r := range a.Lib().Roots() {
		fmt.Printf("  根路径: %s\n", r.Dir)
	}

	shutdown := func() {
		shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shCancel()
		_ = httpSrv.Shutdown(shCtx)
		cancel()
		a.Close()
	}

	if *headless {
		fmt.Println("headless 模式：按 Ctrl+C 退出")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		shutdown()
		return
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  *title,
			Width:  uint(*width),
			Height: uint(*height),
			Center: true,
		},
	})
	if w == nil {
		shutdown()
		fatalf(*debug, "WebView2 运行时不可用。请安装 Microsoft WebView2 Runtime（Windows 10/11 通常自带）。")
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run() // 阻塞至窗口关闭
	shutdown()
	fmt.Println("已退出")
	_ = errors.New // 保持 errors 引用（未来扩展）
}

func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}

// fatalf 打印错误并以非零码退出；正常模式（--debug 之外）无控制台可看，
// 补一个错误对话框，避免双击启动失败时进程无声消失。
func fatalf(debug bool, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	if !debug {
		showErrorMessageBox(format, args...)
	}
	os.Exit(1)
}

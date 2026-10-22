// lotus-server-dashboard：lotus-server 的桌面管理窗口（Windows，WebView2）。
//
// 一个小 dashboard：状态与服务时间、访问记录、流量、文件夹/网络管理、
// 系统服务注册与启停。数据全部来自运行中的 lotus-server（daemon）本地管理 API，
// 本进程只做三件事：
//  1. 内嵌 dashboard 页面（TS 编译产物）起一个随机端口的本机 HTTP 服务；
//  2. /p/* 把请求带 token 代理给 daemon（token 不进前端 JS）；
//  3. /ctl/* 执行本机控制：注册/启停系统服务（UAC 提权）、前台拉起 daemon。
//
// CLI（lotus-server.exe）保持控制台风格不变；本窗口与其同目录分发。
//
// 构建：
//
//	cd server-desktop/frontend && npm install && npm run build   # TS → ../dist/
//	go build -ldflags "-H windowsgui" -o lotus-server-dashboard.exe ./server-desktop
package main

import (
	"embed"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jchv/go-webview2"

	"lotus/internal/core"
	"lotus/internal/firewall"
)

//go:embed dist
var distFS embed.FS

func usage() {
	fmt.Println(`lotus-server 桌面控制台（dashboard）

用法:
  lotus-server-dashboard [选项]

选项:
      --config <文件>      lotus-server 配置文件，默认 ./server.json
      --server <可执行>    lotus-server CLI 路径（默认取本程序同目录的 lotus-server.exe）
      --title <标题>       窗口标题
      --width <px>         窗口宽度，默认 1000
      --height <px>        窗口高度，默认 700
      --headless           不开窗口，仅启动本机服务（调试用）
      --debug              显示控制台与日志输出
  -h, --help               显示帮助`)
}

func main() {
	// --firewall-install：UAC 提权子进程入口（/ctl/firewall 的放行动作经 runas
	// 复用本程序，给 lotus-server CLI 添加防火墙放行），完成后直接退出。
	if firewall.HandleInstallArgs(os.Args[1:]) {
		return
	}
	fs2 := flag.NewFlagSet("lotus-server-dashboard", flag.ContinueOnError)
	cfgPath := fs2.String("config", "server.json", "")
	serverExe := fs2.String("server", "", "")
	title := fs2.String("title", "lotus-server 控制台", "")
	width := fs2.Int("width", 1000, "")
	height := fs2.Int("height", 700, "")
	headless := fs2.Bool("headless", false, "")
	debug := fs2.Bool("debug", false, "")
	fs2.Usage = usage
	if err := fs2.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}
	if *debug {
		setupDebugConsole()
	}

	cfgPathAbs, err := filepath.Abs(*cfgPath)
	if err != nil {
		fatalf(*debug, "配置路径无效: %v", err)
	}
	cfg, _, err := core.LoadConfig(cfgPathAbs)
	if err != nil {
		fatalf(*debug, "配置文件解析失败: %v", err)
	}
	exe := *serverExe
	if exe == "" {
		exe = defaultServerExe()
	}

	ctl, err := newController(cfgPathAbs, cfg, exe)
	if err != nil {
		fatalf(*debug, "%v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatalf(*debug, "本机端口监听失败: %v", err)
	}
	httpSrv := &http.Server{Handler: ctl.mux()}
	go func() { _ = httpSrv.Serve(ln) }()
	url := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)

	fmt.Println("lotus-server 桌面控制台已启动")
	fmt.Printf("  窗口地址: %s\n", url)
	fmt.Printf("  守护服务: %s（%s）\n", ctl.daemonBase, cfgPathAbs)
	fmt.Printf("  CLI:      %s\n", exe)

	shutdown := func() {
		_ = httpSrv.Close()
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
}

// defaultServerExe 本程序同目录的 lotus-server CLI（同目录分发约定）。
func defaultServerExe() string {
	self, err := os.Executable()
	if err != nil {
		return "lotus-server"
	}
	name := "lotus-server"
	if _, err := os.Stat(filepath.Join(filepath.Dir(self), name+".exe")); err == nil {
		return filepath.Join(filepath.Dir(self), name+".exe")
	}
	return filepath.Join(filepath.Dir(self), name)
}

func fatalf(debug bool, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args)
	if !debug {
		showErrorMessageBox(format, args...)
	}
	os.Exit(1)
}

// lotus：pic 的 Go 重构版服务器入口（单二进制；原二进制名 picd 已更名 lotus）。
//
// 用法:
//	lotus [选项] <根路径1> [根路径2 ...]
//
// 不传路径时读取 config.json（命令行参数优先）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"lotus/internal/api"
	"lotus/internal/app"
	"lotus/internal/core"
)

func usage() {
	fmt.Println(`图片浏览服务（lotus / picnet）

用法:
  lotus [选项] <路径1> [路径2 ...]

选项:
  -p, --port <端口>        HTTP 端口，默认 3466（服务端 lotus-server 用 3456，两者分离）
      --host <地址>        HTTP 监听地址，默认 127.0.0.1（局域网访问用 0.0.0.0）
      --quic-port <端口>   节点互联 QUIC/UDP 端口，默认 3467（服务端用 3457）
      --config <文件>      配置文件路径，默认 ./config.json
      --no-mdns            关闭局域网自动发现
      --no-auto-join       [测试] 关闭全部自动入网（本机服务 + 局域网自动发现）
      --delay-ms <毫秒>    [测试] 节点流 I/O 模拟延迟
      --fresh-sec <秒>     [测试] 在线新鲜度窗口
  -h, --help               显示帮助

不传路径时读取 config.json（命令行参数优先）:
  { "port": 3466, "host": "127.0.0.1", "paths": ["C:\\\\图片"], "net": { ... } }

示例:
  lotus C:\\图片
  lotus -p 8080 "D:\\壁纸" "E:\\相册"`)
}

func main() {
	fs := flag.NewFlagSet("lotus", flag.ContinueOnError)
	port := fs.Int("p", 0, "")
	_port := fs.Int("port", 0, "")
	host := fs.String("host", "", "")
	quicPort := fs.Int("quic-port", 0, "")
	cfgPath := fs.String("config", "config.json", "")
	noMDNS := fs.Bool("no-mdns", false, "")
	noAutoJoin := fs.Bool("no-auto-join", false, "")
	delayMS := fs.Int("delay-ms", 0, "")
	freshSec := fs.Int("fresh-sec", 0, "")
	fs.Usage = usage
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}
	if fs.NArg() == 0 && *_port == 0 && *port == 0 && *host == "" {
		// 完全无参数且无 config.json 时显示用法
		if _, err := os.Stat(*cfgPath); os.IsNotExist(err) {
			usage()
			os.Exit(1)
		}
	}

	lvl := slog.LevelInfo
	if os.Getenv("PIC_LOG") == "debug" {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(app.NewMdnsSpamFilter(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))))
	log := slog.Default()

	cfg, _, err := core.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 配置文件解析失败: %v\n", err)
		os.Exit(1)
	}
	cfgPathAbs, _ := filepath.Abs(*cfgPath)

	// 命令行参数 > config.json > 默认
	p := *port
	if p == 0 {
		p = *_port
	}
	if p != 0 {
		cfg.Port = p
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *quicPort != 0 {
		cfg.Net.Port = *quicPort
	}
	if fs.NArg() > 0 {
		cfg.Paths = fs.Args()
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
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
	for _, w := range a.Warns {
		fmt.Fprintln(os.Stderr, w)
	}
	if len(cfg.Paths) == 0 || len(a.Lib().Roots()) == 0 {
		fmt.Fprintln(os.Stderr, "错误: 没有可用的根路径")
		usage()
		os.Exit(1)
	}

	// 持久化一次（uplinks→hubs 迁移、新增默认字段落盘）
	_ = core.SaveConfig(cfgPathAbs, cfg)

	srv := api.New(a)
	httpServer := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler: srv.Handler(),
	}

	ln, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "listen" {
			fmt.Fprintf(os.Stderr, "错误: 端口 %d 已被占用，请用 --port 换一个端口\n", cfg.Port)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "错误: 监听失败: %v\n", err)
		os.Exit(1)
	}

	shown := cfg.Host
	if cfg.Host == "0.0.0.0" || cfg.Host == "" {
		shown = "localhost"
	}
	fmt.Println("图片浏览服务已启动")
	fmt.Printf("  打开: http://%s:%d/\n", shown, cfg.Port)
	fmt.Printf("  节点: %s（QUIC 端口 %d）\n", a.PeerID, cfg.Net.Port)
	for _, r := range a.Lib().Roots() {
		fmt.Printf("  根路径: %s\n", r.Dir)
	}

	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP 服务异常退出", "err", err)
			os.Exit(1)
		}
	}()

	// 优雅停机：SIGINT/SIGTERM → 停止接收、落盘缓存索引、关停网络
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\n正在退出…")
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = httpServer.Shutdown(shCtx)
	cancel()
	a.Close()
	fmt.Println("已退出")
}

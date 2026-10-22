package main

// dashboard 宿主的本地 HTTP：静态页面 + daemon 反向代理 + 本机控制端点。
// 前端一律走相对路径（同源），token 只在本进程内使用，不进 JS。

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"lotus/internal/core"
	"lotus/internal/firewall"
)

type controller struct {
	cfgPath string
	exe     string
	dist    fs.FS
	client  *http.Client

	mu         sync.Mutex
	daemonBase string
	token      string
}

func newController(cfgPath string, cfg *core.Config, exe string) (*controller, error) {
	dist, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	c := &controller{
		cfgPath: cfgPath,
		exe:     exe,
		dist:    dist,
		client:  &http.Client{Timeout: 45 * time.Second}, // net/join 最坏要 ~30s（退避重试）
	}
	c.applyConfig(cfg)
	return c, nil
}

// applyConfig 重读 server.json 刷新 daemon 地址与 token——daemon 可能刚生成或
// 修改过配置。文件尚不存在时按服务端默认端口假设（daemon 首次 run 会写入 3456）。
func (c *controller) applyConfig(cfg *core.Config) {
	host := cfg.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c.mu.Lock()
	c.daemonBase = "http://" + net_JoinHostPort(host, cfg.Port)
	c.token = cfg.API.Token
	c.mu.Unlock()
}

// reloadCfg 每次代理/查询前调用（本地小文件，代价可忽略）。
func (c *controller) reloadCfg() {
	cfg, existed, err := core.LoadConfig(c.cfgPath)
	if err != nil {
		return
	}
	if !existed {
		cfg.Port = core.ServerHTTPPort
	}
	c.applyConfig(cfg)
}

func (c *controller) base() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.daemonBase, c.token
}

func net_JoinHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

func (c *controller) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/", c.handleStatic)
	m.HandleFunc("/p/", c.handleProxy)
	m.HandleFunc("/ctl/config", c.handleConfig)
	m.HandleFunc("/ctl/service", c.handleService)
	m.HandleFunc("/ctl/daemon/start", c.handleDaemonStart)
	m.HandleFunc("/ctl/browse", c.handleBrowse)
	m.HandleFunc("/ctl/firewall", c.handleFirewall)
	m.HandleFunc("/ctl/firewall/allow", c.handleFirewallAllow)
	return m
}

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

/* ---------------- 静态页面 ---------------- */

func (c *controller) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	if p == "/" || p == "." {
		p = "/index.html"
	}
	b, err := fs.ReadFile(c.dist, strings.TrimPrefix(p, "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case strings.HasSuffix(p, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(p, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(p, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	_, _ = w.Write(b)
}

/* ---------------- daemon 反向代理（/p/*） ---------------- */

func (c *controller) handleProxy(w http.ResponseWriter, r *http.Request) {
	c.reloadCfg()
	base, token := c.base()
	target := base + strings.TrimPrefix(r.URL.Path, "/p")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	if _, err := url.Parse(target); err != nil {
		writeJSON(w, 400, map[string]string{"error": "目标地址无效"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "请求构造失败: " + err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "服务未运行或不可达（" + err.Error() + "）"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

/* ---------------- 本机控制（/ctl/*） ---------------- */

func (c *controller) handleConfig(w http.ResponseWriter, r *http.Request) {
	c.reloadCfg()
	base, _ := c.base()
	installed, running := serviceState()
	writeJSON(w, 200, map[string]any{
		"cfgPath":    c.cfgPath,
		"daemonBase": base,
		"exe":        c.exe,
		"service":    map[string]bool{"installed": installed, "running": running},
	})
}

// handleService POST {action: install|uninstall|start|stop}
// Windows 经 UAC 提权调用同目录 lotus-server CLI（结果靠随后的状态轮询确认）。
func (c *controller) handleService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体错误")
		return
	}
	switch body.Action {
	case "install", "uninstall", "start", "stop":
	default:
		writeErr(w, 400, "未知操作: "+body.Action)
		return
	}
	msg, err := requestServiceAction(c.exe, body.Action, c.cfgPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": msg})
		return
	}
	// 给 SCM/systemctl 一点时间，回带最新状态
	time.Sleep(1500 * time.Millisecond)
	installed, running := serviceState()
	writeJSON(w, 200, map[string]any{
		"ok": true, "message": msg,
		"service": map[string]bool{"installed": installed, "running": running},
	})
}

// handleDaemonStart POST：前台拉起 daemon（不提权；开发/未注册服务时用）。
func (c *controller) handleDaemonStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	if err := spawnDaemon(c.exe, c.cfgPath); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "已启动服务进程（后台）"})
}

// handleBrowse POST：弹原生文件夹选择框（对话框关闭前请求阻塞）。
// 返回 {canceled:true} 或 {path:"…"}；失败带 error（前端回退手输路径）。
func (c *controller) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	dir, canceled, err := browseFolder()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": !canceled, "canceled": canceled, "path": dir})
}

// handleFirewall GET：Windows 防火墙对 lotus-server CLI（c.exe）的放行状态。
// 检测在本进程做（COM 只读，非提权可查）——daemon 没跑也能查，正是配置时机。
func (c *controller) handleFirewall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "只支持 GET")
		return
	}
	cfg, _, err := core.LoadConfig(c.cfgPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "配置文件解析失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, firewall.Check(c.exe, firewall.NeedsOf(cfg.Host, cfg.Port, cfg.Net.Port)))
}

// handleFirewallAllow POST：提权给 lotus-server CLI 添加防火墙放行（弹 UAC）。
// 复用本程序（在用户会话内）的 --firewall-install 子命令；结果由前端轮询确认。
func (c *controller) handleFirewallAllow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	self, err := os.Executable()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "取程序路径失败: " + err.Error()})
		return
	}
	if err := firewall.Elevate(self, c.exe, "lotus-server 入站放行", []string{"tcp", "udp"}); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "已请求提权，请在 UAC 弹窗确认"})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

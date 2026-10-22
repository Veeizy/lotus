package api

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lotus/internal/core"
	"lotus/internal/firewall"
)

func slogWarn(msg string) { slog.Warn(msg) }

// isWinAbs Windows 绝对路径判定（盘符或 UNC）。
// 不用 filepath.IsAbs：跨卷拼接场景下其行为不符合直觉。
const sepBack = byte(92)

func isWinAbs(p string) bool {
	if strings.HasPrefix(p, `\`) {
		return true
	}
	return len(p) >= 2 && p[1] == ':' && (len(p) == 2 || p[2] == sepBack || p[2] == '/')
}

// 桌面版扩展配置面（PROTOCOL §9-18）：图库根路径管理、网络缓存目录、
// 原生文件夹打开钩子。全部仅限本机（loopback）。

// OpenFolderHook 桌面版注入的原生「打开文件夹」实现；nil = 不支持。
var OpenFolderHook func(dir string) error

// FirewallAllowHook 桌面版注入的「防火墙一键放行」提权实现（弹 UAC，结果由前端
// 随后轮询状态确认）；nil = 不支持（如服务进程无法弹窗，CLI 版请用 dashboard）。
var FirewallAllowHook func() error

func (s *Server) apiConfig(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodGet && !isLoopback(r) {
		writeErr(w, 403, "只允许本机操作")
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeErr(w, 405, "只支持 GET")
			return
		}
		writeJSON(w, 200, map[string]any{
			"paths":      s.a.Cfg.Paths,
			"cacheDir":   s.a.CacheDirAbs(),
			"quicPort":   s.a.Cfg.Net.Port,
			"name":       s.a.Cfg.Net.Name,
			"fixedRoots": s.a.FixedRoots, // 移动端：根与缓存目录固定，前端隐藏编辑入口
			"writable":   isLoopback(r),  // 消费者模式判定：非本机客户端只读（管理入口收敛）
		})

	case "paths":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "只支持 POST")
			return
		}
		if s.a.FixedRoots {
			writeErr(w, 403, "移动端使用应用默认图库文件夹，不支持编辑")
			return
		}
		var body struct {
			Paths []string `json:"paths"`
		}
		if err := readBody(r, 256*1024, &body); err != nil {
			writeErr(w, 400, "请求体错误")
			return
		}
		cleaned := make([]string, 0, len(body.Paths))
		for _, p := range body.Paths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !isWinAbs(p) {
				p = filepath.Join(s.a.AppDir, p)
			}
			cleaned = append(cleaned, filepath.Clean(p))
		}
		for _, p := range cleaned {
			slog.Warn("paths 收到", "p", p, "abs", isWinAbs(p), "isabs", filepath.IsAbs(p))
		}
		warns := s.a.SetLibrary(cleaned)
		s.saveCfg()
		out := []map[string]any{}
		for _, rt := range s.a.Lib().Roots() {
			out = append(out, map[string]any{"name": rt.Name, "path": rt.Dir, "mtime": rt.Mtime})
		}
		writeJSON(w, 200, map[string]any{"ok": true, "warns": warns, "roots": out})

	case "cache-dir":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "只支持 POST")
			return
		}
		if s.a.FixedRoots {
			writeErr(w, 403, "移动端使用应用默认缓存文件夹，不支持编辑")
			return
		}
		var body struct {
			Dir string `json:"dir"`
		}
		if err := readBody(r, 64*1024, &body); err != nil {
			writeErr(w, 400, "请求体错误")
			return
		}
		dir := strings.TrimSpace(body.Dir)
		if dir == "" {
			writeErr(w, 400, "缺少 dir")
			return
		}
		if !isWinAbs(dir) {
			dir = filepath.Join(s.a.AppDir, dir)
		}
		dir = filepath.Clean(dir)
		if st, err := os.Stat(dir); err == nil && !st.IsDir() {
			writeErr(w, 400, "该路径不是文件夹")
			return
		}
		if err := s.a.SetCacheDir(dir); err != nil {
			writeErr(w, 500, "缓存目录切换失败: "+err.Error())
			return
		}
		s.saveCfg()
		writeJSON(w, 200, map[string]any{"ok": true, "cacheDir": dir})

	case "open-folder":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "只支持 POST")
			return
		}
		var body struct {
			Dir string `json:"dir"`
		}
		if err := readBody(r, 64*1024, &body); err != nil {
			writeErr(w, 400, "请求体错误")
			return
		}
		dir := strings.TrimSpace(body.Dir)
		if dir == "" || !filepath.IsAbs(dir) {
			writeErr(w, 400, "需要绝对路径")
			return
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			writeErr(w, 404, "文件夹不存在")
			return
		}
		if OpenFolderHook == nil {
			writeErr(w, 501, "仅桌面版支持打开文件夹")
			return
		}
		if err := OpenFolderHook(dir); err != nil {
			writeErr(w, 500, "打开失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})

	case "firewall":
		// Windows 防火墙状态（仅本机）：exe 路径含本机信息，不对局域网暴露
		if r.Method != http.MethodGet {
			writeErr(w, 405, "只支持 GET")
			return
		}
		if !isLoopback(r) {
			writeErr(w, 403, "只允许本机查询")
			return
		}
		self, err := os.Executable()
		if err != nil {
			writeErr(w, 500, "取程序路径失败: "+err.Error())
			return
		}
		writeJSON(w, 200, firewall.Check(self,
			firewall.NeedsOf(s.a.Cfg.Host, s.a.Cfg.Port, s.a.Cfg.Net.Port)))

	case "firewall-allow":
		if r.Method != http.MethodPost {
			writeErr(w, 405, "只支持 POST")
			return
		}
		if FirewallAllowHook == nil {
			writeErr(w, 501, "当前运行方式不支持一键放行（服务进程请在 dashboard 中操作）")
			return
		}
		if err := FirewallAllowHook(); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "message": "已请求提权，请在 UAC 弹窗确认"})

	default:
		writeErr(w, 404, "未找到")
	}
}

func (s *Server) saveCfg() {
	if err := core.SaveConfig(s.a.CfgPath, s.a.Cfg); err != nil {
		slogWarn("配置保存失败: " + err.Error())
	}
}

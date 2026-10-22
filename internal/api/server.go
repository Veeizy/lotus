// Package api：localhost HTTP 管理面（PROTOCOL §2）。
// 旧路径 /api/*（前端零改动依赖，逐字段冻结）+ 新前缀 /api/v1/*（token 鉴权 + SSE）。
package api

import (
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"lotus/internal/app"
	"lotus/internal/core"
	"lotus/web"
)

type Server struct {
	a   *app.App
	mux *http.ServeMux
}

// staticFS 可选静态资产（桌面版注入 TS 编译产物）；nil 时服务内嵌 web/index.html。
var staticFS fs.FS

// SetStaticAssets 注入静态资产根（含 index.html）。须在 New 之前调用。
func SetStaticAssets(f fs.FS) { staticFS = f }

func New(a *app.App) *Server {
	s := &Server{a: a, mux: http.NewServeMux()}
	s.mux.HandleFunc("/", s.handleStatic)
	s.mux.HandleFunc("/api/", s.handleAPI)                    // 冻结路径（永不鉴权）
	s.mux.HandleFunc("/api/v1/", s.auth(s.handleAPI))         // 新前缀（token 鉴权）
	return s
}

func (s *Server) Handler() http.Handler { return s.cors(s.mux) }

// cors 跨域支持（api.cors，默认开）：安卓消费端从应用内资产源（https://appassets.*）
// 直接读取本服务的 /api/*。仅作用于 API 面；写接口另有 loopback 门禁，不因 CORS 放开。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && s.a.Cfg.API.CorsEnabled() {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", "*")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				h.Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// auth /api/v1 token 鉴权（未配置 token 则放行）。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok := s.a.Cfg.API.Token; tok != "" {
			ok := r.Header.Get("Authorization") == "Bearer "+tok ||
				r.URL.Query().Get("token") == tok
			if !ok {
				writeErr(w, 401, "未授权")
				return
			}
		}
		next(w, r)
	}
}

/* ---------------- 通用工具 ---------------- */

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // 对齐 JS JSON.stringify（PROTOCOL §2.1）
	_ = enc.Encode(obj)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

/* ---------------- 静态资源 ---------------- */

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if staticFS != nil {
		// 桌面版资产（TS 编译产物）由 FileServer 托管（/ → index.html）。
		// 本地资产禁止缓存：升级 exe 后浏览器可能继续用旧 bundle（启发式缓存）。
		w.Header().Set("Cache-Control", "no-store")
		http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
		return
	}
	switch r.URL.Path {
	case "/", "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.Index)
	case "/favicon.ico":
		w.WriteHeader(204)
	default:
		writeErr(w, 404, "未找到")
	}
}

/* ---------------- API 分发 ---------------- */

// apiSuffix 归一化路径：/api/xxx 与 /api/v1/xxx 返回相同的 suffix。
func apiSuffix(p string) string {
	p = path.Clean(p)
	for _, pre := range []string{"/api/v1", "/api"} {
		if strings.HasPrefix(p, pre+"/") {
			return "/" + strings.TrimPrefix(p, pre+"/")
		}
	}
	return ""
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	suffix := apiSuffix(r.URL.Path)
	isV1 := strings.HasPrefix(r.URL.Path, "/api/v1/")

	// 非 /api/net/（与 /api/v1/net）路径仅允许 GET/HEAD
	isNet := strings.HasPrefix(suffix, "/net") || strings.HasPrefix(suffix, "/events") ||
		strings.HasPrefix(suffix, "/v1/net")
	_ = isNet
	if !strings.HasPrefix(suffix, "/net") && !strings.HasPrefix(suffix, "/events") &&
		!strings.HasPrefix(suffix, "/config") && !strings.HasPrefix(suffix, "/desktop/") &&
		!strings.HasPrefix(suffix, "/admin") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, 405, "只支持 GET")
			return
		}
	}

	switch {
	case suffix == "/roots":
		s.apiRoots(w, r)
	case suffix == "/browse":
		s.apiBrowse(w, r)
	case suffix == "/times":
		s.apiTimes(w, r)
	case suffix == "/image":
		s.apiImage(w, r)
	case strings.HasPrefix(suffix, "/net/"):
		s.apiNet(w, r, strings.TrimPrefix(suffix, "/net/"))
	case suffix == "/config" || strings.HasPrefix(suffix, "/config/"):
		action := strings.TrimPrefix(suffix, "/config")
		action = strings.TrimPrefix(action, "/")
		s.apiConfig(w, r, action)
	case suffix == "/desktop/open-folder":
		s.apiConfig(w, r, "open-folder")
	case suffix == "/desktop/firewall":
		s.apiConfig(w, r, "firewall")
	case suffix == "/desktop/firewall/allow":
		s.apiConfig(w, r, "firewall-allow")
	case isV1 && suffix == "/events":
		s.apiEvents(w, r)
	case isV1 && strings.HasPrefix(suffix, "/admin/"):
		s.apiAdmin(w, r, strings.TrimPrefix(suffix, "/admin/"))
	default:
		if strings.HasPrefix(suffix, "/net") || suffix == "/events" || strings.HasPrefix(suffix, "/v1") {
			writeErr(w, 404, "未找到网络接口")
			return
		}
		writeErr(w, 404, "未找到")
	}
}

/* ---------------- 本地浏览端点（§2.2，冻结） ---------------- */

func (s *Server) apiRoots(w http.ResponseWriter, r *http.Request) {
	roots := []map[string]any{}
	for _, rt := range s.a.Lib().Roots() {
		roots = append(roots, map[string]any{
			"name": rt.Name, "path": rt.Dir, "mtime": rt.Mtime,
			"service": rt.Service, // 本机服务的根（🖥 图标区分；浏览与出图直接读盘）
		})
	}
	writeJSON(w, 200, map[string]any{"roots": roots})
}

func (s *Server) apiBrowse(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeErr(w, 400, "缺少 path 参数")
		return
	}
	bd, err := s.a.Lib().Browse(p)
	if err != nil {
		writeBrowseErr(w, err)
		return
	}
	writeJSON(w, 200, bd)
}

func (s *Server) apiTimes(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeErr(w, 400, "缺少 path 参数")
		return
	}
	times, err := s.a.Lib().Times(p)
	if err != nil {
		writeBrowseErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"times": times})
}

func (s *Server) apiImage(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	if p == "" {
		writeErr(w, 400, "缺少 p 参数")
		return
	}
	realPath, _, err := s.a.Lib().CheckReadable(filepath.Clean(p))
	if err != nil {
		writeErr(w, 403, "路径不在配置的根目录内")
		return
	}
	streamFile(w, realPath)
}

// streamFile 按缓存头流式出图（本地与远程共用）。
func streamFile(w http.ResponseWriter, realPath string) {
	st, err := os.Stat(realPath)
	if err != nil || !st.Mode().IsRegular() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(404)
		_, _ = w.Write([]byte("Not Found"))
		return
	}
	ct := core.MIME[strings.ToLower(filepath.Ext(realPath))]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(200)
	f, err := os.Open(realPath)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

func writeBrowseErr(w http.ResponseWriter, err error) {
	switch {
	case err == core.ErrOutsideRoots:
		writeErr(w, 403, err.Error())
	case err == core.ErrNotFound:
		writeErr(w, 404, err.Error())
	default:
		writeErr(w, 500, err.Error())
	}
}

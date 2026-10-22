package api

import (
	"net/http"
	"strconv"
	"strings"
)

/* ---------------- /api/v1/admin/* 运行观测（lotus-server 查询面） ----------------
 * 访问记录 / 流量 / 服务时间。仅限本机（loopback）+ /api/v1 token 鉴权
 * （server.json 的 api.token 未设置则仅 loopback 保护）。 */

func (s *Server) apiAdmin(w http.ResponseWriter, r *http.Request, action string) {
	if !isLoopback(r) {
		writeErr(w, 403, "只允许本机操作")
		return
	}
	if s.a.Net == nil || s.a.Stats == nil {
		writeErr(w, 500, "加密模块异常，网络功能不可用")
		return
	}
	switch action {
	case "status":
		rep := s.a.Stats.StatusSnapshot()
		snap := s.a.Net.Snapshot()
		members := []map[string]any{}
		for _, mem := range snap.Members {
			members = append(members, map[string]any{
				"id": mem.ID, "name": mem.Name, "addr": mem.Addr, "direct": mem.Direct,
			})
		}
		uplinks := []map[string]any{}
		for _, u := range snap.Uplinks {
			uplinks = append(uplinks, map[string]any{"addr": u.Addr, "status": u.Status})
		}
		roots := []map[string]any{}
		for _, rt := range s.a.Lib().Roots() {
			roots = append(roots, map[string]any{"name": rt.Name, "path": rt.Dir, "mtime": rt.Mtime})
		}
		writeJSON(w, 200, map[string]any{
			"ok":        true,
			"app":       "picnet",
			"id":        s.a.PeerID,
			"name":      s.a.Cfg.Net.Name,
			"startedAt": rep.StartedAt,
			"uptimeSec": rep.UptimeSec,
			"httpPort":  s.a.Cfg.Port,
			"quicPort":  s.a.Cfg.Net.Port,
			"folders":   roots,
			"joined":    snap.Joined,
			"hub":       snap.Hub,
			"uplinks":   uplinks,
			"members":   members,
			"requests":  rep.Requests,
			"kinds":     rep.Kinds,
			"traffic":   rep.Traffic,
		})

	case "traffic":
		writeJSON(w, 200, s.a.Stats.TrafficSnapshot())

	case "access":
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "entries": s.a.Stats.AccessSnapshot(limit)})

	case "hub/remove":
		// lotus-server CLI 逐网络离开：移除单个 hub 并断开（其余连接保持）
		if r.Method != http.MethodPost {
			writeErr(w, 405, "只支持 POST")
			return
		}
		var body struct {
			Addr string `json:"addr"`
		}
		if err := readBody(r, 64*1024, &body); err != nil {
			writeErr(w, 400, "请求体错误")
			return
		}
		addr := strings.TrimSpace(body.Addr)
		if addr == "" {
			writeErr(w, 400, "缺少 addr")
			return
		}
		if !s.a.Net.RemoveHub(addr) {
			writeErr(w, 404, "该地址不在已加入的网络列表中")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})

	default:
		writeErr(w, 404, "未找到管理接口")
	}
}

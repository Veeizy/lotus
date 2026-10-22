package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"time"

	"lotus/internal/core"
	"lotus/internal/node"
)

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

/* ---------------- /api/net/* 管理面（§2.3，冻结） ---------------- */

func (s *Server) apiNet(w http.ResponseWriter, r *http.Request, action string) {
	// 加密自检失败的统一入口行为
	if s.a.Net == nil {
		writeErr(w, 500, "加密模块异常，网络功能不可用")
		return
	}
	net := s.a.Net

	// 方法约束：写操作必须 POST
	needsPost := map[string]bool{"join": true, "leave": true, "settings": true,
		"prefetch": true, "cancel": true}
	if needsPost[action] && r.Method != http.MethodPost {
		writeErr(w, 404, "未找到网络接口")
		return
	}
	loopOnly := map[string]bool{"join": true, "leave": true, "settings": true}
	if loopOnly[action] && !isLoopback(r) {
		writeErr(w, 403, "只允许本机操作")
		return
	}

	switch action {
	case "info":
		// 引导端点（跨机可达）：仅暴露公开信息（等同 attestation），供 join 探测身份
		if s.a.Net == nil {
			writeErr(w, 500, "加密模块异常，网络功能不可用")
			return
		}
		writeJSON(w, 200, map[string]any{
			"app": "picnet", "v": 2,
			"id":      s.a.PeerID,
			"name":    s.a.Cfg.Net.Name,
			"quicPort": s.a.Cfg.Net.Port,
		})

	case "state":
		snap := net.Snapshot()
		secret := ""
		if isLoopback(r) {
			secret = s.a.ID.Secret
		}
		me := map[string]any{
			"id":        s.a.PeerID,
			"name":      s.a.Cfg.Net.Name,
			"publicKey": publicKeyB64(s),
		}
		if secret != "" {
			me["secret"] = secret
		}
		cached := []map[string]any{}
		exclude := map[string]bool{s.a.PeerID: true}
		for _, mem := range snap.Members {
			exclude[mem.ID] = true
		}
		for _, cn := range s.a.Cache().CachedNodes(exclude) {
			cached = append(cached, map[string]any{
				"id": cn.ID, "name": cn.Name, "files": cn.Files, "lastSeen": cn.LastSeen,
				"local": s.a.Net.IsLocalService(cn.ID),
			})
		}
		visited := s.visitedRoots(&snap)
		uplinks := []map[string]any{}
		for _, u := range snap.Uplinks {
			uplinks = append(uplinks, map[string]any{"addr": u.Addr, "status": u.Status})
		}
		members := []map[string]any{}
		for _, mem := range snap.Members {
			members = append(members, map[string]any{
				"id": mem.ID, "name": mem.Name, "addr": mem.Addr, "direct": mem.Direct,
				"local": s.a.Net.IsLocalService(mem.ID),
			})
		}
		writeJSON(w, 200, map[string]any{
			"ok":          true,
			"me":          me,
			"share":       snap.Share,
			"cacheServe":  snap.CacheServe,
			"concurrency": snap.Concurrency,
			"uplinks":     uplinks,
			"members":     members,
			"cached":      cached,
			"visited":     visited,
			"hub":         snap.Hub,
			"joined":      snap.Joined,
			"lanDiscovery": snap.LanDiscovery,
			"scanPorts":    snap.ScanPorts,
			"libVersion":   s.a.LibVersion(), // 本地图库版本（服务根同步后前端自动刷新根视图）
			"hostLoopback": s.a.Cfg.Host == "127.0.0.1" || s.a.Cfg.Host == "::1" || s.a.Cfg.Host == "localhost",
		})

	case "discover":
		discovered := []map[string]any{}
		for _, d := range net.Discovered() {
			discovered = append(discovered, map[string]any{
				"id": d.ID, "name": d.Name, "addr": d.Addr, "ts": d.TS, "known": d.Known,
			})
		}
		writeJSON(w, 200, map[string]any{"ok": true, "discovered": discovered})

	case "join":
		var body struct {
			Addrs []string `json:"addrs"`
		}
		if err := readBody(r, 64*1024, &body); err != nil {
			writeErr(w, 400, "请求体错误")
			return
		}
		results := net.Join(body.Addrs)
		out := []map[string]any{}
		for _, res := range results {
			item := map[string]any{"addr": res.Addr, "ok": res.OK}
			if res.Node != "" {
				item["node"] = res.Node
			}
			if res.Error != "" {
				item["error"] = res.Error
			}
			if res.Note != "" {
				item["note"] = res.Note
			}
			out = append(out, item)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "results": out})

	case "leave":
		net.Leave()
		writeJSON(w, 200, map[string]any{"ok": true})

	case "settings":
		var body struct {
			Name         *string `json:"name"`
			Share        *bool   `json:"share"`
			CacheServe   *bool   `json:"cacheServe"`
			Concurrency  *int    `json:"concurrency"`
			Advertise    *string `json:"advertise"`
			LanDiscovery *bool   `json:"lanDiscovery"`
			ScanPorts    []int   `json:"scanPorts"`
		}
		if err := readBody(r, 64*1024, &body); err != nil {
			writeErr(w, 400, "请求体错误")
			return
		}
		net.UpdateSettings(node.SettingsUpdate{
			Name: body.Name, Share: body.Share, CacheServe: body.CacheServe,
			Concurrency: body.Concurrency, Advertise: body.Advertise,
			LanDiscovery: body.LanDiscovery, ScanPorts: body.ScanPorts,
		})
		writeJSON(w, 200, map[string]any{"ok": true})

	case "lan-discover":
		// 手动触发局域网发现并入网（与自动周期同一实现）
		if r.Method != http.MethodPost {
			writeErr(w, 405, "只支持 POST")
			return
		}
		if !isLoopback(r) {
			writeErr(w, 403, "只允许本机操作")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		results := net.DiscoverAndJoinLAN(ctx)
		out := []map[string]any{}
		for _, res := range results {
			out = append(out, map[string]any{
				"id": res.ID, "addr": res.Addr, "name": res.Name, "via": res.Via,
				"ok": res.OK, "error": res.Error,
			})
		}
		writeJSON(w, 200, map[string]any{"ok": true, "results": out})

	case "browse":
		nodeID := r.URL.Query().Get("node")
		if nodeID == "" {
			writeErr(w, 400, "缺少 node 参数")
			return
		}
		rpath := r.URL.Query().Get("path")
		s.netBrowse(w, nodeID, rpath)

	case "times":
		nodeID := r.URL.Query().Get("node")
		rpath := r.URL.Query().Get("path")
		s.netTimes(w, nodeID, rpath)

	case "image":
		nodeID := r.URL.Query().Get("node")
		if nodeID == "" {
			writeErr(w, 400, "缺少 node 参数")
			return
		}
		rpath := r.URL.Query().Get("p")
		s.netImage(w, nodeID, rpath)

	case "prefetch":
		var body struct {
			Node   string   `json:"node"`
			Folder string   `json:"folder"`
			Paths  []string `json:"paths"`
		}
		if err := readBody(r, 4*1024*1024, &body); err != nil {
			writeErr(w, 400, "请求体过大")
			return
		}
		// 本机服务：内容在本地盘，无需预取（进度记为完成，避免前端反复触发）
		if s.a.Net.IsLocalService(body.Node) {
			s.a.Queue.SetStats(body.Node+"|"+body.Folder,
				core.FolderStat{Total: len(body.Paths), Done: len(body.Paths)})
			writeJSON(w, 200, map[string]any{"ok": true, "total": len(body.Paths), "cached": len(body.Paths)})
			return
		}
		if !s.peerReachable(body.Node) {
			writeErr(w, 404, "节点不在网络中")
			return
		}
		total, cached := net.Prefetch(body.Node, body.Folder, body.Paths)
		writeJSON(w, 200, map[string]any{"ok": true, "total": total, "cached": cached})

	case "cancel":
		var body struct {
			Node   string `json:"node"`
			Folder string `json:"folder"`
		}
		if err := readBody(r, 64*1024, &body); err != nil {
			writeErr(w, 400, "请求体错误")
			return
		}
		net.CancelFolder(body.Node, body.Folder)
		writeJSON(w, 200, map[string]any{"ok": true})

	case "progress":
		nodeID := r.URL.Query().Get("node")
		folder := r.URL.Query().Get("folder")
		st := net.FolderProgress(nodeID, folder)
		writeJSON(w, 200, map[string]any{"total": st.Total, "done": st.Done, "failed": st.Failed})

	default:
		writeErr(w, 404, "未找到网络接口")
	}
}

// visitedRootInfo 汇总所有节点已访问过的对方根（本地根视图「永久可见」的数据源）。
type visitedRootInfo struct {
	Node     string `json:"node"`
	NodeName string `json:"nodeName"`
	Root     string `json:"root"`
	RootName string `json:"rootName"`
	Files    int    `json:"files"`
	Online   bool   `json:"online"`
	Local    bool   `json:"local"` // 本机服务（同机 lotus-server）：前端归入「本机」分区
}

func (s *Server) visitedRoots(snap *node.SnapshotData) []visitedRootInfo {
	online := map[string]bool{}
	names := map[string]string{}
	for _, m := range snap.Members {
		online[m.ID] = true
		if m.Name != "" {
			names[m.ID] = m.Name
		}
	}
	out := []visitedRootInfo{}
	for _, id := range s.a.Cache().NodeIDs() {
		if id == s.a.PeerID {
			continue
		}
		idx := s.a.Cache().Lookup(id)
		if idx == nil {
			continue
		}
		name := names[id]
		if name == "" {
			name = idx.Name
		}
		for _, vr := range s.a.Cache().VisitedRoots(id) {
			out = append(out, visitedRootInfo{
				Node: id, NodeName: name, Root: vr.Path,
				RootName: filepath.Base(vr.Path), Files: vr.Files,
				Online: online[id],
				Local:  s.a.Net.IsLocalService(id),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeName != out[j].NodeName {
			return out[i].NodeName < out[j].NodeName
		}
		return out[i].RootName < out[j].RootName
	})
	return out
}

func publicKeyB64(s *Server) string {
	// 明钥 = X25519 公钥（b64url 无填充）
	return b64u(s.a.ID.PubX)
}

// peerReachable 目标在线（成员表/连接判定；转发候选不算在线，PROTOCOL §2.3 的 404 语义）。
func (s *Server) peerReachable(nodeID string) bool {
	return s.a.Net.IsOnline(nodeID)
}

func (s *Server) netBrowse(w http.ResponseWriter, nodeID, rpath string) {
	// 本机服务：图库已并入（service 根），一律本地出——不经网络、不写任何缓存
	if s.a.Net.IsLocalService(nodeID) {
		s.netBrowseLocal(w, nodeID, rpath)
		return
	}
	// 离线快路径：目标不在线时不发起拨号（避免无谓的超时等待），直接回退缓存/404
	if !s.a.Net.IsOnline(nodeID) {
		s.offlineOrError(w, nodeID, rpath, nil, false)
		return
	}
	ctx, cancel := withTimeout(20 * time.Second)
	defer cancel()
	if rpath == "" {
		// 远程 roots 视图
		head, err := s.a.Net.RequestRoots(ctx, nodeID)
		if err != nil || !head.OK {
			s.offlineOrError(w, nodeID, "", err, true)
			return
		}
		var data struct {
			Roots []struct {
				Name  string `json:"name"`
				Path  string `json:"path"`
				Mtime int64  `json:"mtime"`
			} `json:"roots"`
		}
		_ = json.Unmarshal(head.Data, &data)
		folders := []map[string]any{}
		for _, rt := range data.Roots {
			folders = append(folders, map[string]any{"name": rt.Name, "path": rt.Path, "mtime": rt.Mtime})
		}
		name := nodeID
		if att := s.a.Net.AttestationOf(nodeID); att != nil && att.Name != "" {
			name = att.Name
		}
		out := map[string]any{
			"name": name, "path": "", "parent": nil,
			"folders": folders, "images": []any{}, "breadcrumbs": []any{},
		}
		s.cacheListing(nodeID, rpath, out)
		writeJSON(w, 200, out)
		return
	}
	head, err := s.a.Net.RequestBrowse(ctx, nodeID, rpath)
	if err != nil || !head.OK {
		s.offlineOrError(w, nodeID, rpath, err, true)
		return
	}
	var bd core.BrowseData
	if err := json.Unmarshal(head.Data, &bd); err != nil {
		s.offlineOrError(w, nodeID, rpath, err, true)
		return
	}
	s.cacheListing(nodeID, rpath, &bd)
	writeJSON(w, 200, &bd)
}

// netBrowseLocal 本机服务的浏览：从本地图库出（service 根已并入）。
// 不发起远程请求，也不写目录缓存（cacheListing 仅用于真正的远程节点）。
func (s *Server) netBrowseLocal(w http.ResponseWriter, nodeID, rpath string) {
	if rpath == "" {
		name := "本机服务"
		if att := s.a.Net.AttestationOf(nodeID); att != nil && att.Name != "" {
			name = att.Name
		}
		folders := []map[string]any{}
		for _, rt := range s.a.Lib().Roots() {
			if rt.Service {
				folders = append(folders, map[string]any{"name": rt.Name, "path": rt.Dir, "mtime": rt.Mtime})
			}
		}
		writeJSON(w, 200, map[string]any{
			"name": name, "path": "", "parent": nil,
			"folders": folders, "images": []any{}, "breadcrumbs": []any{},
		})
		return
	}
	bd, err := s.a.Lib().Browse(rpath)
	if err != nil {
		writeBrowseErr(w, err)
		return
	}
	writeJSON(w, 200, bd)
}

// cacheListing 在线浏览结果写入缓存索引（离线浏览与目录缓存，fire-and-forget）。
func (s *Server) cacheListing(nodeID, rpath string, data any) {
	go func() {
		idx := s.a.Cache().Get(nodeID)
		idx.LastSeen = time.Now().UnixMilli()
		if att := s.a.Net.AttestationOf(nodeID); att != nil && att.Name != "" {
			idx.Name = att.Name
		}
		bd, ok := data.(*core.BrowseData)
		if !ok {
			m := data.(map[string]any)
			b, _ := json.Marshal(m)
			bd = &core.BrowseData{}
			_ = json.Unmarshal(b, bd)
		}
		// 以对方根为根：记录该目录所属根（镜像树与「访问过的根」的数据源）
		root := ""
		if len(bd.Breadcrumbs) > 0 {
			root = bd.Breadcrumbs[0].Path
		}
		idx.Listings[rpath] = &core.Listing{TS: time.Now().UnixMilli(), Root: root, Data: bd}
		s.a.Cache().Mark(nodeID)
	}()
}

// offlineOrError 在线失败（或目标已离线）→ 回退本地缓存列表（offline:true）；
// 无缓存时按在线/离线分别报错（离线 404 文案为冻结契约，PROTOCOL §2.3）。
func (s *Server) offlineOrError(w http.ResponseWriter, nodeID, rpath string, err error, online bool) {
	if idx := s.a.Cache().Lookup(nodeID); idx != nil {
		if hit, ok := idx.Listings[rpath]; ok && hit.Data != nil {
			out := *hit.Data
			out.Offline = true
			writeJSON(w, 200, &out)
			return
		}
	}
	if !online {
		writeErr(w, 404, "节点不在网络中，且本地没有该目录的缓存")
		return
	}
	if err == nil {
		// 在线但对方返回错误
		writeErr(w, 502, "对方返回错误")
		return
	}
	if nodeID == "" {
		writeErr(w, 400, "缺少 node 参数")
		return
	}
	writeErr(w, 502, "访问失败: "+err.Error())
}

func (s *Server) netTimes(w http.ResponseWriter, nodeID, rpath string) {
	// 本机服务：本地读（图库已并入），不走远程
	if s.a.Net.IsLocalService(nodeID) {
		times, err := s.a.Lib().Times(rpath)
		if err != nil {
			writeBrowseErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"times": times})
		return
	}
	if !s.peerReachable(nodeID) {
		writeErr(w, 404, "节点不在网络中")
		return
	}
	ctx, cancel := withTimeout(20 * time.Second)
	defer cancel()
	head, err := s.a.Net.RequestTimes(ctx, nodeID, rpath)
	if err != nil || !head.OK {
		msg := "访问失败: 对方返回错误"
		if err != nil {
			msg = "访问失败: " + err.Error()
		}
		writeErr(w, 502, msg)
		return
	}
	var data struct {
		Times []core.Entry `json:"times"`
	}
	_ = json.Unmarshal(head.Data, &data)
	writeJSON(w, 200, map[string]any{"times": data.Times})
}

func (s *Server) netImage(w http.ResponseWriter, nodeID, rpath string) {
	if rpath == "" {
		writeErr(w, 400, "缺少 p 参数")
		return
	}
	// 本机服务：内容就在本地盘（图库已并入），任何入口都直接读文件——
	// 不经网络、不缓存（含网络面板点击等全部 UI 路径）
	if s.a.Net.IsLocalService(nodeID) {
		realPath, _, err := s.a.Lib().CheckReadable(filepath.Clean(rpath))
		if err != nil {
			writeErr(w, 404, "文件不存在")
			return
		}
		streamFile(w, realPath)
		return
	}
	folderKey := nodeID + "|" + filepath.Dir(rpath)
	// 缓存命中即直接出图（即使节点在线，不回源核对）
	cachePath := s.cachePathOf(nodeID, rpath)
	slog.Debug("netImage", "node", shortID(nodeID), "cacheHit", cachePath != "")
	if cachePath == "" {
		if !s.peerReachable(nodeID) {
			writeErr(w, 404, "节点不在网络中且本地无缓存")
			return
		}
		var err error
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer cancel()
		cachePath, err = s.a.Net.EnsureImage(ctx, nodeID, rpath, folderKey, true)
		if err != nil {
			writeErr(w, 502, "下载失败: "+err.Error())
			return
		}
	}
	streamFile(w, cachePath)
}

func (s *Server) cachePathOf(nodeID, rpath string) string {
	p := s.a.Cache().PathFor(nodeID, rpath)
	if st, err := osStat(p); err == nil && st.Mode().IsRegular() {
		return p
	}
	return ""
}

/* ---------------- SSE 事件（/api/v1/events，§2.4 新增） ---------------- */

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "流式响应不可用")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	fmtFprintf(w, "retry: 3000\n\n")
	fl.Flush()

	ch := s.a.Events.Subscribe()
	defer s.a.Events.Unsubscribe(ch)
	ticker := newTicker(15 * time.Second)
	defer ticker.Stop()
	// 建立连接即推送一次成员快照
	s.sendMembers(w, fl)
	lastProgress := map[string][3]int{}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			switch ev.Kind {
			case "members":
				s.sendMembers(w, fl)
			case "progress":
				// 全量对账（节流由 5s 维护循环天然保证）
				for key, st := range s.a.Queue.AllStats() {
					cur := [3]int{st.Total, st.Done, st.Failed}
					if lastProgress[key] != cur {
						lastProgress[key] = cur
						nodeID, folder := splitFolderKey(key)
						fmtFprintf(w, "event: progress\ndata: {\"node\":%q,\"folder\":%q,\"total\":%d,\"done\":%d,\"failed\":%d}\n\n",
							nodeID, folder, st.Total, st.Done, st.Failed)
						fl.Flush()
					}
				}
			}
		case <-ticker.C:
			fmtFprintf(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) sendMembers(w http.ResponseWriter, fl http.Flusher) {
	snap := s.a.Net.Snapshot()
	members := []map[string]any{}
	for _, m := range snap.Members {
		members = append(members, map[string]any{
			"id": m.ID, "name": m.Name, "addr": m.Addr, "direct": m.Direct,
		})
	}
	b, _ := json.Marshal(map[string]any{
		"joined": snap.Joined, "hub": snap.Hub, "members": members,
	})
	fmtFprintf(w, "event: members\ndata: %s\n\n", b)
	fl.Flush()
}

func splitFolderKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

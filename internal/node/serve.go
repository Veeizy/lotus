package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"

	"lotus/internal/core"
	"lotus/internal/crypto"
	"lotus/internal/p2p"
	"lotus/internal/stats"
)

/* ---------------- 节点间协议的公共结构（PROTOCOL §3.2） ---------------- */

type NodeRequest struct {
	Path   string   `json:"path,omitempty"`
	PubX   string   `json:"pubX,omitempty"`
	Target string   `json:"target,omitempty"`
	Via    []string `json:"via,omitempty"` // 途径节点表（环路防护：发起方写入自身，每级转发追加自身，PROTOCOL §9-11）
}

// NodeRespHead image 应答的 JSON 头；其余协议复用 Data 字段。
type NodeRespHead struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	PlainSize int64           `json:"plainSize,omitempty"`
	MtimeMs   int64           `json:"mtimeMs,omitempty"`
	Sha256    string          `json:"sha256,omitempty"`
	Ext       string          `json:"ext,omitempty"`
	Receipt   *crypto.Receipt `json:"receipt,omitempty"`
	Stale     bool            `json:"stale,omitempty"`
}

func errResp(msg string) *NodeRespHead { return &NodeRespHead{OK: false, Error: msg} }

/* ---------------- 服务端：简单协议（roots/browse/times/stat） ---------------- */

// protoShort "/picnet/browse/1" → "browse"（观测日志用类型名）。
func protoShort(proto string) string {
	return strings.TrimSuffix(strings.TrimPrefix(proto, "/picnet/"), "/1")
}

func (m *Manager) serveSimple(proto string) func(network.Stream) {
	return func(s network.Stream) {
		defer s.Close()
		s = m.countWrap(s)
		start := time.Now()
		peerID := s.Conn().RemotePeer().String()
		ev := stats.AccessEvent{Kind: protoShort(proto), Peer: peerID, OK: true}
		defer func() {
			ev.DurMS = time.Since(start).Milliseconds()
			m.logAccess(ev)
		}()
		raw, err := p2p.ReadMsg(s, 0)
		if err != nil {
			return
		}
		var req NodeRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			ev.OK, ev.Extra = false, "请求格式错误"
			writeHead(s, errResp("请求格式错误"), m.delay)
			return
		}
		ev.Path = req.Path
		if !m.shareEnabled() {
			ev.OK, ev.Extra = false, "对方未开放图库访问"
			writeHead(s, errResp("对方未开放图库访问"), m.delay)
			return
		}
		switch proto {
		case ProtoRoots:
			// {"ok":true,"data":{"roots":[{name,path,mtime}]}}（PROTOCOL §3.2）
			roots := []map[string]any{}
			for _, rt := range m.lib().Roots() {
				roots = append(roots, map[string]any{"name": rt.Name, "path": rt.Dir, "mtime": rt.Mtime})
			}
			data, _ := json.Marshal(map[string]any{"roots": roots})
			writeHead(s, &NodeRespHead{OK: true, Data: data}, m.delay)
		case ProtoBrowse:
			bd, err := m.lib().Browse(req.Path)
			if err != nil {
				ev.OK, ev.Extra = false, serveErrText(err)
				writeHead(s, errResp(serveErrText(err)), m.delay)
				return
			}
			data, _ := json.Marshal(bd)
			writeHead(s, &NodeRespHead{OK: true, Data: data}, m.delay)
		case ProtoTimes:
			times, err := m.lib().Times(req.Path)
			if err != nil {
				ev.OK, ev.Extra = false, serveErrText(err)
				writeHead(s, errResp(serveErrText(err)), m.delay)
				return
			}
			data, _ := json.Marshal(map[string]any{"times": times})
			writeHead(s, &NodeRespHead{OK: true, Data: data}, m.delay)
		case ProtoStat:
			realPath, _, err := m.lib().CheckReadable(filepath.Clean(req.Path))
			if err != nil {
				ev.OK, ev.Extra = false, "路径不在对方开放的根目录内"
				writeHead(s, errResp("路径不在对方开放的根目录内"), m.delay)
				return
			}
			st, err := os.Stat(realPath)
			if err != nil || !st.Mode().IsRegular() {
				ev.OK, ev.Extra = false, "文件不存在"
				writeHead(s, errResp("文件不存在"), m.delay)
				return
			}
			data, _ := json.Marshal(map[string]any{"size": st.Size(), "mtimeMs": st.ModTime().UnixMilli()})
			writeHead(s, &NodeRespHead{OK: true, Data: data}, m.delay)
		default:
			ev.OK, ev.Extra = false, "未知协议"
			writeHead(s, errResp("未知协议"), m.delay)
		}
	}
}

func serveErrText(err error) string {
	switch {
	case errors.Is(err, core.ErrOutsideRoots):
		return "路径不在对方开放的根目录内"
	case errors.Is(err, core.ErrNotFound):
		return "文件不存在"
	default:
		return "处理失败: " + err.Error()
	}
}

func (m *Manager) shareEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings.Share
}

func (m *Manager) cacheServeEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings.CacheServe
}

func writeHead(w io.Writer, h *NodeRespHead, delay p2p.ChunkDelay) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return p2p.WriteMsg(w, b, delay)
}

/* ---------------- 服务端：image（缓存代答 / 转发 / 本地出图） ---------------- */

func (m *Manager) serveImage(s network.Stream) {
	defer s.Close()
	s = m.countWrap(s)
	peerID := s.Conn().RemotePeer().String()
	raw, err := p2p.ReadMsg(s, 0)
	if err != nil {
		return
	}
	var req NodeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeHead(s, errResp("请求格式错误"), m.delay)
		return
	}
	m.log.Debug("入站 image 请求", "from", shortID(peerID), "path", filepath.Base(req.Path), "target", shortID(req.Target))
	if req.PubX == "" {
		writeHead(s, errResp("缺少有效的请求方明钥"), m.delay)
		return
	}
	target := req.Target
	if target == "" {
		target = peerID
	}
	if target != m.selfID {
		// 节点记忆环路防护：本节点已在途径表中 = 请求绕回来了，拒绝
		if slices.Contains(req.Via, m.selfID) {
			m.log.Warn("检测到请求环路，拒绝", "from", shortID(peerID), "target", shortID(target))
			writeHead(s, errResp("检测到请求环路"), m.delay)
			return
		}
		m.serveAsIntermediate(s, peerID, target, &req)
		return
	}
	m.serveOwnImage(s, &req)
}

// serveOwnImage 目标节点即本节点：校验 → 整文件 ECIES → 流式写出。
func (m *Manager) serveOwnImage(s network.Stream, req *NodeRequest) {
	start := time.Now()
	peerID := s.Conn().RemotePeer().String()
	deny := func(msg string) {
		m.logAccess(stats.AccessEvent{Kind: stats.KindImage, Peer: peerID, Path: req.Path,
			OK: false, Extra: msg, DurMS: time.Since(start).Milliseconds()})
		writeHead(s, errResp(msg), m.delay)
	}
	if !m.shareEnabled() {
		deny("对方未开放图库访问")
		return
	}
	pubX, err := crypto.B64uDec(req.PubX)
	if err != nil || len(pubX) != 32 {
		deny("缺少有效的请求方明钥")
		return
	}
	realPath, _, err := m.lib().CheckReadable(filepath.Clean(req.Path))
	if err != nil {
		deny("路径不在对方开放的根目录内")
		return
	}
	st, err := os.Stat(realPath)
	if err != nil || !st.Mode().IsRegular() {
		deny("文件不存在")
		return
	}
	if st.Size() > MaxImageSize {
		deny(fmt.Sprintf("图片超过大小上限（%dMB）", MaxImageSize>>20))
		return
	}
	// 第一遍：内容哈希（receipt 与应答头都要用）
	f, err := os.Open(realPath)
	if err != nil {
		deny("文件不存在")
		return
	}
	shaHex, err := shaHexOfFile(f)
	f.Close()
	if err != nil {
		deny("读取失败")
		return
	}
	// 第二遍：ECIES 加密到临时文件（内存受限，PROTOCOL §3.6）
	src, err := os.Open(realPath)
	if err != nil {
		deny("读取失败")
		return
	}
	defer src.Close()
	hdr, ctFile, ctLen, err := crypto.EncryptReader(pubX, src, func() (*os.File, error) {
		return os.CreateTemp(m.cache().Dir(), "enc-*.tmp")
	})
	if err != nil {
		deny("加密失败: " + err.Error())
		return
	}
	defer func() { ctFile.Close(); os.Remove(ctFile.Name()) }()

	receipt := m.deps.ID.SignReceipt(m.selfID, req.Path, shaHex, time.Now().UnixMilli())
	head := &NodeRespHead{
		OK:        true,
		PlainSize: st.Size(),
		MtimeMs:   st.ModTime().UnixMilli(),
		Sha256:    shaHex,
		Ext:       RpathExt(req.Path),
		Receipt:   receipt,
	}
	if err := writeHead(s, head, m.delay); err != nil {
		return
	}
	// 密文帧：80B ECIES 头（PNX1+salt+ephPub+nonce+tag）裸写 + 密文体（至 EOF）
	if _, err := s.Write(hdr); err != nil {
		return
	}
	if m.delay != nil {
		m.delay(len(hdr))
	}
	m.log.Info("提供加密图片", "to", shortID(s.Conn().RemotePeer().String()),
		"file", filepath.Base(realPath), "cipher", ctLen)
	streamTemp(s, ctFile, ctLen, m.delay)
	m.logAccess(stats.AccessEvent{Kind: stats.KindImage, Peer: peerID, Path: req.Path,
		Bytes: st.Size(), OK: true, DurMS: time.Since(start).Milliseconds()})
}

// streamTemp 把密文临时文件写出到流（分段写出以应用延迟模拟与背压）。
func streamTemp(w io.Writer, f *os.File, n int64, delay p2p.ChunkDelay) {
	buf := make([]byte, 512*1024)
	for n > 0 {
		chunk := int64(len(buf))
		if n < chunk {
			chunk = n
		}
		got, err := f.Read(buf[:chunk])
		if got > 0 {
			if _, werr := w.Write(buf[:got]); werr != nil {
				return
			}
			if delay != nil {
				delay(got)
			}
			n -= int64(got)
		}
		if err != nil {
			return
		}
	}
}

// serveAsIntermediate 中间节点：缓存代答（§3.4-2）或转发（§3.4-3）。
func (m *Manager) serveAsIntermediate(s network.Stream, fromID, target string, req *NodeRequest) {
	start := time.Now()
	forward := func() {
		m.forwardImage(s, fromID, target, req)
	}
	if !m.cacheServeEnabled() {
		forward()
		return
	}
	key := core.CacheKey(target, req.Path)
	entry := m.cache().File(target, key)
	if entry == nil || entry.Receipt == nil {
		forward()
		return
	}
	// 向目标核对 stat（8s 超时）；不可达 → 提供过期缓存（stale）
	fresh := m.statPeer(target, req.Path)
	if fresh != nil && (fresh.Size != entry.Size || fresh.MtimeMs != entry.MtimeMs) {
		forward() // 文件已更新：走真实链路
		return
	}
	cachePath := m.cache().Path(target, key, entry.Ext)
	f, err := os.Open(cachePath)
	if err != nil {
		forward()
		return
	}
	defer f.Close()
	// 缓存完整性：sha256 与索引一致才可代答
	shaHex, err := shaHexOfFile(f)
	if err != nil || shaHex != entry.Sha256 {
		m.log.Warn("缓存文件损坏，放弃代答", "peer", shortID(target), "key", key)
		forward()
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		forward()
		return
	}
	pubX, err := crypto.B64uDec(req.PubX)
	if err != nil || len(pubX) != 32 {
		forward()
		return
	}
	// 用请求方明钥重新加密（原样携带目标节点的原始 receipt）
	hdr, ctFile, ctLen, err := crypto.EncryptReader(pubX, f, func() (*os.File, error) {
		return os.CreateTemp(m.cache().Dir(), "enc-*.tmp")
	})
	if err != nil {
		forward()
		return
	}
	defer func() { ctFile.Close(); os.Remove(ctFile.Name()) }()
	head := &NodeRespHead{
		OK:        true,
		PlainSize: entry.Size,
		MtimeMs:   entry.MtimeMs,
		Sha256:    entry.Sha256,
		Ext:       entry.Ext,
		Receipt:   entry.Receipt,
		Stale:     fresh == nil,
	}
	m.log.Info("链路缓存代答", "to", shortID(fromID), "of", shortID(target),
		"file", filepath.Base(req.Path), "stale", head.Stale)
	if err := writeHead(s, head, m.delay); err != nil {
		return
	}
	if _, err := s.Write(hdr); err != nil {
		return
	}
	if m.delay != nil {
		m.delay(len(hdr))
	}
	streamTemp(s, ctFile, ctLen, m.delay)
	m.logAccess(stats.AccessEvent{Kind: stats.KindCache, Peer: fromID, Path: req.Path,
		Bytes: entry.Size, OK: true, Extra: fmt.Sprintf("of=%s,stale=%t", shortID(target), head.Stale),
		DurMS: time.Since(start).Milliseconds()})
}

// forwardImage 中间节点转发：按【自身路由】递归转发（直连目标 → 自己的邻居逐级），
// 而非强制直拨目标——多级 NAT 链路中，下一级节点未必能直达目标。
// 密文对转发者不可读，原样回写。
// 环路防护 = 节点记忆：把自身追加进 Via 再转发；任何环路在首次重临时即被
// 途中节点拒绝（无需跳数上限，转发天然终止于有限节点集）。排除上一跳防乒乓。
func (m *Manager) forwardImage(s network.Stream, fromID, target string, req *NodeRequest) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(m.deps.Ctx, ImageTimeout)
	defer cancel()
	out := &NodeRequest{
		Path:   req.Path,
		PubX:   req.PubX, // 透传请求方明钥
		Target: target,
		Via:    append(append([]string{}, req.Via...), m.selfID),
	}
	headBytes, body, err := m.routeImage(ctx, out, target, fromID)
	if err != nil {
		m.logAccess(stats.AccessEvent{Kind: stats.KindRelayFail, Peer: fromID, Path: req.Path,
			OK: false, Extra: err.Error(), DurMS: time.Since(start).Milliseconds()})
		writeHead(s, errResp("转发失败: "+err.Error()), m.delay)
		return
	}
	defer body.Close()
	var head NodeRespHead
	if err := json.Unmarshal(headBytes, &head); err != nil {
		m.logAccess(stats.AccessEvent{Kind: stats.KindRelayFail, Peer: fromID, Path: req.Path,
			OK: false, Extra: "应答格式错误", DurMS: time.Since(start).Milliseconds()})
		writeHead(s, errResp("转发失败: 应答格式错误"), m.delay)
		return
	}
	m.log.Info("转发图片请求", "from", shortID(fromID), "target", shortID(target),
		"ok", head.OK)
	if err := p2p.WriteMsg(s, headBytes, m.delay); err != nil {
		return
	}
	if !head.OK {
		// 下游错误应答已透传；记为失败转发（无密文体）
		m.logAccess(stats.AccessEvent{Kind: stats.KindRelayFail, Peer: fromID, Path: req.Path,
			OK: false, Extra: head.Error, DurMS: time.Since(start).Milliseconds()})
		return
	}
	// 密文体原样搬运（不读入内存）
	buf := make([]byte, 512*1024)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return
			}
			if m.delay != nil {
				m.delay(n)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				s.CloseWrite()
				m.logAccess(stats.AccessEvent{Kind: stats.KindRelay, Peer: fromID, Path: req.Path,
					Bytes: head.PlainSize, OK: true,
					Extra: "target=" + shortID(target), DurMS: time.Since(start).Milliseconds()})
			}
			return
		}
	}
}

// statPeer 向目标发 stat 请求（链路缓存核对；超时 8s，失败返回 nil）。
func (m *Manager) statPeer(target, rpath string) *StatInfo {
	ctx, cancel := context.WithTimeout(m.deps.Ctx, StatTimeout)
	defer cancel()
	headBytes, body, err := m.callRaw(ctx, target, ProtoStat, &NodeRequest{Path: rpath})
	if err != nil {
		return nil
	}
	defer body.Close()
	var head NodeRespHead
	if err := json.Unmarshal(headBytes, &head); err != nil || !head.OK {
		return nil
	}
	var d struct {
		Size    int64 `json:"size"`
		MtimeMs int64 `json:"mtimeMs"`
	}
	if err := json.Unmarshal(head.Data, &d); err != nil {
		return nil
	}
	return &StatInfo{Size: d.Size, MtimeMs: d.MtimeMs}
}

type StatInfo struct {
	Size    int64
	MtimeMs int64
}

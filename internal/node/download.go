package node

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"lotus/internal/core"
	"lotus/internal/crypto"
	"lotus/internal/stats"
)

// EnsureImage 取远程图片（阻塞等待完成，/api/net/image 用）：
// 磁盘命中 → 直接返回；否则入队下载（同键去重、direct=true 插队）并等待。
func (m *Manager) EnsureImage(ctx context.Context, target, rpath, folderKey string, direct bool) (string, error) {
	prio := 0
	if direct {
		prio = 1
	}
	ch := m.enqueueImage(target, rpath, folderKey, prio)
	v, err := core.Wait(ch)
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// PrefetchImage 非阻塞入队（prefetch 批量用，进度由账本跟踪）。
func (m *Manager) PrefetchImage(target, rpath, folderKey string) {
	m.enqueueImage(target, rpath, folderKey, 0)
}

func (m *Manager) enqueueImage(target, rpath, folderKey string, prio int) <-chan core.TaskResult {
	key := core.CacheKey(target, rpath)
	return m.deps.Queue.Enqueue(key, folderKey, prio, func(ctx context.Context) (any, error) {
		// 缓存路径在落地时计算（镜像树依赖当时的目录列表；以对方根为根镜像）
		cachePath := m.cache().PathFor(target, rpath)
		// 双检：同键并发场景下可能已被前一个任务落地
		if _, err := os.Stat(cachePath); err == nil {
			return cachePath, nil
		}
		return m.downloadOnce(ctx, target, rpath, key, cachePath)
	})
}

// downloadOnce 完整下载管线：请求 → 密文流式落临时 → 解密流式落缓存临时 →
// sha256 + receipt 校验 → 原子落地 → 索引更新（PROTOCOL §3.6）。
func (m *Manager) downloadOnce(ctx context.Context, target, rpath, key, cachePath string) (res any, err error) {
	start := time.Now()
	var plainSize int64
	defer func() {
		ev := stats.AccessEvent{Kind: stats.KindDownload, Peer: target, Path: rpath,
			Bytes: plainSize, OK: err == nil, DurMS: time.Since(start).Milliseconds()}
		if err != nil {
			ev.Extra = err.Error()
		}
		m.logAccess(ev)
	}()
	headBytes, body, err := m.requestImage(ctx, target, rpath, m.PubXSelf())
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	defer body.Close()
	var head NodeRespHead
	if err := json.Unmarshal(headBytes, &head); err != nil {
		return nil, errors.New("下载失败: 应答格式错误")
	}
	if !head.OK {
		return nil, fmt.Errorf("下载失败: %s", head.Error)
	}
	plainSize = head.PlainSize
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return nil, err
	}
	// 密文 → 临时文件（边收边落盘；接收端不整段进内存）
	tmpCt, err := os.CreateTemp(filepath.Dir(cachePath), "dl-*.ct")
	if err != nil {
		return nil, err
	}
	ctName := tmpCt.Name()
	defer func() { tmpCt.Close(); os.Remove(ctName) }()
	n, err := io.Copy(tmpCt, io.LimitReader(body, MaxImageSize+1))
	if cerr := tmpCt.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	if n > MaxImageSize {
		return nil, errors.New("下载失败: 图片超过大小上限")
	}
	if n < crypto.ECIESHead {
		return nil, errors.New("下载失败: 密文不完整")
	}
	// 解密（校验 GCM 标签）→ 明文临时文件，顺带计算 sha256
	tmpCt2, err := os.Open(ctName)
	if err != nil {
		return nil, err
	}
	defer tmpCt2.Close()
	hdr := make([]byte, crypto.ECIESHead)
	if _, err := io.ReadFull(tmpCt2, hdr); err != nil {
		return nil, errors.New("下载失败: 密文头不完整")
	}
	tmpPlain, err := os.CreateTemp(filepath.Dir(cachePath), "dl-*.pt")
	if err != nil {
		return nil, err
	}
	ptName := tmpPlain.Name()
	defer func() { tmpPlain.Close(); os.Remove(ptName) }()
	shaHex, err := crypto.DecryptStream(m.deps.ID.SeedX, hdr, tmpCt2, tmpPlain)
	if cerr := tmpPlain.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("解密失败: %w", err)
	}
	// 校验之二：内容哈希
	if shaHex != head.Sha256 {
		return nil, errors.New("图片校验不符")
	}
	// 校验之三：内容凭证必须是【目标节点】对该内容的签名
	if head.Receipt == nil || head.Receipt.Node != target || head.Receipt.Sha256 != head.Sha256 {
		return nil, errors.New("缺少有效的内容凭证")
	}
	pub, err := m.deps.Host.PeerPubKey(target)
	if err != nil {
		return nil, fmt.Errorf("无法验证内容凭证: %w", err)
	}
	if !crypto.VerifyReceipt(head.Receipt, pub) {
		return nil, errors.New("内容凭证验签失败")
	}
	// 原子落地 + 索引（此后取消不影响已完整校验的结果）
	if err := os.Rename(ptName, cachePath); err != nil {
		return nil, err
	}
	idx := m.cache().Get(target)
	idx.LastSeen = time.Now().UnixMilli()
	if att := m.AttestationOf(target); att != nil && att.Name != "" {
		idx.Name = att.Name
	}
	root, _ := m.cache().MatchRoot(target, rpath)
	idx.Files[key] = &core.FileEntry{
		Rpath:   rpath,
		Sha256:  head.Sha256,
		Size:    head.PlainSize,
		MtimeMs: head.MtimeMs,
		Ext:     RpathExt(rpath),
		Root:    root,
		Receipt: head.Receipt,
		TS:      time.Now().UnixMilli(),
	}
	m.cache().Mark(target)
	m.log.Info("图片已缓存", "from", shortID(target), "file", filepath.Base(rpath))
	return cachePath, nil
}

// shaHexOfFile 计算文件内容 sha256（hex）。
func shaHexOfFile(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Prefetch 批量预取（PROTOCOL §2.3）：命中缓存计数，未命中入队（非阻塞）。
func (m *Manager) Prefetch(target, folder string, paths []string) (total, cached int) {
	if len(paths) > 5000 {
		paths = paths[:5000]
	}
	folderKey := target + "|" + folder
	for _, rp := range paths {
		if _, err := os.Stat(m.cache().PathFor(target, rp)); err == nil {
			cached++
		} else {
			m.PrefetchImage(target, rp, folderKey)
		}
	}
	m.deps.Queue.SetStats(folderKey, core.FolderStat{Total: len(paths), Done: cached})
	return len(paths), cached
}

// CancelFolder 离开文件夹：清进度 + 剔除排队 + 尽力取消在途（ctx 取消）。
func (m *Manager) CancelFolder(target, folder string) {
	m.deps.Queue.Cancel(target + "|" + folder)
}

// FolderProgress 进度查询（无记录返回全 0）。
func (m *Manager) FolderProgress(target, folder string) core.FolderStat {
	return m.deps.Queue.Stats(target + "|" + folder)
}

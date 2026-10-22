package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lotus/internal/crypto"
)

// CacheKey = hex(SHA256(nodeID + "|" + rpath)) 前 24 位（PROTOCOL §1.6，冻结）。
func CacheKey(nodeID, rpath string) string {
	s := sha256.Sum256([]byte(nodeID + "|" + rpath))
	return fmt.Sprintf("%x", s[:])[:24]
}

// FileEntry 缓存索引中的单文件条目（结构沿用旧 index.json，PROTOCOL §6.3）。
type FileEntry struct {
	Rpath   string          `json:"rpath"`
	Sha256  string          `json:"sha256"`
	Size    int64           `json:"size"`
	MtimeMs int64           `json:"mtimeMs"`
	Ext     string          `json:"ext"`
	Root    string          `json:"root,omitempty"` // 所属的对方根（镜像归档用）
	Receipt *crypto.Receipt `json:"receipt,omitempty"`
	TS      int64           `json:"ts"`
}

// Listing 目录列表缓存（离线浏览用）。Root = 该目录所属的对方根路径
// （以对方根为根镜像到本地；空 = 节点根列表本身）。
type Listing struct {
	TS   int64       `json:"ts"`
	Root string      `json:"root,omitempty"`
	Data *BrowseData `json:"data"`
}

// Index 每节点一份（net-cache/<peerID>/index.json）。
type Index struct {
	Name     string                `json:"name"`
	LastSeen int64                 `json:"lastSeen"`
	Listings map[string]*Listing   `json:"listings"`
	Files    map[string]*FileEntry `json:"files"`
}

func newIndex() *Index {
	return &Index{Listings: map[string]*Listing{}, Files: map[string]*FileEntry{}}
}

// CachedNodeInfo 供 /api/net/state.cached。
type CachedNodeInfo struct {
	ID       string
	Name     string
	Files    int
	Listings int
	LastSeen int64
}

// Cache 管理所有节点的缓存索引，2s 防抖落盘（PROTOCOL §6.3）。
type Cache struct {
	dir string

	mu    sync.Mutex
	index map[string]*Index // key = peerID
	mark  map[string]time.Time
	timer *time.Timer
}

// OpenCache 打开缓存目录并加载全部已有索引。
func OpenCache(dir string) (*Cache, error) {
	c := &Cache{dir: dir, index: map[string]*Index{}, mark: map[string]time.Time{}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "index.json"))
		if err != nil {
			continue
		}
		var idx Index
		if err := json.Unmarshal(b, &idx); err != nil {
			continue // 坏索引不阻塞启动
		}
		if idx.Listings == nil {
			idx.Listings = map[string]*Listing{}
		}
		if idx.Files == nil {
			idx.Files = map[string]*FileEntry{}
		}
		c.index[e.Name()] = &idx
	}
	return c, nil
}

// Get 返回该节点的索引（不存在则创建空索引）。
func (c *Cache) Get(peerID string) *Index {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx, ok := c.index[peerID]
	if !ok {
		idx = newIndex()
		c.index[peerID] = idx
	}
	return idx
}

// VisitedRoot 已访问过的对方根（含镜像缓存文件数）。
type VisitedRoot struct {
	Path  string // 对方根路径（rpath 语义）
	Name  string // 根目录名（basename）
	Files int    // 该根下已缓存的文件数
}

func normPath(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, "/", "\\"))
}

func rpathExt(p string) string {
	e := strings.ToLower(filepath.Ext(p))
	return e
}

// SanitizeName 清洗路径段为 Windows 安全文件名。
func SanitizeName(s string) string {
	const forbidden = `<>:"/|?*`
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || strings.ContainsRune(forbidden, r) {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	out := strings.TrimRight(b.String(), " .")
	upper := strings.ToUpper(out)
	for _, reserved := range []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"} {
		if upper == reserved {
			out = "_" + out
			break
		}
	}
	if out == "" {
		out = "_"
	}
	return out
}

// MatchRoot 找到包含 rpath 的、已缓存的对方根（最长前缀匹配，大小写不敏感）。
func (c *Cache) MatchRoot(nodeID, rpath string) (string, bool) {
	idx := c.Lookup(nodeID)
	if idx == nil {
		return "", false
	}
	rn := normPath(filepath.Clean(rpath))
	best := ""
	for _, l := range idx.Listings {
		if l == nil || l.Root == "" {
			continue
		}
		ln := normPath(filepath.Clean(l.Root))
		if rn == ln || strings.HasPrefix(rn, ln+string(filepath.Separator)) {
			if len(ln) > len(normPath(best)) {
				best = l.Root
			}
		}
	}
	return best, best != ""
}

// MirrorPath 计算 rpath 在本地镜像树中的存放路径：
// <cacheDir>/<nodeID>/<对方根名[-hash]>/<相对路径…>。
// 无匹配根（未浏览过其所在目录）时回退旧的哈希平铺布局。
func (c *Cache) MirrorPath(nodeID, rpath string) (path, root string, mirrored bool) {
	root, ok := c.MatchRoot(nodeID, rpath)
	if !ok {
		return filepath.Join(c.dir, nodeID, CacheKey(nodeID, rpath)+rpathExt(rpath)), "", false
	}
	rel := rpath[len(root):]
	rootName := SanitizeName(filepath.Base(root))
	sum := sha256.Sum256([]byte(normPath(root)))
	rootName += "-" + fmt.Sprintf("%x", sum[:])[:6]
	parts := []string{c.dir, nodeID, rootName}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "" || seg == "." {
			continue
		}
		parts = append(parts, SanitizeName(seg))
	}
	return filepath.Join(parts...), root, true
}

// PathFor rpath 的本地缓存路径（镜像优先，哈希回退）。
func (c *Cache) PathFor(nodeID, rpath string) string {
	if p, _, ok := c.MirrorPath(nodeID, rpath); ok {
		return p
	}
	return filepath.Join(c.dir, nodeID, CacheKey(nodeID, rpath)+rpathExt(rpath))
}

// VisitedRoots 汇总某节点已访问过的对方根（永久展示在本地根视图的数据源）。
func (c *Cache) VisitedRoots(nodeID string) []VisitedRoot {
	idx := c.Lookup(nodeID)
	if idx == nil {
		return nil
	}
	seen := map[string]*VisitedRoot{}
	for _, l := range idx.Listings {
		if l == nil || l.Root == "" {
			continue
		}
		if _, ok := seen[l.Root]; !ok {
			seen[l.Root] = &VisitedRoot{Path: l.Root, Name: SanitizeName(filepath.Base(l.Root))}
		}
	}
	for _, f := range idx.Files {
		if f == nil || f.Root == "" {
			continue
		}
		if vr, ok := seen[f.Root]; ok {
			vr.Files++
		}
	}
	out := make([]VisitedRoot, 0, len(seen))
	for _, vr := range seen {
		out = append(out, *vr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Dir 返回缓存根目录。
func (c *Cache) Dir() string { return c.dir }

// Lookup 只读获取（不创建）。
func (c *Cache) Lookup(peerID string) *Index {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index[peerID]
}

// File 查缓存文件条目。
func (c *Cache) File(peerID, key string) *FileEntry {
	idx := c.Lookup(peerID)
	if idx == nil {
		return nil
	}
	return idx.Files[key]
}

// Path 缓存文件绝对路径：<cacheDir>/<peerID>/<cacheKey><ext>。
func (c *Cache) Path(peerID, key, ext string) string {
	return filepath.Join(c.dir, peerID, key+ext)
}

// Mark 标记脏索引，2s 防抖后落盘。
func (c *Cache) Mark(peerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mark[peerID] = time.Now()
	if c.timer != nil {
		return
	}
	c.timer = time.AfterFunc(2*time.Second, func() { c.flush() })
}

func (c *Cache) flush() {
	c.mu.Lock()
	c.timer = nil
	var toSave []string
	now := time.Now()
	for id, t := range c.mark {
		if now.Sub(t) >= 2*time.Second {
			toSave = append(toSave, id)
			delete(c.mark, id)
		}
	}
	// 未到 2s 的重新计时
	var next bool
	for range c.mark {
		next = true
	}
	idxSnaps := map[string][]byte{}
	for _, id := range toSave {
		idx := c.index[id]
		if idx == nil {
			continue
		}
		b, err := json.Marshal(idx)
		if err == nil {
			idxSnaps[id] = b
		}
	}
	c.mu.Unlock()

	if next {
		c.mu.Lock()
		if c.timer == nil {
			c.timer = time.AfterFunc(2*time.Second, func() { c.flush() })
		}
		c.mu.Unlock()
	}
	for id, b := range idxSnaps {
		dir := filepath.Join(c.dir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		tmp := filepath.Join(dir, "index.json.tmp")
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			continue
		}
		os.Rename(tmp, filepath.Join(dir, "index.json"))
	}
}

// Close 立即落盘全部脏索引。
func (c *Cache) Close() {
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	for id := range c.mark {
		c.mark[id] = time.Now().Add(-2 * time.Second)
	}
	c.mu.Unlock()
	c.flush()
}

// NodeIDs 全部有索引的节点 ID。
func (c *Cache) NodeIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.index))
	for id := range c.index {
		out = append(out, id)
	}
	return out
}

// CachedNodes 汇总"不在成员表但有内容"的节点（state.cached 的数据源）。
// exclude 传入当前在线成员 ID 集合与自身 ID。
func (c *Cache) CachedNodes(exclude map[string]bool) []CachedNodeInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []CachedNodeInfo{}
	for id, idx := range c.index {
		if exclude[id] {
			continue
		}
		nFiles := len(idx.Files)
		nList := len(idx.Listings)
		if nFiles == 0 && nList == 0 {
			continue
		}
		out = append(out, CachedNodeInfo{ID: id, Name: idx.Name, Files: nFiles, Listings: nList, LastSeen: idx.LastSeen})
	}
	// lastSeen 降序（与旧实现一致）
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LastSeen > out[j-1].LastSeen; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

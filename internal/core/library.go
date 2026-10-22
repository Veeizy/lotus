// Package core：本地图库扫描、链路缓存、下载队列、配置持久化。
// 本包不 import net/http 与 libp2p（架构边界，见 PROTOCOL.md §0）。
package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// MIME 表（与旧实现逐项一致，PROTOCOL §7）。
var MIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".jfif": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".bmp":  "image/bmp",
	".avif": "image/avif",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
}

// ErrOutsideRoots / ErrNotFound 与 HTTP 状态码的映射由 api 层完成；
// core 只区分错误类别，文案与旧实现逐字一致。
var sep = string(filepath.Separator)

var (
	ErrOutsideRoots = errors.New("路径不在配置的根目录内")
	ErrNotFound     = errors.New("文件夹不存在")
)

// BrowseData 是 /api/browse 与 /picnet/browse/1 的统一输出结构。
// 字段与旧 server.js buildBrowse 逐字段一致；空集合必须是 [] 而非 null。
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Mtime *int64 `json:"mtime,omitempty"`
}

type Crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type BrowseData struct {
	Name        string  `json:"name"`
	Path        string  `json:"path"`
	Parent      *string `json:"parent"`
	IsRoots     bool    `json:"isRoots,omitempty"`
	Breadcrumbs []Crumb `json:"breadcrumbs"`
	Folders     []Entry `json:"folders"`
	Images      []Entry `json:"images"`
	Offline     bool    `json:"offline,omitempty"` // 离线回退时由管理面附加
}

type Root struct {
	Name string
	Dir  string // 用户配置的路径（用于展示）
	Real string // EvalSymlinks 解析后的真实路径（用于安全判定）
	Mtime int64
	Service bool // 同机 lotus-server 的根（并入本机图库：仅本地浏览直接读盘，不对外共享）
}

// Library 管理配置的根路径集合与目录扫描。
type Library struct {
	roots []*Root
	coll  *collate.Collator
}

// NewLibrary 解析并校验根路径，不存在的跳过并返回警告列表。
func NewLibrary(paths []string) (*Library, []string) {
	l := &Library{coll: collate.New(language.Chinese, collate.IgnoreCase, collate.IgnoreDiacritics, collate.Numeric)}
	var warns []string
	for _, p := range paths {
		if w := l.addRoot(p, false); w != "" {
			warns = append(warns, w)
		}
	}
	return l, warns
}

// addRoot 校验并追加一个根；service=true 为同机服务根（不存在时静默跳过，不产生警告）。
func (l *Library) addRoot(p string, service bool) string {
	dir, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return ""
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		if service {
			return ""
		}
		return fmt.Sprintf("[警告] 根路径不存在或不是文件夹，已忽略: %s", dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	name := filepath.Base(dir)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = dir
	}
	if service {
		// 与已有根去重（Windows 大小写不敏感）
		for _, r := range l.roots {
			if strings.EqualFold(r.Real, real) {
				return ""
			}
		}
	}
	l.roots = append(l.roots, &Root{Name: name, Dir: dir, Real: real, Mtime: st.ModTime().UnixMilli(), Service: service})
	return ""
}

// AddServiceRoots 追加同机服务的根（Service 标记；仅供本地浏览，不参与对外共享）。
// 与已有根重复的路径跳过。
func (l *Library) AddServiceRoots(paths []string) {
	for _, p := range paths {
		_ = l.addRoot(p, true)
	}
}

// Clone 复制根列表（Root 不可变，浅拷贝即可；排序器复用）。
func (l *Library) Clone() *Library {
	return &Library{roots: append([]*Root(nil), l.roots...), coll: l.coll}
}

func (l *Library) Roots() []*Root { return l.roots }

// IsInsideRoots 判定解析后的真实路径是否落在某个根的真实路径内。
// absRes 已是 EvalSymlinks 解析结果（或不存在路径的清理结果）。
func (l *Library) insideReal(absRes string) *Root {
	for _, r := range l.roots {
		if pathInside(r.Real, absRes) {
			return r
		}
	}
	return nil
}

func pathInside(rootDir, p string) bool {
	if strings.EqualFold(rootDir, p) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(p), strings.ToLower(rootDir+sep))
}

// FindRoot 按字符串判定 abs 归属（保持旧 findRoot 的展示语义）；
// 读取文件前的安全复核请使用 CheckReadable。
func (l *Library) FindRoot(abs string) *Root {
	abs = filepath.Clean(abs)
	for _, r := range l.roots {
		rel, err := filepath.Rel(r.Dir, abs)
		if err != nil {
			continue
		}
		if rel == "." || rel == "" {
			return r
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+sep) && !filepath.IsAbs(rel) {
			return r
		}
	}
	return nil
}

// CheckReadable 读取前的安全复核：路径解析 symlink 后必须仍在根内。
// 返回解析后的真实路径（供读取）与所属根。这是对旧实现 symlink 越权缺口的收紧。
func (l *Library) CheckReadable(abs string) (realPath string, r *Root, err error) {
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// 不存在/链接断裂：回落字符串判定，让后续 stat 给出 404
		if root := l.FindRoot(abs); root != nil {
			return abs, root, nil
		}
		return "", nil, ErrOutsideRoots
	}
	root := l.insideReal(resolved)
	if root == nil {
		return "", nil, ErrOutsideRoots
	}
	return resolved, root, nil
}

// sortEntries 以 zh+numeric 排序（对应 Intl.Collator('zh',{numeric,sensitivity:base})）。
func (l *Library) sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool { return l.coll.CompareString(es[i].Name, es[j].Name) < 0 })
}

// ListDir 一次 readdir 取类型（不逐项 stat），分类排序。
// symlink 仅在名字像图片时才 stat 跟随判断是否目录（与旧实现一致）。
func (l *Library) ListDir(dir string) (folders, images []Entry, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	folders = []Entry{}
	images = []Entry{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		isDir := e.IsDir()
		if !isDir && e.Type()&os.ModeSymlink != 0 {
			if _, ok := MIME[ext]; ok {
				st, err := os.Stat(filepath.Join(dir, name))
				if err != nil {
					continue
				}
				isDir = st.IsDir()
			}
		}
		if isDir {
			folders = append(folders, Entry{Name: name, Path: filepath.Join(dir, name)})
		} else if _, ok := MIME[ext]; ok {
			images = append(images, Entry{Name: name, Path: filepath.Join(dir, name)})
		}
	}
	l.sortEntries(folders)
	l.sortEntries(images)
	return folders, images, nil
}

// Browse 空路径返回 roots 视图；否则返回该层内容。
// 错误：ErrOutsideRoots（403）/ ErrNotFound（404）。
func (l *Library) Browse(p string) (*BrowseData, error) {
	if p == "" {
		return l.RootsView(), nil
	}
	abs := filepath.Clean(p)
	root := l.FindRoot(abs)
	if root == nil {
		return nil, ErrOutsideRoots
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return nil, ErrNotFound
	}
	// symlink 复核（安全收紧，PROTOCOL §8-5）：目录解析 symlink 后必须仍在根内
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && l.insideReal(resolved) == nil {
		return nil, ErrOutsideRoots
	}
	folders, images, err := l.ListDir(abs)
	if err != nil {
		return nil, ErrNotFound
	}
	rel, err := filepath.Rel(root.Dir, abs)
	if err != nil {
		return nil, ErrOutsideRoots
	}
	crumbs := []Crumb{{Name: root.Name, Path: root.Dir}}
	cur := root.Dir
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		crumbs = append(crumbs, Crumb{Name: seg, Path: cur})
	}
	name := root.Name
	var parent *string
	if rel != "." && rel != "" {
		name = filepath.Base(abs)
		d := filepath.Dir(abs)
		parent = &d
	}
	return &BrowseData{
		Name:        name,
		Path:        abs,
		Parent:      parent,
		Breadcrumbs: crumbs,
		Folders:     folders,
		Images:      images,
	}, nil
}

// RootsView 根目录列表视图（isRoots:true）。
func (l *Library) RootsView() *BrowseData {
	folders := []Entry{}
	for _, r := range l.roots {
		mt := r.Mtime
		folders = append(folders, Entry{Name: r.Name, Path: r.Dir, Mtime: &mt})
	}
	return &BrowseData{Name: "roots", Path: "", IsRoots: true, Folders: folders,
		Images: []Entry{}, Breadcrumbs: []Crumb{}}
}

// Times 批量读取目录条目的修改时间（并发 64，跳过隐藏项，失败项跳过）。
func (l *Library) Times(p string) ([]Entry, error) {
	abs := filepath.Clean(p)
	if l.FindRoot(abs) == nil {
		return nil, ErrOutsideRoots
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, ErrNotFound
	}
	type res struct {
		idx   int
		name  string
		mtime int64
		ok    bool
	}
	out := make([]res, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for i, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st, err := os.Stat(filepath.Join(abs, name))
			if err != nil {
				return
			}
			out[i] = res{idx: i, name: name, mtime: st.ModTime().UnixMilli(), ok: true}
		}(i, name)
	}
	wg.Wait()
	times := []Entry{}
	for _, r := range out {
		if r.ok {
			mt := r.mtime
			times = append(times, Entry{Name: r.name, Mtime: &mt})
		}
	}
	return times, nil
}

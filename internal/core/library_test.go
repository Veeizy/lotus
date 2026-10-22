package core

import (
	"path/filepath"
	"testing"
)

func TestServiceRoots(t *testing.T) {
	own := t.TempDir()
	svc := t.TempDir()
	l, warns := NewLibrary([]string{own})
	if len(warns) != 0 {
		t.Fatalf("自身根不应有警告: %v", warns)
	}
	l.AddServiceRoots([]string{svc, svc, filepath.Join(svc, "不存在")})
	roots := l.Roots()
	if len(roots) != 2 {
		t.Fatalf("应有 2 个根（自身 + 服务，服务重复与不存在跳过）: %d", len(roots))
	}
	var svcMarked, ownMarked int
	for _, r := range roots {
		switch {
		case r.Service && r.Dir == svc:
			svcMarked++
		case !r.Service && r.Dir == own:
			ownMarked++
		}
	}
	if svcMarked != 1 || ownMarked != 1 {
		t.Fatalf("标记错误: svc=%d own=%d", svcMarked, ownMarked)
	}
	// Clone 独立：追加不影响原库
	c := l.Clone()
	c.AddServiceRoots([]string{t.TempDir()})
	if len(l.Roots()) != 2 || len(c.Roots()) != 3 {
		t.Fatalf("Clone 应独立: l=%d c=%d", len(l.Roots()), len(c.Roots()))
	}
}

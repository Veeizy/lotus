package app

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
)

// mdnsSpamFilter 对 zeroconf 在 Windows 上反复打印的组播接口设置失败日志做去刷屏：
//   - 首次出现：升级为 Warn 并附说明（可见、可诊断）；
//   - 之后出现：降级为 Debug（Info 级别下不再刷屏）。
//
// 背景：zeroconf 每应答一次 mDNS 查询，就会对本机每个网卡尝试设置组播接口；
// Windows 上虚拟网卡 / IPv4 接口被设到 IPv6 套接字等场景会返回 EINVAL 并刷日志。
// 该失败只影响「局域网自动发现」的单个网卡出口，不影响浏览与节点互联。
type mdnsSpamFilter struct {
	inner slog.Handler
	seen  *atomic.Bool
}

const mdnsSpamMarker = "Failed to set multicast interface"

func NewMdnsSpamFilter(inner slog.Handler) slog.Handler {
	return &mdnsSpamFilter{inner: inner, seen: &atomic.Bool{}}
}

func (h *mdnsSpamFilter) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *mdnsSpamFilter) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, mdnsSpamMarker) {
		if h.seen.Swap(true) {
			r.Level = slog.LevelDebug
		} else {
			r.Level = slog.LevelWarn
			r.Add("说明", "局域网自动发现的组播接口设置失败（常见于虚拟网卡/多网卡）；不影响浏览与节点互联，重复日志已静默；如不需要局域网发现可加 --no-mdns")
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *mdnsSpamFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &mdnsSpamFilter{inner: h.inner.WithAttrs(attrs), seen: h.seen}
}

func (h *mdnsSpamFilter) WithGroup(name string) slog.Handler {
	return &mdnsSpamFilter{inner: h.inner.WithGroup(name), seen: h.seen}
}

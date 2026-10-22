package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// readBody 读取 JSON 请求体（带大小上限）。
func readBody(r *http.Request, cap int64, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, cap+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > cap {
		return fmt.Errorf("请求体过大")
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// extOf 小写扩展名（含点）。
func extOf(p string) string {
	return lower(filepath.Ext(p))
}

func lower(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 32
		}
	}
	return string(out)
}

// b64u base64url 无填充。
func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// osStat 包装（便于统一错误处理）。
func osStat(p string) (os.FileInfo, error) { return os.Stat(p) }

// fmtFprintf 轻包装（SSE 输出）。
func fmtFprintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// newTicker / withTimeout* 时间辅助（集中便于测试调整）。
func newTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }

func withTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func withTimeoutCtx(d time.Duration) context.Context {
	return context.Background()
}

// Package e2e：多进程端到端测试（WP8）。
// 场景：基础互联 / 链路缓存与离线 / 网络环境（高延迟·多层级·嫁接·分区·重启）。
// 每个用例拉起真实 lotus.exe 进程，通过管理面 HTTP API 断言（与旧 5 套 e2e 同思路）。
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var binPath string

func TestMain(m *testing.M) {
	// 固定编译输出目录：防火墙规则按 exe 完整路径放行，随机目录会导致每次跑 e2e 都弹放行提示。
	dir := filepath.Join(os.TempDir(), "lotus-e2e-bin")
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "清理 %s 失败（上一次 e2e 的 lotus.exe 可能还在运行）: %v\n", dir, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "lotus.exe")
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/picd")
	cmd.Env = append(os.Environ(), "GOARCH=amd64", "GOTOOLCHAIN=local")
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "构建 lotus 失败: %v\n%s\n", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

/* ---------------- 节点进程管理 ---------------- */

type e2eNode struct {
	t        *testing.T
	Name     string
	Dir      string
	Gallery  string
	HTTPPort int
	QUICPort int
	BaseURL  string
	opt      nodeOpt
	cmd      *exec.Cmd
	LogBuf   *strings.Builder
	procLog  string
}

var portCursor = 3600

func freePort() int {
	for {
		portCursor++
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", portCursor))
		if err == nil {
			ln.Close()
			return portCursor
		}
	}
}

// freeUDPPort QUIC 端口用 UDP 探测分配（TCP 空闲不代表 UDP 空闲；
// Windows 下刚被杀进程的 UDP 端口会短暂滞留）。
func freeUDPPort() int {
	for {
		portCursor++
		s, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: portCursor})
		if err == nil {
			s.Close()
			return portCursor
		}
	}
}

type nodeOpt struct {
	delayMS  int
	freshSec int
	name     string
}

func startNode(t *testing.T, name string, opt nodeOpt) *e2eNode {
	t.Helper()
	dir, err := os.MkdirTemp("", "picd-node-"+name+"-")
	if err != nil {
		t.Fatal(err)
	}
	gallery := filepath.Join(dir, "gallery")
	if err := os.MkdirAll(gallery, 0o755); err != nil {
		t.Fatal(err)
	}
	n := &e2eNode{t: t, Name: name, Dir: dir, Gallery: gallery, opt: opt,
		HTTPPort: freePort(), QUICPort: freeUDPPort()}
	n.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", n.HTTPPort)
	n.writeConfig(opt)
	n.spawn()
	t.Cleanup(func() { n.stop(); os.RemoveAll(dir) })
	return n
}

func (n *e2eNode) writeConfig(opt nodeOpt) {
	fresh := opt.freshSec
	cfg := map[string]any{
		"port":  n.HTTPPort,
		"host":  "127.0.0.1",
		"paths": []string{n.Gallery},
		"net": map[string]any{
			"name":        opt.name,
			"share":       true,
			"cacheServe":  true,
			"concurrency": 5,
			"port":        n.QUICPort,
			"hubs":        []string{},
			"known":       map[string]string{},
			"cacheDir": "net-cache",
		},
	}
	_ = fresh
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(n.Dir, "config.json"), b, 0o644); err != nil {
		n.t.Fatal(err)
	}
}

func (n *e2eNode) spawn(extra ...string) {
	for attempt := 0; ; attempt++ {
		args := []string{"--config", filepath.Join(n.Dir, "config.json"), "--no-mdns", "--no-auto-join"}
		if n.freshSec() > 0 {
			args = append(args, "--fresh-sec", fmt.Sprint(n.freshSec()))
		}
		if n.delayMS() > 0 {
			args = append(args, "--delay-ms", fmt.Sprint(n.delayMS()))
		}
		args = append(args, extra...)
		n.LogBuf = &strings.Builder{}
		cmd := exec.Command(binPath, args...)
		cmd.Env = append(os.Environ(), "PIC_LOG=debug")
		cmd.Stdout = n.LogBuf
		cmd.Stderr = n.LogBuf
		if err := cmd.Start(); err != nil {
			n.t.Fatalf("启动 %s 失败: %v", n.Name, err)
		}
		n.cmd = cmd
		// 等待 HTTP 就绪
		deadline := time.Now().Add(20 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			resp, err := http.Get(n.BaseURL + "/api/roots")
			if err == nil {
				resp.Body.Close()
				ready = true
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
		if ready {
			return
		}
		n.stop()
		logs := n.LogBuf.String()
		// 端口被残留进程占用（Windows UDP 端口短暂滞留）→ 换端口重试
		if attempt < 3 && strings.Contains(logs, "bind") {
			n.HTTPPort = freePort()
			n.QUICPort = freeUDPPort()
			n.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", n.HTTPPort)
			n.writeConfig(n.opt)
			continue
		}
		n.t.Fatalf("%s 启动超时\n日志: %s", n.Name, logs)
	}
}

func (n *e2eNode) freshSec() int {
	v := os.Getenv("PIC_FRESH_SEC")
	if v == "" {
		return 8 // e2e 全局用较短的新鲜度窗口加速收敛
	}
	i := 0
	fmt.Sscanf(v, "%d", &i)
	return i
}

func (n *e2eNode) delayMS() int { return 0 }

func (n *e2eNode) stop() {
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
		_, _ = n.cmd.Process.Wait()
		n.cmd = nil
	}
}

// restart 杀掉进程并用同一目录重启（身份/缓存延续）。
func (n *e2eNode) restart(extra ...string) {
	n.stop()
	time.Sleep(500 * time.Millisecond)
	n.spawn(extra...)
}

/* ---------------- HTTP 客户端 ---------------- */

func (n *e2eNode) get(path string, timeout time.Duration) (int, []byte) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(n.BaseURL + path)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (n *e2eNode) post(path string, body any, timeout time.Duration) (int, map[string]any) {
	client := &http.Client{Timeout: timeout}
	b, _ := json.Marshal(body)
	resp, err := client.Post(n.BaseURL+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func (n *e2eNode) getJSON(path string, timeout time.Duration) (int, map[string]any) {
	code, b := n.get(path, timeout)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return code, m
}

func (n *e2eNode) getBytes(path string, timeout time.Duration) (int, []byte) {
	return n.get(path, timeout)
}

func (n *e2eNode) state() map[string]any {
	_, m := n.getJSON("/api/net/state", 10*time.Second)
	return m
}

func (n *e2eNode) join(addrs ...string) map[string]any {
	_, m := n.post("/api/net/join", map[string]any{"addrs": addrs}, 30*time.Second)
	return m
}

func (n *e2eNode) memberIDs() []string {
	m := n.state()
	members, _ := m["members"].([]any)
	out := []string{}
	for _, x := range members {
		if mm, ok := x.(map[string]any); ok {
			if id, ok := mm["id"].(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

func (n *e2eNode) selfID() string {
	m := n.state()
	me, _ := m["me"].(map[string]any)
	id, _ := me["id"].(string)
	return id
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func filePut(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func q(s string) string { return strings.ReplaceAll(s, "\\", "\\\\") }

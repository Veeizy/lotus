//go:build linux

// Linux 服务注册：systemd（root 写系统级单元，普通用户写 user 单元）。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const serviceName = "lotus-server"

func isWindowsService() bool { return false }

// runAsService Linux 无 SCM 概念：systemd 直接跑 `run` 子命令，不应走到这里。
func runAsService(args []string) { _ = runDaemon(parseRunArgs(args), nil) }

func parseRunArgs(args []string) daemonOpts {
	var o daemonOpts
	fs := newRunFlags(&o)
	_ = fs.Parse(args)
	return o
}

func systemctl(scope string, args ...string) ([]byte, error) {
	full := append([]string{scope}, args...)
	out, err := exec.Command("systemctl", full...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("systemctl %s: %v\n%s", strings.Join(full, " "), err, out)
	}
	return out, nil
}

// scopeFor 返回 systemd 作用域与单元文件路径（root=系统级，否则 user 级）。
func scopeFor() (scope, unitDir string, err error) {
	if os.Geteuid() == 0 {
		return "", "/etc/systemd/system", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return "--user", filepath.Join(home, ".config", "systemd", "user"), nil
}

func unitContent(exe, cfgPath string) string {
	return fmt.Sprintf(`[Unit]
Description=lotus-server 中转服务（图片网络）
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, exe, cfgPath)
}

func unitUserContent(exe, cfgPath string) string {
	return fmt.Sprintf(`[Unit]
Description=lotus-server 中转服务（图片网络）
After=network-online.target

[Service]
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe, cfgPath)
}

func installService(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	scope, unitDir, err := scopeFor()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unit := filepath.Join(unitDir, serviceName+".service")
	content := unitContent(exe, cfgPath)
	if scope == "--user" {
		content = unitUserContent(exe, cfgPath)
	}
	if err := os.WriteFile(unit, []byte(content), 0o644); err != nil {
		return err
	}
	if _, err := systemctl(scope, "daemon-reload"); err != nil {
		return err
	}
	hint := ""
	if scope == "--user" {
		hint = "\n提示: 无桌面会话运行（如服务器 SSH 后退出仍要运行）需执行一次: sudo loginctl enable-linger $USER"
	}
	fmt.Printf("服务已注册（systemd %s）：%s\n  单元: %s\n启动服务: lotus-server start%s\n",
		map[string]string{"": "system", "--user": "user"}[scope], serviceName, unit, hint)
	return nil
}

func uninstallService(cfgPath string) error {
	scope, unitDir, err := scopeFor()
	if err != nil {
		return err
	}
	unit := filepath.Join(unitDir, serviceName+".service")
	_, _ = systemctl(scope, "stop", serviceName)
	_, _ = systemctl(scope, "disable", serviceName)
	if err := os.Remove(unit); err != nil {
		return fmt.Errorf("单元文件移除失败: %w", err)
	}
	_, _ = systemctl(scope, "daemon-reload")
	fmt.Printf("服务已移除：%s（%s）\n", serviceName, unit)
	return nil
}

func startService(cfgPath string) error {
	scope, unitDir, err := scopeFor()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(unitDir, serviceName+".service")); err != nil {
		return fmt.Errorf("服务未注册（先运行 lotus-server install）")
	}
	if _, err := systemctl(scope, "enable", "--now", serviceName); err != nil {
		return err
	}
	fmt.Printf("服务已启动（开机自启已开启）：%s\n查看状态: lotus-server status\n", serviceName)
	return nil
}

func stopService(cfgPath string) error {
	scope, _, err := scopeFor()
	if err != nil {
		return err
	}
	if _, err := systemctl(scope, "stop", serviceName); err != nil {
		return err
	}
	fmt.Printf("服务已停止：%s\n", serviceName)
	return nil
}

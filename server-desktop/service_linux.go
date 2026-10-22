//go:build linux

// 系统服务控制（Linux）：user 级 systemd 无需提权，直接执行 CLI 子命令并回传输出。
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const serviceName = "lotus-server"

func unitPath() string {
	if os.Geteuid() == 0 {
		return filepath.Join("/etc/systemd/system", serviceName+".service")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "systemd", "user", serviceName+".service")
}

func serviceState() (installed, running bool) {
	unit := unitPath()
	if unit == "" {
		return false, false
	}
	if _, err := os.Stat(unit); err != nil {
		return false, false
	}
	installed = true
	scope := ""
	if os.Geteuid() != 0 {
		scope = "--user"
	}
	out, err := exec.Command("systemctl", scope, "is-active", serviceName).Output()
	running = err == nil && strings.TrimSpace(string(out)) == "active"
	return installed, running
}

func requestServiceAction(exe, action, cfgPath string) (string, error) {
	out, err := exec.Command(exe, action, "--config", cfgPath).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s 失败: %v\n%s（system 级单元需要 root 运行本控制台）", action, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func spawnDaemon(exe, cfgPath string) error {
	cmd := exec.Command(exe, "run", "--config", cfgPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// browseFolder 弹出原生文件夹选择框（zenity；取消 = exit 1）。
func browseFolder() (dir string, canceled bool, err error) {
	out, err := exec.Command("zenity", "--file-selection", "--directory").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", true, nil
		}
		return "", false, fmt.Errorf("文件夹选择器启动失败（需要 zenity）: %v", err)
	}
	dir = strings.TrimSpace(string(out))
	if dir == "" {
		return "", true, nil
	}
	return dir, false, nil
}

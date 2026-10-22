//go:build !windows && !linux

// 其余平台：无系统服务注册，前台拉起 daemon 仍可用。
package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

func serviceState() (installed, running bool) { return false, false }

func requestServiceAction(exe, action, cfgPath string) (string, error) {
	return "", fmt.Errorf("此平台不支持系统服务注册")
}

func spawnDaemon(exe, cfgPath string) error {
	cmd := exec.Command(exe, "run", "--config", cfgPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func browseFolder() (dir string, canceled bool, err error) {
	return "", false, fmt.Errorf("此平台不支持文件夹选择，请手动输入路径")
}

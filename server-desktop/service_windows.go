//go:build windows

// 系统服务控制（Windows）：查询走 SCM（非提权可查），写操作经 UAC 提权
// 调用同目录 lotus-server CLI；前台拉起 daemon 用 DETACHED_PROCESS。
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const serviceName = "lotus-server"

// serviceState 查询服务是否已安装/运行中。
// 用只读权限（SC_MANAGER_CONNECT + SERVICE_QUERY_STATUS）——普通用户即可；
// mgr.Connect 申请 ALL_ACCESS 非提权必失败，会被误判成"未注册"。
func serviceState() (installed, running bool) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, false
	}
	defer windows.CloseServiceHandle(m)
	s, err := windows.OpenService(m, windows.StringToUTF16Ptr(serviceName), windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false, false
	}
	defer windows.CloseServiceHandle(s)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(s, &st); err != nil {
		return true, false
	}
	return true, st.CurrentState == windows.SERVICE_RUNNING || st.CurrentState == windows.SERVICE_START_PENDING
}

// requestServiceAction 提权执行 CLI 子命令（弹 UAC）；输出拿不到，
// 结果由前端随后的状态轮询确认。
func requestServiceAction(exe, action, cfgPath string) (string, error) {
	args := syscall.EscapeArg(action) + " --config " + syscall.EscapeArg(cfgPath)
	if err := windows.ShellExecute(0,
		windows.StringToUTF16Ptr("runas"),
		windows.StringToUTF16Ptr(exe),
		windows.StringToUTF16Ptr(args),
		nil, windows.SW_SHOW); err != nil {
		return "", fmt.Errorf("提权执行失败（在 UAC 弹窗点了取消？）: %v", err)
	}
	return "已请求执行 " + action + "（提权窗口完成后自动生效）", nil
}

// spawnDaemon 后台拉起 lotus-server run（无窗口）。
func spawnDaemon(exe, cfgPath string) error {
	cmd := exec.Command(exe, "run", "--config", cfgPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %v", err)
	}
	go func() { _ = cmd.Wait() }() // 收尸，不阻塞
	return nil
}

// browseFolder 弹出原生文件夹选择框（WinForms FolderBrowserDialog，
// 由独立 PowerShell 进程承载，HideWindow 避免闪控制台）。
// 取消 = exit 2。选中路径经 UTF-8（无 BOM）临时文件回传——重定向管道的
// 默认输出编码是系统 OEM 代码页（中文 Windows 为 GBK），中文路径会乱码。
func browseFolder() (dir string, canceled bool, err error) {
	tmp, err := os.CreateTemp("", "lotus-browse-*.txt")
	if err != nil {
		return "", false, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	outPath := strings.ReplaceAll(tmp.Name(), "'", "''") // PowerShell 单引号转义
	script := `Add-Type -AssemblyName System.Windows.Forms | Out-Null; ` +
		`$d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$d.Description = '选择要共享的文件夹'; ` +
		`if ($d.ShowDialog((New-Object System.Windows.Forms.Form)) -ne [System.Windows.Forms.DialogResult]::OK) { exit 2 }; ` +
		`[IO.File]::WriteAllText('` + outPath + `', $d.SelectedPath, (New-Object System.Text.UTF8Encoding($false)))`
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 2 {
			return "", true, nil
		}
		return "", false, fmt.Errorf("文件夹选择器启动失败: %v", err)
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return "", false, err
	}
	dir = strings.TrimSpace(string(b))
	if dir == "" {
		return "", true, nil
	}
	return dir, false, nil
}

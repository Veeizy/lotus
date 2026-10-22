//go:build windows

// exe 以 -ldflags "-H windowsgui" 构建为 GUI 子系统；--debug 时把日志接到控制台，
// 正常模式致命错误弹窗显示（避免双击启动失败时进程无声消失）。
package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procAllocConsole     = kernel32.NewProc("AllocConsole")
)

const attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS

func setupDebugConsole() {
	if r, _, _ := procGetConsoleWindow.Call(); r != 0 {
		return // 已连接控制台
	}
	if !stdHandleValid(windows.STD_OUTPUT_HANDLE) && !stdHandleValid(windows.STD_ERROR_HANDLE) {
		if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
			if r, _, _ := procAllocConsole.Call(); r == 0 {
				return
			}
		}
	}
	if h, err := windows.CreateFile(windows.StringToUTF16Ptr("CONOUT$"),
		windows.GENERIC_WRITE, windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0); err == nil {
		_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, h)
		_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, h)
		f := os.NewFile(uintptr(h), "CONOUT$")
		os.Stdout = f
		os.Stderr = f
	}
	if h, err := windows.CreateFile(windows.StringToUTF16Ptr("CONIN$"),
		windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, 0, 0); err == nil {
		_ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, h)
		os.Stdin = os.NewFile(uintptr(h), "CONIN$")
	}
}

func stdHandleValid(id uint32) bool {
	h, err := windows.GetStdHandle(id)
	return err == nil && h != 0 && h != windows.InvalidHandle
}

func showErrorMessageBox(format string, args ...any) {
	text := windows.StringToUTF16Ptr("错误: " + fmt.Sprintf(format, args...))
	_, _ = windows.MessageBox(0, text,
		windows.StringToUTF16Ptr("lotus-server 控制台"),
		windows.MB_OK|windows.MB_ICONERROR)
}

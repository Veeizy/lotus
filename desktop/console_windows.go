//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// exe 以 -ldflags "-H windowsgui" 构建为 GUI 子系统，系统启动时不再分配控制台窗口；
// --debug 时用下面的 API 把日志接到控制台（终端启动→复用父终端，双击启动→新开一个）。
var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procAllocConsole     = kernel32.NewProc("AllocConsole")
)

// ATTACH_PARENT_PROCESS = (DWORD)-1
const attachParentProcess = ^uintptr(0)

// setupDebugConsole 把 stdout/stderr/stdin 接到控制台，供 --debug 模式查看日志。
// 输出已被重定向时（如 lotus-desktop.exe --debug > log.txt）不动原句柄，直接写文件。
func setupDebugConsole() {
	if r, _, _ := procGetConsoleWindow.Call(); r != 0 {
		return // 已连接控制台（如控制台子系统构建）
	}
	if !stdHandleValid(windows.STD_OUTPUT_HANDLE) && !stdHandleValid(windows.STD_ERROR_HANDLE) {
		if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
			if r, _, _ := procAllocConsole.Call(); r == 0 {
				return // 复用父终端与新开控制台都失败，保持无控制台运行
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

// showErrorMessageBox 在无控制台的正常模式下弹窗显示致命错误，避免静默退出。
func showErrorMessageBox(format string, args ...any) {
	text := windows.StringToUTF16Ptr("错误: " + fmt.Sprintf(format, args...))
	_, _ = windows.MessageBox(0, text,
		windows.StringToUTF16Ptr("图片浏览服务 - lotus"),
		windows.MB_OK|windows.MB_ICONERROR)
}

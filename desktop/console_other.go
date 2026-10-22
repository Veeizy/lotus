//go:build !windows

package main

import "fmt"

// 非 Windows 占位：桌面版仅面向 Windows（WebView2），其余平台无法走到这里。
func setupDebugConsole() {}

func showErrorMessageBox(format string, args ...any) {
	fmt.Printf("错误: "+format+"\n", args...)
}

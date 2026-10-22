//go:build !windows

package main

import "fmt"

func setupDebugConsole() {}

func showErrorMessageBox(format string, args ...any) {
	fmt.Printf("错误: "+format+"\n", args...)
}

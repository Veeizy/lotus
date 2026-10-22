//go:build !windows && !linux

// 其余平台（如 darwin）：服务注册暂不支持，run 前台运行不受影响。
package main

func isWindowsService() bool { return false }

func runAsService(args []string) {}

func installService(cfgPath string) error   { return errUnsupported }
func uninstallService(cfgPath string) error { return errUnsupported }
func startService(cfgPath string) error     { return errUnsupported }
func stopService(cfgPath string) error      { return errUnsupported }

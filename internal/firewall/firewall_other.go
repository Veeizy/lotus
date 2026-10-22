//go:build !windows

// 非 Windows：无 Windows 防火墙概念，全部退化为「不支持」，UI 据此隐藏入口。
package firewall

// Check 恒返回不支持。
func Check(exe string, needs []Need) Status {
	st := Status{Supported: false, Reason: "仅 Windows 提供防火墙状态检测", Exe: exe, Allowed: true}
	return st
}

// EnsureRules 非 Windows 无实现。
func EnsureRules(exe, baseName string, protos []string) error {
	return errUnsupported
}

// Elevate 非 Windows 无实现。
func Elevate(selfExe, program, baseName string, protos []string) error {
	return errUnsupported
}

// HandleInstallArgs 非 Windows 不识别该子命令。
func HandleInstallArgs(args []string) bool { return false }

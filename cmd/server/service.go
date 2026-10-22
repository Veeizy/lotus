// 系统服务注册的共享入口：install/uninstall/start/stop 子命令。
// 平台实现见 service_windows.go（SCM）、service_linux.go（systemd）、service_other.go。
package main

import (
	"errors"
	"flag"
	"path/filepath"
)

func cfgFlagFn(name string, fn func(cfgPath string) error) func([]string) error {
	return func(args []string) error {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		cfgPath := fs.String("config", defaultCfgName, "")
		fs.Usage = usage
		if err := fs.Parse(args); err != nil {
			return err
		}
		abs, err := filepath.Abs(*cfgPath)
		if err != nil {
			return err
		}
		return fn(abs)
	}
}

func cmdInstall(args []string) error {
	return cfgFlagFn("install", installService)(args)
}

func cmdUninstall(args []string) error {
	return cfgFlagFn("uninstall", uninstallService)(args)
}

func cmdStart(args []string) error {
	return cfgFlagFn("start", startService)(args)
}

func cmdStop(args []string) error {
	return cfgFlagFn("stop", stopService)(args)
}

var errUnsupported = errors.New("此平台暂不支持系统服务注册（可用 lotus-server run 前台运行，或自行配置 systemd/任务计划）")

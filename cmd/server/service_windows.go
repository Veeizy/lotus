//go:build windows

// Windows 服务注册：golang.org/x/sys/windows/svc(mgr)。
// 服务以「<exe> run --config <绝对路径>」常驻，SCM 发停止请求时优雅停机。
package main

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "lotus-server"

func isWindowsService() bool {
	r, _ := svc.IsWindowsService()
	return r
}

// runAsService SCM 拉起入口：接管生命周期，把 args 里的 run 参数透传给 daemon。
func runAsService(args []string) {
	_ = svc.Run(serviceName, &lotusService{args: args})
}

type lotusService struct {
	args []string
}

func (l *lotusService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending, WaitHint: 15000}

	// 解析服务命令行里的 --config（缺省 server.json 在 exe 同目录）
	var o daemonOpts
	fs := newRunFlags(&o)
	_ = fs.Parse(l.args)
	o.svcLog = true // 无控制台：日志写 server.json 同目录 lotus-server.log

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runDaemon(o, stop) }()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, WaitHint: 10000}
				close(stop)
				<-done
				return false, 0
			}
		case <-done:
			return false, 0 // daemon 自行退出（端口被占等）
		}
	}
}

func connect() (*mgr.Mgr, error) { return mgr.Connect() }

// serviceBinPath 服务命令行：exe + run + --config（含空格路径按 SCM 规则转义）。
func serviceBinPath(exe, cfgPath string) string {
	return syscall.EscapeArg(exe) + " run --config " + syscall.EscapeArg(cfgPath)
}

func installService(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := connect()
	if err != nil {
		return fmt.Errorf("服务管理器连接失败（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err == nil {
		defer s.Close()
		// 已存在：更新二进制路径与参数（配置可能变了；其余字段一并刷新）
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		cfg.BinaryPathName = serviceBinPath(exe, cfgPath)
		cfg.DisplayName = "lotus-server 中转服务"
		cfg.Description = "图片网络常驻中转节点（lotus）。被动等待其他 lotus 节点接入并提供转发。"
		cfg.DelayedAutoStart = true
		if err := s.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("服务已存在，但更新失败: %w", err)
		}
		fmt.Printf("服务已更新：%s\n  二进制: %s\n  配置:   %s\n", serviceName, exe, cfgPath)
		return nil
	}
	s, err = m.CreateService(serviceName, exe, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: "lotus-server 中转服务",
		Description: "图片网络常驻中转节点（lotus）。被动等待其他 lotus 节点接入并提供转发。",
		DelayedAutoStart: true,
	}, "run", "--config", cfgPath)
	if err != nil {
		return fmt.Errorf("服务创建失败（需要管理员权限）: %w", err)
	}
	defer s.Close()
	// 崩溃自动重启：60s 窗口内最多 3 次，每次延迟 5s
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	}, 60)
	fmt.Printf("服务已注册：%s\n  二进制: %s\n  配置:   %s\n启动服务: lotus-server start（或重启系统后自动启动）\n",
		serviceName, exe, cfgPath)
	return nil
}

func uninstallService(cfgPath string) error {
	m, err := connect()
	if err != nil {
		return fmt.Errorf("服务管理器连接失败（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("服务 %s 未注册", serviceName)
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		waitStopped(s)
	}
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Printf("服务已移除：%s\n", serviceName)
	return nil
}

func startService(cfgPath string) error {
	m, err := connect()
	if err != nil {
		return fmt.Errorf("服务管理器连接失败（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("服务 %s 未注册（先运行 lotus-server install）", serviceName)
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}
	fmt.Printf("服务已启动：%s\n查看状态: lotus-server status\n", serviceName)
	return nil
}

func stopService(cfgPath string) error {
	m, err := connect()
	if err != nil {
		return fmt.Errorf("服务管理器连接失败（需要管理员权限）: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("服务 %s 未注册", serviceName)
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("停止失败: %w", err)
	}
	waitStopped(s)
	fmt.Printf("服务已停止：%s\n", serviceName)
	return nil
}

func waitStopped(s *mgr.Service) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

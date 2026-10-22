//go:build windows

// Windows 实现：COM（NetFwPolicy2）读规则、加规则；ShellExecute "runas" 提权。
package firewall

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

const (
	profileAll = 0x7FFFFFFF // NET_FW_PROFILE2_ALL（netsh profile=any 落库值）
	dirIn      = 1          // NET_FW_DIR_IN
	actionAllow = 1         // NET_FW_ACTION_ALLOW
)

// profileNames 活动配置文件掩码 → 展示名。
func profileNames(mask int) []string {
	var out []string
	for bit, name := range map[int]string{1: "域网络", 2: "专用网络", 4: "公用网络"} {
		if mask&bit != 0 {
			out = append(out, name)
		}
	}
	return out
}

// comSession 一次 COM 会话：锁 OS 线程 + 初始化，用完反初始化。
type comSession struct {
	uninit bool
}

func comBegin() *comSession {
	runtime.LockOSThread()
	s := &comSession{}
	// S_FALSE（本线程已按同模式初始化）与 RPC_E_CHANGED_MODE（已是 MTA）都视为可用，
	// 但两者都不配对 CoUninitialize（不是我们加的引用/模式）。
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err == nil {
		s.uninit = true
	}
	return s
}

func (s *comSession) end() {
	if s.uninit {
		ole.CoUninitialize()
	}
	runtime.UnlockOSThread()
}

// fwPolicy 打开 NetFwPolicy2；用完对 disp 调 Release。
func (s *comSession) fwPolicy() (disp *ole.IDispatch, err error) {
	clsid, err := ole.ClassIDFrom("HNetCfg.FwPolicy2")
	if err != nil {
		return nil, fmt.Errorf("防火墙 COM 类不可用: %v", err)
	}
	unk, err := ole.CreateInstance(clsid, ole.IID_IDispatch)
	if err != nil {
		return nil, fmt.Errorf("无法访问 Windows 防火墙（防火墙服务未运行？）: %v", err)
	}
	defer unk.Release()
	return unk.MustQueryInterface(ole.IID_IDispatch), nil
}

// fwRuleInfo 规则读取所需的最小字段。
type fwRuleInfo struct {
	name, app, ports string
	dir, action      int
	proto, profiles  int
	enabled          bool
}

// readRules 拉全量入站规则中「按程序路径精确匹配 exe」的子集。
func (s *comSession) readRules(policy *ole.IDispatch, exe string) []fwRuleInfo {
	var out []fwRuleInfo
	rulesVar, err := oleutil.GetProperty(policy, "Rules")
	if err != nil {
		return nil
	}
	defer rulesVar.Clear()
	rules := rulesVar.ToIDispatch() // 引用归 rulesVar.Clear() 释放

	_ = oleutil.ForEach(rules, func(v *ole.VARIANT) error {
		d := v.ToIDispatch()
		if d == nil {
			return nil
		}
		getStr := func(name string) string {
			p, err := oleutil.GetProperty(d, name)
			if err != nil {
				return ""
			}
			defer p.Clear()
			return p.ToString()
		}
		getInt := func(name string) int {
			p, err := oleutil.GetProperty(d, name)
			if err != nil {
				return 0
			}
			defer p.Clear()
			if i, ok := p.Value().(int32); ok {
				return int(i)
			}
			return 0
		}
		getBool := func(name string) bool {
			p, err := oleutil.GetProperty(d, name)
			if err != nil {
				return false
			}
			defer p.Clear()
			b, _ := p.Value().(bool)
			return b
		}
		app := strings.TrimSpace(getStr("ApplicationName"))
		r := fwRuleInfo{
			name:     getStr("Name"),
			app:      app,
			ports:    strings.TrimSpace(getStr("LocalPorts")),
			dir:      getInt("Direction"),
			action:   getInt("Action"),
			proto:    getInt("Protocol"),
			profiles: getInt("Profiles"),
			enabled:  getBool("Enabled"),
		}
		// 只保留与我们程序相关的入站规则（含拦截规则：拦截优先级高于放行）
		if r.dir == dirIn && r.enabled && r.app != "" && strings.EqualFold(r.app, exe) {
			out = append(out, r)
		}
		return nil
	})
	return out
}

// portCovers 规则的 LocalPorts 是否覆盖端口 p（"" / "*" = 不限；支持 "80,443" 与 "5000-5010"）。
func portCovers(ports string, p int) bool {
	if ports == "" || ports == "*" {
		return true
	}
	for _, tok := range strings.Split(ports, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" || tok == "*" {
			return true
		}
		if lo, hi, ok := strings.Cut(tok, "-"); ok {
			l, err1 := strconv.Atoi(strings.TrimSpace(lo))
			h, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 == nil && err2 == nil && p >= l && p <= h {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(tok); err == nil && n == p {
			return true
		}
	}
	return false
}

// covers 规则是否放行/拦截到给定需求（协议匹配 + 端口匹配 + 配置文件重叠）。
func (r fwRuleInfo) covers(active int, n Need) bool {
	pn := protoNum(n.Proto)
	if pn != 0 && r.proto != 256 && r.proto != pn { // 256 = NET_FW_IP_PROTOCOL_ANY
		return false
	}
	if !portCovers(r.ports, n.Port) {
		return false
	}
	return r.profiles == profileAll || r.profiles == 0 || r.profiles&active != 0
}

// Check 检测 exe 在当前活动网络下对 needs 的放行状态。
func Check(exe string, needs []Need) Status {
	st := Status{Supported: false, Exe: exe, Allowed: true}
	s := comBegin()
	defer s.end()

	policy, err := s.fwPolicy()
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	defer policy.Release()
	st.Supported = true

	active := 0
	if v, err := oleutil.GetProperty(policy, "CurrentProfileTypes"); err == nil {
		defer v.Clear()
		if i, ok := v.Value().(int32); ok {
			active = int(i)
		}
	}
	st.ActiveProfiles = profileNames(active)
	st.FirewallOn = false
	for _, bit := range []int{1, 2, 4} {
		if active&bit == 0 {
			continue
		}
		if v, err := oleutil.GetProperty(policy, "FirewallEnabled", bit); err == nil {
			defer v.Clear()
			if b, _ := v.Value().(bool); b {
				st.FirewallOn = true
			}
		}
	}

	rules := s.readRules(policy, exe)
	needsAllowed := true
	for _, n := range needs {
		ns := NeedStatus{Proto: n.Proto, Port: n.Port, Desc: n.Desc}
		for _, r := range rules {
			if !r.covers(active, n) {
				continue
			}
			if r.action == actionAllow {
				if ns.RuleName == "" {
					ns.RuleName = r.name
				}
			} else if ns.BlockBy == "" {
				ns.BlockBy = r.name
			}
		}
		ns.Allowed = ns.BlockBy == "" && ns.RuleName != ""
		st.Needs = append(st.Needs, ns)
		if !ns.Allowed {
			needsAllowed = false
		}
	}
	// 防火墙关闭 = 不拦截，视为全部放行
	st.Allowed = !st.FirewallOn || needsAllowed
	return st
}

// EnsureRules 为 exe 添加程序+协议级放行规则（任意端口、全部配置文件）。
// 须管理员权限；已有等效放行规则时不重复添加。protos 取 "tcp"/"udp" 子集。
func EnsureRules(exe, baseName string, protos []string) error {
	s := comBegin()
	defer s.end()

	policy, err := s.fwPolicy()
	if err != nil {
		return err
	}
	defer policy.Release()
	rulesVar, err := oleutil.GetProperty(policy, "Rules")
	if err != nil {
		return err
	}
	defer rulesVar.Clear()
	rules := rulesVar.ToIDispatch()

	existing := s.readRules(policy, exe)
	for _, p := range protos {
		pn := protoNum(p)
		if pn == 0 {
			continue
		}
		// 去重：已有该协议的启用入站放行规则即跳过（不限端口，等效覆盖）
		dup := false
		for _, r := range existing {
			if r.action == actionAllow && (r.proto == pn || r.proto == 256) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		clsid, err := ole.ClassIDFrom("HNetCfg.FwRule")
		if err != nil {
			return err
		}
		unk, err := ole.CreateInstance(clsid, ole.IID_IDispatch)
		if err != nil {
			return fmt.Errorf("创建规则对象失败: %v", err)
		}
		rule := unk.MustQueryInterface(ole.IID_IDispatch)
		name := baseName + "（" + strings.ToUpper(p) + "）"
		oleutil.MustPutProperty(rule, "Name", name)
		oleutil.MustPutProperty(rule, "ApplicationName", exe)
		oleutil.MustPutProperty(rule, "Protocol", int32(pn))
		oleutil.MustPutProperty(rule, "Direction", int32(dirIn))
		oleutil.MustPutProperty(rule, "Action", int32(actionAllow))
		oleutil.MustPutProperty(rule, "Enabled", true)
		oleutil.MustPutProperty(rule, "Profiles", int32(profileAll))
		_, err = oleutil.CallMethod(rules, "Add", rule)
		rule.Release()
		if err != nil {
			return fmt.Errorf("添加规则 %s 失败（须管理员权限）: %v", name, err)
		}
	}
	return nil
}

// Elevate 提权重跑自身 exe 的 --firewall-install 隐藏子命令（弹 UAC），
// 由子进程调用 EnsureRules 给 program 添加放行。结果由调用方随后轮询 Check 确认。
func Elevate(selfExe, program, baseName string, protos []string) error {
	args := "--firewall-install --program " + syscall.EscapeArg(program) +
		" --name " + syscall.EscapeArg(baseName) +
		" --protos " + syscall.EscapeArg(strings.Join(protos, ","))
	if err := windows.ShellExecute(0,
		windows.StringToUTF16Ptr("runas"),
		windows.StringToUTF16Ptr(selfExe),
		windows.StringToUTF16Ptr(args),
		nil, windows.SW_SHOW); err != nil {
		return fmt.Errorf("提权请求失败（在 UAC 弹窗点了取消？）: %v", err)
	}
	return nil
}

// HandleInstallArgs 处理 --firewall-install 隐藏子命令（提权子进程入口）。
// 命中则添加规则并退出进程（GUI 子系统无控制台，错误静默退出 1，调用方靠轮询确认）；
// 未命中返回 false，main 正常继续。
func HandleInstallArgs(args []string) bool {
	if len(args) == 0 || args[0] != "--firewall-install" {
		return false
	}
	fs := flag.NewFlagSet("firewall-install", flag.ContinueOnError)
	program := fs.String("program", "", "")
	name := fs.String("name", "", "")
	protos := fs.String("protos", "tcp,udp", "")
	_ = fs.Parse(args[1:])
	if *program == "" {
		self, err := os.Executable()
		if err != nil {
			os.Exit(1)
		}
		*program = self
	}
	if *name == "" {
		*name = strings.TrimSuffix(filepath.Base(*program), ".exe") + " 入站放行"
	}
	var ps []string
	for _, p := range strings.Split(*protos, ",") {
		if p = strings.TrimSpace(strings.ToLower(p)); p != "" {
			ps = append(ps, p)
		}
	}
	if err := EnsureRules(*program, *name, ps); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
	return true
}

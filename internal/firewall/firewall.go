// Package firewall：Windows 防火墙状态检测与一键放行（COM NetFwPolicy2）。
//
// server/dashboard/desktop 三端 UI 内嵌「防火墙状态 + 申请放行」：
//   - StatusFor 按需（协议+端口）匹配本程序的入站放行/拦截规则（按 exe 路径精确匹配，
//     含当前活动网络配置文件的重叠判断）；
//   - EnsureRules 添加放行规则——须管理员权限，只应由提权子进程调用；
//   - Elevate 用 ShellExecute "runas" 复用自身 exe 的 --firewall-install 隐藏
//     子命令弹 UAC（GUI 进程在用户会话内才能弹窗；Windows 服务在 session 0 不行）。
//
// 规则粒度：程序 + 协议级放行（不限端口）——端口在配置里可改，规则不因改端口失效。
// 非 Windows 平台全部退化为「不支持」，UI 据此隐藏入口。
package firewall

import (
	"errors"
	"strings"
)

// errUnsupported 非 Windows 平台的统一错误。
var errUnsupported = errors.New("仅 Windows 支持")

// Need 一条入站需求：协议 + 端口（0 = 任意，仅展示用）+ 展示文案。
type Need struct {
	Proto string // "tcp" | "udp"
	Port  int    // 展示用；0 = 任意端口
	Desc  string // 展示用，如 "HTTP 服务端口"
}

// NeedStatus 单条需求的放行状态。
type NeedStatus struct {
	Proto    string `json:"proto"`
	Port     int    `json:"port"`
	Desc     string `json:"desc"`
	Allowed  bool   `json:"allowed"`
	RuleName string `json:"ruleName,omitempty"` // 命中的放行规则名
	BlockBy  string `json:"blockBy,omitempty"`  // 命中的拦截规则名（拦截优先于放行）
}

// Status 防火墙检测结果（JSON 直出给前端）。
type Status struct {
	Supported      bool         `json:"supported"`
	Reason         string       `json:"reason,omitempty"` // 不支持/检测失败原因
	FirewallOn     bool         `json:"firewallOn"`       // 当前活动配置文件的防火墙开关
	ActiveProfiles []string     `json:"activeProfiles"`   // 如 ["公用网络"]
	Exe            string       `json:"exe"`              // 受检程序完整路径
	Needs          []NeedStatus `json:"needs"`
	Allowed        bool         `json:"allowed"` // 全部需求放行；防火墙关闭时恒为 true
}

// IsLoopbackHost 回环地址判定（host 为空 = 未配置，视为非回环由调用方决定）。
func IsLoopbackHost(h string) bool {
	return h == "127.0.0.1" || h == "::1" || h == "localhost"
}

// NeedsOf 按配置推导入站需求：HTTP 对外绑定（非回环 host）需要 TCP 服务端口，
// QUIC 节点互联始终需要 UDP（局域网发现 mDNS 走同一放行覆盖）。
func NeedsOf(host string, httpPort, quicPort int) []Need {
	var needs []Need
	if !IsLoopbackHost(host) && httpPort > 0 {
		needs = append(needs, Need{Proto: "tcp", Port: httpPort, Desc: "HTTP 服务端口（其他节点经此加入/访问）"})
	}
	needs = append(needs, Need{Proto: "udp", Port: quicPort, Desc: "QUIC 节点互联（含局域网发现）"})
	return needs
}

// protoNum "tcp"/"udp" → NET_FW_IP_PROTOCOL_TCP/UDP；其余 0（未知）。
func protoNum(p string) int {
	switch strings.ToLower(p) {
	case "tcp":
		return 6
	case "udp":
		return 17
	}
	return 0
}

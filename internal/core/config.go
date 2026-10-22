package core

import (
	"encoding/json"
	"os"
)

/* ---------------- config.json（PROTOCOL §6.2） ---------------- */

// 端口约定：服务端（lotus-server）与客户端（lotus / lotus-desktop）用两套默认端口，
// 同一台机器可以同时跑「常驻服务 + 客户端」双进程——客户端启动时会自动探测并加入
// 本机服务端（见 NetConfig.AutoJoinLocal）。配置文件里的显式值永远优先于默认值；
// 旧版客户端 config.json 里已落盘的 3456/3457 不做迁移（显式值语义），同机部署时
// 手工改掉客户端端口即可。
const (
	ServerHTTPPort = 3456 // lotus-server HTTP（同时是客户端自动探测本机服务的端口）
	ServerQUICPort = 3457 // lotus-server QUIC
	NodeHTTPPort   = 3466 // 客户端 HTTP 默认（原 3456 挪给服务端专用）
	NodeQUICPort   = 3467 // 客户端 QUIC 默认
)

type APIConfig struct {
	Token string `json:"token"`
	// Cors 跨域访问开关（安卓消费端从应用内资产源跨域读取 /api/*）。nil = 开。
	Cors *bool `json:"cors"`
}

// CorsEnabled CORS 是否开启（nil 视为开）。
func (a APIConfig) CorsEnabled() bool { return a.Cors == nil || *a.Cors }

type NetConfig struct {
	Name         string   `json:"name"`
	Share        bool     `json:"share"`
	CacheServe   bool     `json:"cacheServe"`
	Concurrency  int      `json:"concurrency"`
	Advertise    string   `json:"advertise"`
	Port         int      `json:"port"`  // QUIC(UDP) 监听端口
	Hubs         []string `json:"hubs"`  // 旧 uplinks 改名
	Known        map[string]string `json:"known"` // TOFU: 规范化地址 → PeerID
	CacheDir     string   `json:"cacheDir"`
	LanDiscovery bool     `json:"lanDiscovery"` // 局域网自动发现并入网（mDNS + 端口扫描；本机服务连接不受此开关控制，始终开启）
	ScanPorts    []int    `json:"scanPorts"`    // 局域网扫描的附加端口（默认端口 3456 恒在扫描列表）
}

type Config struct {
	Port  int       `json:"port"`
	Host  string    `json:"host"`
	Paths []string  `json:"paths"`
	API   APIConfig `json:"api"`
	Net   NetConfig `json:"net"`
}

// rawConfig 用于读取阶段兼容 uplinks→hubs 迁移。
type rawConfig struct {
	Port  *int     `json:"port"`
	Host  string   `json:"host"`
	Paths []string `json:"paths"`
	API   *struct {
		Token string `json:"token"`
		Cors  *bool  `json:"cors"`
	} `json:"api"`
	Net *struct {
		Name         string   `json:"name"`
		Share        *bool    `json:"share"`
		CacheServe   *bool    `json:"cacheServe"`
		Concurrency  int      `json:"concurrency"`
		Advertise    string   `json:"advertise"`
		Port         int      `json:"port"`
		Hubs         []string `json:"hubs"`
		Uplinks      []string `json:"uplinks"` // 旧键，读兼容
		Known        map[string]string `json:"known"`
		CacheDir     string   `json:"cacheDir"`
		LanDiscovery *bool    `json:"lanDiscovery"`
		ScanPorts    []int    `json:"scanPorts"`
		AutoJoinLocal *bool   `json:"autoJoinLocal"` // 旧键：已废弃（本机连接始终开启），读兼容忽略
	} `json:"net"`
}

// DefaultConfig 返回缺省配置（客户端语义：PROTOCOL §6.2/§7）。
func DefaultConfig() *Config {
	return &Config{
		Port:  NodeHTTPPort,
		Host:  "127.0.0.1",
		Paths: []string{},
		Net: NetConfig{
			Name:         "",
			Share:        true,
			CacheServe:   true,
			Concurrency:  5,
			Advertise:    "",
			Port:         NodeQUICPort,
			Hubs:         []string{},
			Known:        map[string]string{},
			CacheDir:     "net-cache",
			LanDiscovery: true,
		},
	}
}

// LoadConfig 读取配置（缺文件返回默认值，existed=false）。
// 读取 net.uplinks 视为 net.hubs（读兼容，写回只写 hubs）。
func LoadConfig(path string) (cfg *Config, existed bool, err error) {
	cfg = DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, false, nil
		}
		return cfg, false, err
	}
	var raw rawConfig
	if err := json.Unmarshal(b, &raw); err != nil {
		return cfg, true, err
	}
	if raw.Port != nil {
		cfg.Port = *raw.Port
	}
	if raw.Host != "" {
		cfg.Host = raw.Host
	}
	if raw.Paths != nil {
		cfg.Paths = raw.Paths
	}
	if raw.API != nil {
		cfg.API.Token = raw.API.Token
		cfg.API.Cors = raw.API.Cors
	}
	if raw.Net != nil {
		n := raw.Net
		if n.Name != "" {
			cfg.Net.Name = n.Name
		}
		if n.Share != nil {
			cfg.Net.Share = *n.Share
		}
		if n.CacheServe != nil {
			cfg.Net.CacheServe = *n.CacheServe
		}
		if n.Concurrency != 0 {
			cfg.Net.Concurrency = n.Concurrency
		}
		if n.Advertise != "" {
			cfg.Net.Advertise = n.Advertise
		}
		if n.Port != 0 {
			cfg.Net.Port = n.Port
		}
		if len(n.Hubs) > 0 {
			cfg.Net.Hubs = n.Hubs
		} else if len(n.Uplinks) > 0 {
			cfg.Net.Hubs = n.Uplinks // 迁移
		}
		if n.Known != nil {
			cfg.Net.Known = n.Known
		}
		if n.CacheDir != "" {
			cfg.Net.CacheDir = n.CacheDir
		}
		if n.LanDiscovery != nil {
			cfg.Net.LanDiscovery = *n.LanDiscovery
		}
		if n.ScanPorts != nil {
			cfg.Net.ScanPorts = n.ScanPorts
		}
		// net.autoJoinLocal 旧键：读兼容但忽略（本机服务连接始终开启）
	}
	cfg.Net.Concurrency = clampConcurrency(cfg.Net.Concurrency)
	return cfg, true, nil
}

// SaveConfig 写回配置（hubs 语义；保序字段与旧版兼容）。
func SaveConfig(path string, cfg *Config) error {
	if cfg.Net.Hubs == nil {
		cfg.Net.Hubs = []string{}
	}
	if cfg.Net.Known == nil {
		cfg.Net.Known = map[string]string{}
	}
	if cfg.Net.ScanPorts == nil {
		cfg.Net.ScanPorts = []int{}
	}
	if cfg.Paths == nil {
		cfg.Paths = []string{}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

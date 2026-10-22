# 架构与实现（lotus）

本文按**当前代码的实际实现**描述 lotus 的模块职责、组网协议、数据流与持久化格式，
作为阅读源码的地图。构建与使用说明见 [README](../README.md)。

```
┌─────────────┐  WebView2   ┌──────────────────┐
│ lotus-desktop│────────────│  TS 前端(嵌入exe) │
└──────┬──────┘             └──────────────────┘
       │ 127.0.0.1 HTTP (/api/*, /api/v1/*)
┌──────┴───────────────────────────────────────┐
│ internal/api  ←  internal/app（组合根）        │
│     internal/node（组网核心：成员表/五协议）    │
│        internal/p2p（libp2p QUIC 唯一封装）    │
│     internal/core（图库/缓存/队列，纯本地）     │
└──────┬───────────────────────────────────────┘
       │ QUIC/UDP 3467（客户端）/ 3457（服务端）
       ╰─── 对等节点（lotus / lotus-server / 桌面端内置服务）───
```

## 1. 模块与边界

| 包 | 职责 | 依赖边界 |
|---|---|---|
| `internal/core` | 本地图库扫描、zh 排序、链路缓存索引、下载队列、配置/状态持久化 | **不 import `net/http` 与 libp2p**（硬性架构边界） |
| `internal/p2p` | libp2p 集中封装：QUIC-only host、身份、mDNS、Circuit Relay v2、DCUtR、gossipsub、资源预算 | **全项目唯一 import libp2p 的包** |
| `internal/node` | 组网核心：成员表、宣告、五个流协议、缓存代答/转发、下载管线 | import p2p + core；**不 import `net/http`** |
| `internal/api` | localhost HTTP 管理面：`/api/*`（冻结契约）+ `/api/v1/*`（token + SSE）+ CORS | 依赖 app 暴露的引用 |
| `internal/app` | 组合根：把 core/p2p/node/crypto/stats 粘合为可运行实例 | — |
| `internal/crypto` | ECIES（`PNX1`）、身份/attestation/receipt、流式 GCM | 纯 stdlib + x/crypto |
| `internal/stats` | 运行观测：访问日志（环形 1024 条）、按对端流量、服务时间 | 全内存 |
| `internal/firewall` | Windows 防火墙状态检测与一键放行（COM NetFwPolicy2）；非 Windows 退化为"不支持" | 仅 GUI/服务端 UI 调用 |

每个入口（`cmd/picd`、`cmd/server`、`desktop`、`server-desktop`）都只是组合根的薄壳：
解析参数 → 装配 app → 起 HTTP/窗口。

## 2. 身份与密码学

- **节点身份**：libp2p Ed25519 身份 → PeerID（`12D3KooW…`）。身份私钥落盘于
  `net-state.json`（服务端为 `server-state.json`），首次启动生成，此后不变（v2 格式，
  读旧版 v1 自动兼容）。
- **图片端到端加密**：X25519 + HKDF + AES-256-GCM，密文格式 `PNX1`。
  转发节点只见密文，无法读取内容。
- **成员宣告 attestation**：节点用身份密钥签署成员信息（PeerID 语义）。
- **内容凭证 receipt**：链路缓存条目带原节点 Ed25519 签名凭证，防缓存代答时内容被偷换——
  即使经第三方节点取到缓存，也能验证内容确实出自源节点。
- **TOFU**：`net.known` 记录 地址 → 首次见到的 PeerID；此后该地址出现不同 PeerID 即视为可疑。

## 3. 节点间协议（libp2p stream）

传输为 **QUIC-only** 的 libp2p 流（复用/流控/背压/重连由 libp2p 内建）。
五个流协议，协议 ID 即版本：`/picnet/{roots,browse,times,stat,image}/1`。

| 协议 | 用途 |
|---|---|
| `roots` | 对方的根目录列表 |
| `browse` | 浏览某个目录（远端按需扫描） |
| `times` | 批量图片 mtime（用于异步重排） |
| `stat` | 取图前探测（大小/可达性，8s 预算） |
| `image` | 取图（分片流式传输；单图上限 100MB） |

- **鉴权与门控**：每个协议受 `net.share`（roots/browse/times）与 `net.cacheServe`
  （缓存代答）开关控制；管理面敏感操作仅限 loopback。
- **路由策略（请求方）**：直连优先；不可达时经已连接节点**应用层转发**
  （如 C→B→A 三级链，B 只转发密文）；libp2p 层另有 Circuit Relay v2 +
  DCUtR 打洞作为建连回退。每个节点天然具备中继能力（hub 可选常驻）。
- **缓存代答（image 特有）**：若本机链路缓存中有目标图片且凭证有效，可直接代答
  （fresh）；目标离线时允许回退过期缓存（stale），浏览不中断。源更新后旧缓存失效。

## 4. 发现与成员表

- **gossipsub 宣告**：30s 心跳广播 + 连接建立时重发脉冲；成员表以"活跃连接 **或**
  宣告新鲜（90s 窗口）"判定在线。
- **mDNS 局域网发现**：同网段 lotus 节点自动出现在前端「局域网发现」；
  Windows 多网卡下组播接口设置失败的 WARN 会被静默（不影响其他功能）。
- **子网端口扫描**：`net.lanDiscovery` 开启时每 60s 一轮，扫描子网默认端口
  （3456 恒在，`net.scanPorts` 可追加），发现的 lotus 节点自动入网；
  忽略本机 IP，每轮最多自动入网 8 个。
- **本机服务自动并入**：客户端启动时探测 `127.0.0.1:3456`（lotus-server），
  发现即按普通入网流程加入，始终开启（`--no-auto-join` 仅作测试开关）。

## 5. 下载管线

`internal/core/queue.go` + `internal/node/download.go`：下载队列支持去重、插队、
取消、进度回调；并发度 `net.concurrency`（1–16）。取图先 `stat` 探测再流式接收，
逐分片写入镜像缓存（`net-cache/<PeerID>/` 按原始层级镜像），完成后由前端可见。
取消即清进度；单图超过 100MB 拒绝。

## 6. 本地 HTTP 管理面（`internal/api`）

- **`/api/*`（旧前缀，逐字段冻结）**：前端唯一依赖的读图/浏览面，永不鉴权。
- **`/api/v1/*`（新前缀）**：Bearer token 鉴权（`api.token`，空 = 不鉴权）+ SSE 事件推送。
- **管理端点**：`/api/v1/admin/{status,traffic,access}`（lotus-server 查询面）、
  配置读写（`configapi`）、网络面板（`netapi`：成员/发现/入网/缓存）。
- **CORS**：`api.cors`（null = 开）。安卓 WebView 前端跨域 fetch 依赖此头；
  非本机访问时 `GET /api/config` 返回 `writable:false`，前端据此隐藏全部管理入口
  （服务端写接口另有 loopback 门禁兜底）。

## 7. 各入口

### cmd/picd（`lotus`）
单二进制：本地图库服务 + 组网节点。前端页面 `web/index.html` 经 go:embed 内嵌。
参数：`lotus [--host H] [--port P] <根路径…>`；无参数时读同目录 `config.json`。

### cmd/server（`lotus-server`）
常驻中转服务 CLI。子命令：`run / folders / join / status / access / traffic /
install / start / stop / uninstall`。`service_windows.go` / `service_linux.go` /
`service_other.go` 按平台实现系统服务注册（Windows SCM 自动启动 + 崩溃重启 /
systemd user 级单元）。零图库可纯中转；身份与状态独立落盘（`server-state.json`），
避免与同机客户端的身份冲突。

### desktop（`lotus-desktop`）
Windows GUI 壳：TS 前端（`desktop/frontend/`，esbuild → `desktop/dist/`）经 embed
打入单 exe，jchv/go-webview2 渲染于原生窗口。HTTP 仅绑定 127.0.0.1；
`-H windowsgui` 去 console；`--debug` 显示日志、`--headless` 不开窗口。
设置面板热改 `config.json`；深色模式跟随系统。

### server-desktop（`lotus-server-dashboard`）
lotus-server 的管理窗口：内嵌 dashboard 页面起随机端口本机 HTTP；
`/p/*` 携带 token 代理给 daemon（token 不进前端 JS）；`/ctl/*` 执行本机控制
（UAC 提权注册/启停系统服务、前台拉起 daemon）。服务状态经 SCM 查询（免提权）。

### web
`web.go`：`go:embed index.html`，lotus 内嵌前端页（单文件，零外部依赖）。

### mobile（安卓消费端）
`mobile/android/` 纯 Kotlin WebView 壳，**无 Go/gomobile/NDK**：

- 主页服务端选择：手动填写 + 历史记忆（`ServerStore`，lastUsed 倒序）+
  局域网发现（`LanScanner`：NsdManager `_picnet._udp` + 子网 3456/3466 探测，
  `/api/net/info` 验证身份）。
- 图库页（`GalleryActivity`）：WebViewAssetLoader 映射 `assets/frontend/`
  （与桌面端同一套 TS 前端构建产物），`?server=<地址>` 直连所选服务端；
  WebView `LOAD_NO_CACHE` 不落盘。
- 图片内存窗口：viewer 只在内存预取当前 ±5 共 11 张（跟随排序实时重算），出窗即释放。

## 8. 持久化与运行时文件

| 文件 | 内容 | 归属 |
|---|---|---|
| `config.json` / `server.json` | 图库根、网络开关、端口、hubs | 用户可编辑；GUI 热改写回 |
| `net-state.json` | 节点身份私钥 + TOFU 已知节点（v2） | 客户端；泄露即冒用，勿外传 |
| `server-state.json` | 同上，lotus-server 独立身份 | 服务端 |
| `net-cache/<PeerID>/` | 镜像缓存 + 凭证 | 可整目录删除，重建无害 |
| `lotus-server.log` | 服务日志（注册为系统服务时） | 诊断用 |

## 9. 测试

- **单测**：`internal/crypto`（RFC 7748/8032 官方向量、ECIES 往返/防篡改、流式 GCM
  与 stdlib 对拍）、`internal/core`（扫描/排序快照/symlink 越权/缓存键/队列/配置迁移/
  状态 v1↔v2）、`internal/p2p`（帧协议、宣告送达、中继回退、mDNS）、
  `internal/node`（转发链、代答 fresh/stale、share 门控、TOFU、中转观测）。
- **多进程 e2e**（`test/e2e/`，harness 自动构建 lotus）：入网/远程浏览/取图逐字节/
  预取进度与取消/离线回退/重启身份延续/四级链 3 跳转发/高延迟 stale 应答/
  双 hub 嫁接容灾/分区重启回网。

## 10. 构建流水线

| 目标 | 步骤 |
|---|---|
| `lotus` / `lotus-server` | `go build`（交叉编译：`GOOS=linux`；前端已内嵌无需 Node） |
| `lotus-desktop` | `desktop/frontend` npm build → `go build -ldflags "-H windowsgui"` |
| `lotus-server-dashboard` | `server-desktop/frontend` npm build → 同上 |
| 安卓 APK | `mobile/build-android.ps1`（前端 → assets → gradle；无需 Go 环境） |

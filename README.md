# lotus — 自组网的加密图片库

lotus 是一套**局域网 / 跨网段自组网的图片库互联方案**（Go 实现）：每台设备是一个对等节点，
彼此经 libp2p（QUIC）直连互通，浏览对方的图库就像浏览本地一样；图片传输端到端加密，
不可直达时经其他节点中继转发（转发者看到的只是密文）。

一个仓库产出四个二进制 + 一个安卓应用，共用同一套组网核心与协议：

| 组件 | 形态 | 定位 |
|---|---|---|
| `lotus` | 单二进制 CLI | 单机图库 Web 服务 + 组网节点（前端页面 go:embed 内嵌） |
| `lotus-desktop` | Windows 桌面应用 | TypeScript 前端 + WebView2 原生窗口（同一套前端） |
| `lotus-server` | CLI / 系统服务 | 常驻中转服务节点：可零图库纯中转，可注册为 Windows 服务 / systemd |
| `lotus-server-dashboard` | Windows 桌面应用 | lotus-server 的可视管理窗口（概览 / 访问记录 / 流量 / 管理） |
| `lotus` 安卓端 | Kotlin + WebView 壳 | 纯消费者：选服务端 → 浏览，不内嵌节点、不落磁盘缓存 |

## 特性

- **对等组网，无中心**：任意两在线成员可互访——直连优先，不可达时经已连接节点应用层转发；
  每个节点同时具备 Circuit Relay v2 中继能力，跨网段经中继互通（AutoRelay + DCUtR 打洞）。
- **端到端加密**：X25519 + HKDF + AES-256-GCM（`PNX1` 密文格式）；链路缓存带 Ed25519
  内容凭证（receipt）防偷换；目标离线后过期缓存（stale）仍可读。
- **局域网自动发现**：mDNS + 子网端口扫描，60s 周期自动入网，也可在页面网络面板手动输入地址。
- **镜像缓存**：访问其他节点时按原始层级把浏览过的目录和图片镜像到本地缓存，节点离线也能浏览。
- **手机纯消费**：安卓端不做中转、不落磁盘缓存，图片只在内存中预取当前 ±5 张。

## 快速开始

环境要求：**Go ≥ 1.23**。

```bash
go build -o lotus.exe ./cmd/picd
lotus.exe C:\图片                     # 单机使用（默认 HTTP 127.0.0.1:3456）
lotus.exe --host 0.0.0.0 "D:\壁纸"    # 局域网开放
```

- 不传路径时读取同目录 `config.json`（命令行参数优先）；端口被占用会明确报错退出。
- 首次入网监听 UDP 3457（QUIC），Windows 会弹防火墙放行提示——请放行专用网络上的 UDP。
- 浏览器打开 `http://127.0.0.1:3456`，右上角 🌐 面板可输入其他节点的 HTTP 地址入网。

## config.json

```jsonc
{
  "port": 3456,              // HTTP
  "host": "127.0.0.1",
  "paths": ["C:\\图片"],     // 图库根目录（可多个）
  "api": {
    "token": "",             // /api/v1/* 的 Bearer token（空 = 不鉴权；旧 /api/* 永不鉴权）
    "cors": null             // 跨域读取开关（null = 开，安卓消费端需要）
  },
  "net": {
    "name": "",              // 节点名（空 = node，页面面板里改）
    "share": true,           // 允许他人浏览我的图库
    "cacheServe": true,      // 允许他人取我的缓存（链路缓存）
    "concurrency": 5,        // 下载并发 1–16
    "advertise": "",         // 对外宣告 multiaddr（空 = 自动选内网 IP）
    "port": 3457,            // QUIC/UDP 端口
    "hubs": [],              // 优先认识的节点地址（host:port 或完整 multiaddr）
    "known": {},             // TOFU：地址 → PeerID（自动维护）
    "cacheDir": "net-cache", // 镜像缓存目录
    "lanDiscovery": true     // 局域网自动发现（mDNS + 端口扫描）
  }
}
```

完整示例见 [`config.example.json`](config.example.json)；节点身份存于 `net-state.json`。

## 端口约定（同机双进程）

服务端与客户端用两套默认端口，同一台机器可同时跑「lotus-server + 客户端」：

| 进程 | HTTP | QUIC | 说明 |
|---|---|---|---|
| lotus-server | 3456 | 3457 | 常驻服务端；客户端启动时自动探测此端口 |
| lotus / lotus-desktop | 3466 | 3467 | 客户端（含内置服务）默认端口 |

- 客户端启动时自动探测 `127.0.0.1:3456`，发现本机服务端即按普通入网流程加入；
  局域网内的其他 lotus 节点经 mDNS 自动出现在「局域网发现」。
- 在线判定 = 活跃连接或宣告新鲜（默认 90s 窗口，30s 心跳广播 + 连接时重发脉冲）。

## lotus-server（常驻中转服务，Windows/Linux CLI)

启动即监听（QUIC + HTTP），被动等待其他 lotus 节点接入；**零图库也可纯中转运行**
（转发链 + 链路缓存代答，转发不受 share 门控）。与 `lotus` 复用同一套组网核心，节点间完全互通。

```bash
go build -o lotus-server.exe ./cmd/server        # Linux: GOOS=linux go build（同命令）
lotus-server run --config /etc/lotus/server.json # 启动（默认 ./server.json，首次自动生成）
lotus-server folders add "D:\图片库"              # 共享文件夹（热生效）
lotus-server join 192.168.1.10:3456              # 加入网络（热生效并写入配置）
lotus-server status                              # 运行状态 + 服务时间
lotus-server access -n 100                       # 访问记录（谁、什么、多少字节、耗时）
lotus-server traffic                             # 按对端流量统计
lotus-server install                             # 注册系统服务（Windows 服务 / systemd）
lotus-server start | stop | uninstall            # 服务控制
```

- `server.json` 与桌面版 `config.json` 同构：`paths` = 共享文件夹，`net.hubs` = 启动时加入的网络。
- 查询面数据来自内建观测层：访问日志环形 1024 条（本机出图 / 缓存代答 / 转发 / 转发失败 /
  下载完成），HTTP 查询端点 `/api/v1/admin/{status,traffic,access}`（仅本机 + token）。
- `install` 在 Windows 注册 SCM 服务（自动启动 + 崩溃重启，日志落 `lotus-server.log`）；
  在 Linux 写 systemd 单元（headless 需 `loginctl enable-linger`）。

## lotus-server 控制台（Windows 桌面 dashboard）

lotus-server 的可视管理窗口：状态与服务时间、访问记录、按对端流量、文件夹/网络管理、
系统服务注册与启停。数据全部来自运行中 daemon 的本地管理面（token 由宿主进程代理注入，
不进前端）。

```bash
cd server-desktop/frontend && npm install && npm run build   # TS → ../dist/
cd ../.. && go build -ldflags "-H windowsgui" -o lotus-server-dashboard.exe ./server-desktop
lotus-server-dashboard.exe --config D:\lotus\server.json    # 与 lotus-server 同目录分发
```

- 四个页签：**概览**（服务时间/流量/请求计数/成员）、**访问记录**（5s 刷新）、
  **流量**（按对端）、**管理**（文件夹增删、加入/离开网络、系统服务注册/启停/移除）。
- 注册/启停走 UAC 提权调用同目录 `lotus-server.exe`；服务状态经 SCM 查询（免提权）。

## 桌面版（TypeScript + WebView2 原生窗口）

TypeScript 前端经 esbuild 编译打包后嵌入单 exe，用系统 WebView2 渲染在原生窗口里
（依赖 WebView2 运行时，Windows 10/11 通常自带）。功能与 Web 版一致
（双面板、排序筛选、远程浏览、网络面板），另有：

- **⚙ 设置面板**：图库文件夹增删（热生效写回 config.json）、网络缓存文件夹查看/修改/打开、
  深色背景模式（跟随系统，记忆选择）；
- **镜像缓存**：以对方的根为根，把浏览过的目录和图片按原始层级镜像到本地缓存；这些根永久
  出现在根目录页（📴 图标），节点离线也能浏览已缓存内容；
- `--headless` 仅启动服务不开窗口（调试/服务器场景）；`--debug` 查看日志。

```bash
cd desktop/frontend && npm install && npm run build   # TS → desktop/dist/
cd ../.. && go build -ldflags "-H windowsgui" -o lotus-desktop.exe ./desktop
lotus-desktop.exe "D:\照片"                            # 与 lotus 同一套 config.json
```

## 安卓消费端

`mobile/android/` 是**纯 Kotlin WebView 壳**：不内嵌 lotus 节点（无 gomobile / Go /
libp2p / NDK），手机不做图片中转、不落任何磁盘缓存，只消费服务端。

- **主页 = 服务端选择页**：手动填写 + 用过即存的扫描结果（支持重命名/删除/自动重连）
  + 局域网发现（NsdManager `_picnet._udp` + 子网 3456/3466 探测）。
- **图库页**：应用内打包桌面端同一套 TS 前端，直连所选服务端 API；
  非本机访问时服务端返回 `writable:false`，前端自动隐藏全部管理入口。
- **图片内存窗口**：只在内存预取当前 ±5 共 11 张，出窗即释放；WebView 磁盘缓存关闭。

```bash
powershell -File mobile\build-android.ps1   # 前端 → assets → gradle assembleDebug
# 产物: mobile/android/app/build/outputs/apk/debug/app-debug.apk
```

构建环境只需 JDK 17 + Android SDK + Gradle，详见 [`mobile/README-Android.md`](mobile/README-Android.md)。
服务端需为本版本或更新（`/api/*` 带 CORS 头）。

## 测试

```bash
go test ./...             # 全部：单测 + 进程内集成 + 多进程 e2e
go test ./test/e2e/ -v    # 多进程 e2e（约 2.5 分钟，自动构建 lotus）
```

覆盖：crypto（RFC 官方向量、ECIES 往返/防篡改、流式 GCM 对拍）、core（扫描/排序/缓存键/
队列/配置迁移）、p2p（帧协议、gossipsub 宣告、Circuit Relay v2 中继回退、mDNS）、
node（三级转发链、缓存代答 fresh/stale、share 门控、TOFU）、e2e（入网/远程浏览/取图逐字节/
高延迟回退/双 hub 容灾/分区重启回网）。

## 目录结构

```
cmd/picd/          lotus 入口（单二进制 Web 服务 + 组网节点）
cmd/server/        lotus-server 入口（常驻中转服务 CLI + 系统服务注册）
desktop/           lotus-desktop（WebView2 壳；frontend/ 为 TS 源码）
server-desktop/    lotus-server-dashboard（WebView2 壳；frontend/ 为 TS 源码）
internal/crypto/   ECIES/身份/attestation/receipt/流式GCM（纯 stdlib + x/crypto）
internal/core/     图库扫描/排序/缓存索引/下载队列/配置状态（不 import http 与 libp2p）
internal/p2p/      libp2p 封装（全项目唯一 import libp2p 的包）
internal/node/     组网核心：宣告/成员表/五协议/代答/转发/下载管线
internal/api/      localhost HTTP（/api/* 冻结契约 + /api/v1/* token + SSE + CORS）
internal/firewall/ Windows 防火墙状态检测与一键放行
internal/stats/    运行观测：访问日志/按对端流量/服务时间
internal/app/      组合根
web/               内嵌前端页（go:embed）
mobile/            安卓消费端（纯 Kotlin WebView 壳）
test/e2e/          多进程端到端
docs/              代码文档（架构与实现）
```

## 文档

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 架构与实现：模块职责、协议、数据流、持久化格式。
- [mobile/README-Android.md](mobile/README-Android.md) — 安卓端构建指南与常见错误对照。

## 信任模型

加密保证传输机密性；成员准入是"认识即可"（认识你的人即可与你互联），请只与信任的人组网，
或用 `share` / `cacheServe` 随时关门；管理面敏感操作仅限本机（loopback）。

## License

[MIT](LICENSE)

# lotus 安卓消费端

> 纯 Kotlin + WebView 壳：**不内嵌 lotus 节点**（无 gomobile / Go / libp2p / NDK）。
> 手机是图库的消费者——主页选择服务端（局域网扫描 / 手动填写 / 记忆列表），点开直接浏览。
> 图库页 = 应用内打包的桌面端同一套 TS 前端，经 `?server=` 直连所选服务端的 HTTP API。
> 图片缓存只进内存：预加载当前 ±5 共 11 张（头尾钳制，跟随排序），WebView 磁盘缓存关闭。

## 一键构建

```powershell
powershell -File mobile\build-android.ps1
# 产物: mobile/android/app/build/outputs/apk/debug/app-debug.apk
# 安装: adb install -r mobile/android/app/build/outputs/apk/debug/app-debug.apk
```

## 前置工具链

| 工具 | 用途 |
|---|---|
| JDK 17 | gradle 编译（`JAVA_HOME` 指向安装目录） |
| Android SDK | platforms;android-33 + build-tools（`local.properties` 写 `sdk.dir`） |
| Gradle 7.5.x | 出包（加入 `PATH`） |
| Node.js | 仅改前端时需要（assets/frontend 已入库，跳过前端步骤直接出包也行） |

**不再需要**：Go（无所谓 32/64 位）、gomobile、Android NDK、GOSUMDB、
checklinkname、javac UTF-8——旧 gomobile 方案的环境坑全部消失。

## 手工步骤（与脚本等价）

```bash
# 1) 前端（可选：仓库已带构建产物，只有改了 desktop/frontend 才需要）
(cd desktop/frontend && npm install && npm run build)
for f in index.html app.js styles.css app.css; do
  cp desktop/dist/$f mobile/android/app/src/main/assets/frontend/$f
done

# 2) APK
export JAVA_HOME=<JDK17 安装目录>
export PATH="$JAVA_HOME/bin:<gradle-7.5.x>/bin:$PATH"
export JAVA_TOOL_OPTIONS="-Dfile.encoding=UTF-8"
cd mobile/android && gradle assembleDebug --no-daemon
```

首次构建需 `mobile/android/local.properties` 写 `sdk.dir`（正斜杠路径）。

## 常见错误对照

| 错误 | 原因 | 解法 |
|---|---|---|
| 页面报 net::ERR_INVALID_RESPONSE | AssetsPathHandler 会剥掉注册前缀 `/assets/` 再当资产根路径找文件；找不到时返回 null 流 | `/assets/` 前缀必须映射到 `frontend/` 子目录（见 GalleryActivity，勿改回直接 new AssetsPathHandler） |
| webkit:1.8.0 requires ... compileSdk 34 | androidx.webkit 1.8+ 要 SDK 34 | 已锁 1.7.0，勿随意升级 |
| '-' is not a valid resource name character | 资源 id 不允许 `-` | id 一律下划线 |
| onServiceUnresolved overrides nothing | ResolveListener 无此回调 | 已删除 |
| SDK location not found | 缺 local.properties | 上文步骤 2 注释 |
| Could not GET nexus.bsdn.org/... | 机器级 init.gradle 注入死私服 | settings.gradle 已用 PREFER_SETTINGS + 阿里云镜像隔离 |
| 页面能开但一直"加载失败" | 服务端是旧版（无 CORS 头），前端跨域 fetch 被浏览器拦 | 重新编译并重启服务端（本版本的 `/api/*` 才带 CORS） |

## 服务端要求（消费端能连上的前提）

- lotus 服务端 / 桌面版 **需为本版本或更新**：`/api/*` 已带 CORS 头（`api.cors` 可关，
  默认开），安卓端前端从应用内资产源跨域读 API。
- HTTP 前端在非本机访问时进入**消费者模式**（`GET /api/config` 的 `writable:false`）：
  前端隐藏全部管理入口（网络面板、配置编辑），写接口本身也有 loopback 门禁兜底。
- 局域网扫描默认探测 **3456**（lotus-server）与 **3466**（lotus-desktop）两个端口；
  mDNS 走 lotus 的 `_picnet._udp`。非默认端口请手动添加。
- USB 调试可用 `adb reverse tcp:3456 tcp:3456` 后手动添加 `127.0.0.1:3456` 连接本机服务端。

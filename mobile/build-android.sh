#!/usr/bin/env bash
# lotus 安卓消费端一键构建（前端 -> assets -> APK）
# 需要：Node.js（仅改前端时）、JDK17、Android SDK、Gradle 7.5.1。不再需要 Go / gomobile / NDK。
set -euo pipefail
root="$(cd "$(dirname "$0")" && pwd)"
repo="$(dirname "$root")"

# 1) 前端构建（desktop/frontend -> desktop/dist）
(cd "$repo/desktop/frontend" && npm install && npm run build)

# 2) 产物拷入安卓 assets（只拷前端用到的 4 个文件）
assets="$root/android/app/src/main/assets/frontend"
mkdir -p "$assets"
for f in index.html app.js styles.css app.css; do
  cp "$repo/desktop/dist/$f" "$assets/$f"
done

# 3) APK（JDK17 + sdk.dir 见 local.properties；仓库已含 assets 产物，可跳过 1/2 直接出包）
(cd "$root/android" && gradle assembleDebug --no-daemon)

echo
echo "APK: $root/android/app/build/outputs/apk/debug/app-debug.apk"
echo "安装: adb install -r $root/android/app/build/outputs/apk/debug/app-debug.apk"

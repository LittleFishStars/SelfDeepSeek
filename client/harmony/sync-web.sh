#!/usr/bin/env bash
# 把安卓壳里的前端产物同步进鸿蒙工程的 rawfile。
#
#   bash sync-web.sh
#
# 为什么从 app/android/app/assets 同步而不是各自维护一份：
# 两边跑的是同一套 chat-demo 构建产物（含我们自己维护的 login.html），
# 双份拷贝迟早版本错位。安卓那边先 `bash sync-web.sh <chat-demo/dist>`，
# 这里再跟随同步，只有一个上游。
#
# rawfile 必须「先清空再拷」：直接 cp -a 会把上一版带哈希的 chunk 留下来，
# HAP 体积一路膨胀（安卓 APK 踩过同样的坑）。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$ROOT/../android/app/assets"
DST="$ROOT/entry/src/main/resources/rawfile"

[ -f "$SRC/index.html" ] || { echo "找不到安卓壳的前端产物: $SRC" >&2; exit 1; }

MAINS=$(find "$SRC/static/js" -maxdepth 1 -name 'main.*.js' 2>/dev/null | wc -l)
if [ "$MAINS" -ne 1 ]; then
    echo "安卓 assets 里有 $MAINS 份 main.*.js，说明上游没清理旧构建产物" >&2
    echo "  先在 app/android 里跑: bash sync-web.sh <chat-demo/dist>" >&2
    exit 1
fi

mkdir -p "$DST"
find "$DST" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
cp -a "$SRC"/. "$DST"/

echo "同步完成: $(find "$DST" -type f | wc -l) 个文件, $(du -sh "$DST" | cut -f1)"

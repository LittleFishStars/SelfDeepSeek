#!/usr/bin/env bash
# 把 chat-demo 的构建产物同步进 app/assets：
# 保留我们自己维护的 login.html / icon.png，其余全部清掉再拷，避免旧哈希 chunk 堆积。
#
#   bash sync-web.sh /path/to/chat-demo/dist
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${1:?用法: bash sync-web.sh <dist 目录>}"
[ -f "$SRC/index.html" ] || { echo "不是 chat-demo 的 dist 目录: $SRC"; exit 1; }

DST="$ROOT/app/assets"
KEEP="$ROOT/.work/keep"
mkdir -p "$KEEP" "$DST"
for f in login.html icon.png; do
  [ -f "$DST/$f" ] && mv "$DST/$f" "$KEEP/$f"
done

find "$DST" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
cp -a "$SRC"/. "$DST"/
for f in login.html icon.png; do
  [ -f "$KEEP/$f" ] && mv "$KEEP/$f" "$DST/$f"
done

echo "同步完成: $(find "$DST" -type f | wc -l) 个文件, $(du -sh "$DST" | cut -f1)"

#!/usr/bin/env bash
# 递增版本号：patch +1，VersionCode +1
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
F="$ROOT/version.txt"
NAME="$(sed -n 's/^VERSION_NAME=//p' "$F" | tail -1)"
CODE="$(sed -n 's/^VERSION_CODE=//p' "$F" | tail -1)"
IFS=. read -r major minor patch <<< "$NAME"
patch=$((patch + 1))
NEW_NAME="$major.$minor.$patch"
NEW_CODE=$((CODE + 1))
cat > "$F" <<TXT
# 每次发版改这里；VersionCode 必须单调递增，VersionName 给人看
VERSION_NAME=$NEW_NAME
VERSION_CODE=$NEW_CODE
TXT
echo "版本: $NAME($CODE) → $NEW_NAME($NEW_CODE)"

#!/usr/bin/env bash
# 把静态前端（app/assets）打成 Android APK，走 aapt2 官方流程。
#
#   bash build.sh                                   # 用 assets 里现成的接口地址
#   bash build.sh --api-origin https://api.你的域名  # 打包时改写接口地址（不改动工作树）
#   bash build.sh --version-name 1.2.0 --version-code 7
#
# 产物：dist/sds-<version>.apk（自签名，可直接侧载）
#
# 依赖（缺失时自动下载到 tools/）：apktool.jar（只用来取 aapt2）、android.jar、r8、uber-apk-signer
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

WORK="$ROOT/.work"
TOOLS="$ROOT/tools"
DIST="$ROOT/dist"
ASSETS_SRC="$ROOT/app/assets"

API_ORIGIN=""
VER_NAME=""
VER_CODE=""

while [ $# -gt 0 ]; do
    case "$1" in
        --api-origin)   API_ORIGIN="${2:?--api-origin 需要参数}"; shift 2 ;;
        --version-name) VER_NAME="${2:?}"; shift 2 ;;
        --version-code) VER_CODE="${2:?}"; shift 2 ;;
        -h|--help)      sed -n '2,12p' "$0"; exit 0 ;;
        *) echo "未知参数：$1（-h 看用法）" >&2; exit 2 ;;
    esac
done

# 版本号唯一来源是 version.txt，别在构建脚本里写死
VERSION_FILE="$ROOT/version.txt"
[ -n "$VER_NAME" ] || VER_NAME="$(sed -n 's/^VERSION_NAME=//p' "$VERSION_FILE" | tail -1)"
[ -n "$VER_CODE" ] || VER_CODE="$(sed -n 's/^VERSION_CODE=//p' "$VERSION_FILE" | tail -1)"
VER_NAME="${VER_NAME:-1.0.0}"
VER_CODE="${VER_CODE:-1}"

mkdir -p "$WORK" "$TOOLS" "$DIST"

APKTOOL="$TOOLS/apktool.jar"
ANDROID_JAR="$TOOLS/android-34.jar"
R8_JAR="$TOOLS/r8.jar"
UAS_JAR="$TOOLS/uber-apk-signer.jar"
AAPT2="$TOOLS/aapt2"
KS="$TOOLS/sds.jks"
KS_PASS="sds123456"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

fetch() {
    [ -s "$2" ] && return 0
    log "下载 $(basename "$2")"
    curl -fsSL --retry 3 -o "$2" "$1"
}

log "检查依赖"
fetch "https://github.com/iBotPeaches/Apktool/releases/download/v2.11.1/apktool_2.11.1.jar" "$APKTOOL"
fetch "https://github.com/Sable/android-platforms/raw/master/android-28/android.jar" "$ANDROID_JAR"
fetch "https://maven.google.com/com/android/tools/r8/8.3.37/r8-8.3.37.jar" "$R8_JAR"
fetch "https://github.com/patrickfav/uber-apk-signer/releases/download/v1.3.0/uber-apk-signer-1.3.0.jar" "$UAS_JAR"

if [ ! -x "$AAPT2" ]; then
    log "从 apktool 里取出 aapt2"
    unzip -o -q "$APKTOOL" prebuilt/linux/aapt2_64 -d "$WORK/aapt"
    cp "$WORK/aapt/prebuilt/linux/aapt2_64" "$AAPT2"
    chmod +x "$AAPT2"
fi

# assets 只该有一份 main.*.js：多份说明上次没清干净，会把旧 chunk 一起打进包
MAINS=$(ls "$ASSETS_SRC"/static/js/main.*.js 2>/dev/null | wc -l)
if [ "$MAINS" -gt 1 ]; then
    log "警告：app/assets 里有 $MAINS 份 main.*.js，说明没清理旧构建产物"
    echo "  先跑: bash sync-web.sh <chat-demo/dist>" >&2
    exit 1
fi

# 打包用的 assets 副本：--api-origin 只改副本，不动工作树
ASSETS="$ASSETS_SRC"
if [ -n "$API_ORIGIN" ]; then
    log "改写接口地址 → $API_ORIGIN"
    rm -rf "$WORK/assets"
    cp -a "$ASSETS_SRC" "$WORK/assets"
    ASSETS="$WORK/assets"
    python3 - "$ASSETS" "$API_ORIGIN" <<'PY'
import re, sys, pathlib
root, target = pathlib.Path(sys.argv[1]), sys.argv[2]
# 「https://host」后面紧跟 /chat 或 /auth 才是接口基址
pat = re.compile(r"https://[A-Za-z0-9.\-]+(?=/(?:chat|auth)\b)")
n = 0
for p in root.rglob("*"):
    if not p.is_file() or p.suffix not in {".js", ".html", ".htm", ".json", ".css"}:
        continue
    t = p.read_text(encoding="utf-8", errors="ignore")
    t2, k = pat.subn(target, t)
    if k:
        p.write_text(t2, encoding="utf-8")
        n += 1
if n == 0:
    sys.exit("！没找到任何接口地址，前端产物可能不对")
print(f"    {n} 个文件已改写")
PY
fi

if [ ! -f "$KS" ]; then
    log "生成签名密钥库"
    keytool -genkeypair -keystore "$KS" -storepass "$KS_PASS" -keypass "$KS_PASS" \
        -alias sds -keyalg RSA -keysize 2048 -validity 10000 \
        -dname "CN=SelfDeepSeek, O=SelfDeepSeek, C=CN" >/dev/null 2>&1
fi

log "编译 Java → dex"
rm -rf "$WORK/classes" "$WORK/dex"
mkdir -p "$WORK/classes" "$WORK/dex"
mapfile -t SRC < <(find java -name '*.java')
JAVAC="javac"
[ -n "${JAVA_HOME:-}" ] && [ -x "$JAVA_HOME/bin/javac" ] && JAVAC="$JAVA_HOME/bin/javac"
# 注意：不能给 android.jar 之外的类用 lambda —— javac 在这里找不到 LambdaMetafactory
"$JAVAC" -source 8 -target 8 -nowarn -bootclasspath "$ANDROID_JAR" \
    -d "$WORK/classes" "${SRC[@]}" 2>/dev/null || \
"$JAVAC" -source 8 -target 8 -nowarn -bootclasspath "$ANDROID_JAR" -d "$WORK/classes" "${SRC[@]}"
mapfile -t CLS < <(find "$WORK/classes" -name '*.class')
java -cp "$R8_JAR" com.android.tools.r8.D8 --release --min-api 24 \
    --lib "$ANDROID_JAR" --output "$WORK/dex" "${CLS[@]}" >/dev/null 2>&1

log "aapt2 编译 + 链接资源（assets 一并打包）"
rm -rf "$WORK/res" "$WORK/base.apk"
mkdir -p "$WORK/res"
"$AAPT2" compile --dir app/res -o "$WORK/res/"
"$AAPT2" link -o "$WORK/base.apk" -I "$ANDROID_JAR" \
    --manifest app/AndroidManifest.xml -A "$ASSETS" \
    --min-sdk-version 24 --target-sdk-version 34 \
    --version-code "$VER_CODE" --version-name "$VER_NAME" --auto-add-overlay \
    "$WORK"/res/*.flat

log "合入 classes.dex"
cp "$WORK/base.apk" "$WORK/app.apk"
mkdir -p "$WORK/zipin"
cp "$WORK/dex/classes.dex" "$WORK/zipin/classes.dex"
(cd "$WORK/zipin" && zip -X -q "$WORK/app.apk" classes.dex)

log "zipalign + 签名"
java -jar "$UAS_JAR" -a "$WORK/app.apk" --ks "$KS" --ksAlias sds \
    --ksPass "$KS_PASS" --ksKeyPass "$KS_PASS" --overwrite 2>/dev/null \
    | grep -E "zipalign|sign|verified|Successfully" || true

OUT="$DIST/sds-$VER_NAME.apk"
cp "$WORK/app.apk" "$OUT"

log "自检"
"$AAPT2" dump badging "$OUT" | head -3
# 确认接口地址真的进包了（打错域名是这里最贵的错误）
ORIGIN_IN_APK=$(unzip -p "$OUT" assets/static/js/main.*.js 2>/dev/null \
    | grep -o 'https://[A-Za-z0-9.-]\+/chat' | head -1 || true)
echo "接口地址：${ORIGIN_IN_APK:-（没读到，检查 assets）}"
ls -la "$OUT"
sha256sum "$OUT"

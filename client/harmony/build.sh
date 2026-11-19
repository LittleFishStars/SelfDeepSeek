#!/usr/bin/env bash
# 构建鸿蒙 HAP（纯命令行，Linux 上的鸿蒙 CLI 工具链）。
#
#   bash build.sh            # debug + release 都构建
#   bash build.sh debug      # 只 debug
#   bash build.sh release    # 只 release
#   bash build.sh verify     # 只校验已有产物的签名
#   bash build.sh sync       # 只同步版本号与前端 rawfile，不构建
#
# 前置：鸿蒙命令行工具链装在 ~/harmonyos/command-line-tools
#       （source ~/harmonyos/env.sh 提供 hvigorw / hdc / DEVECO_SDK_HOME）。
#
# 产物：dist/chat-demo-harmony-<version>[-<mode>].hap
#   · 有 signature/ 材料 → 出签名 HAP（可装真机）
#   · 没有 → 只出 unsigned HAP（只能看编译结果，装不上）
#
# 签名材料（不入库，放 signature/）：
#   signature/sign.env            见下方 sign_app() 的字段说明
#   signature/<storeFile>         p12（含应用私钥）
#   signature/<certFile>          AGC 下发的应用证书 .cer
#   signature/<profileFile>       AGC 下发的 profile .p7b
#
# 为什么不用 hvigor 的 signingConfigs：hvigor 会把 storePassword 当密文解密
# （格式是 AES-128-GCM，密钥在 DevEco 生成时的工作目录里），命令行没法造；
# 手工用 hap-sign-tool 直接吃明文口令，反而更可控。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

VERSION_FILE="$ROOT/../android/version.txt"
APP_JSON="$ROOT/AppScope/app.json5"
DIST="$ROOT/dist"
SIGN_DIR="$ROOT/signature"
SIGN_ENV="$SIGN_DIR/sign.env"
# 本地自签（OpenHarmony 官方测试链）：只能装模拟器/OpenHarmony 设备，
# 零售华为设备验签只认 AGC 签发的证书
DEV_SIGN_DIR="$ROOT/.tmp/dev-signature"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[!] %s\033[0m\n' "$*"; }

# ---------- 环境 ----------
if [ -z "${DEVECO_SDK_HOME:-}" ]; then
    if [ -f "$HOME/harmonyos/env.sh" ]; then
        # shellcheck disable=SC1091
        source "$HOME/harmonyos/env.sh"
    else
        echo "找不到鸿蒙工具链：~/harmonyos/env.sh" >&2
        exit 1
    fi
fi
command -v hvigorw >/dev/null || { echo "hvigorw 不在 PATH（先 source ~/harmonyos/env.sh）" >&2; exit 1; }

# hvigor 要往 ~/.hvigor 写缓存；在只读环境（沙箱 / CI）里退回工作区内目录。
# HVIGOR_USER_HOME 必须是绝对路径，否则 hvigor 忽略它。
if ! (mkdir -p "$HOME/.hvigor" && : >"$HOME/.hvigor/.write-test") 2>/dev/null; then
    export HVIGOR_USER_HOME="$ROOT/.tmp/hvigor-home/.hvigor"
    mkdir -p "$HVIGOR_USER_HOME"
    # 复用已经装好的 pnpm，避免每次联网重装
    if [ -d "$HOME/.hvigor/wrapper" ] && [ ! -e "$HVIGOR_USER_HOME/wrapper" ]; then
        ln -s "$HOME/.hvigor/wrapper" "$HVIGOR_USER_HOME/wrapper"
    fi
    warn "HOME 不可写，hvigor 缓存落在 $HVIGOR_USER_HOME"
else
    rm -f "$HOME/.hvigor/.write-test"
fi

SIGN_TOOL="$DEVECO_SDK_HOME/default/openharmony/toolchains/lib/hap-sign-tool.jar"

# ---------- 版本号 ----------
sync_version() {
    [ -f "$VERSION_FILE" ] || { echo "缺版本来源: $VERSION_FILE" >&2; exit 1; }
    local name code
    name="$(sed -n 's/^VERSION_NAME=//p' "$VERSION_FILE" | tail -1)"
    code="$(sed -n 's/^VERSION_CODE=//p' "$VERSION_FILE" | tail -1)"
    [ -n "$name" ] && [ -n "$code" ] || { echo "version.txt 里缺 VERSION_NAME/VERSION_CODE" >&2; exit 1; }
    # 只改 versionCode / versionName 两行，其余（bundleName 等）保持人写的内容
    python3 - "$APP_JSON" "$name" "$code" <<'PY'
import re, sys
path, name, code = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read()
s = re.sub(r'"versionCode"\s*:\s*\d+', '"versionCode": %s' % code, s, count=1)
s = re.sub(r'"versionName"\s*:\s*"[^"]*"', '"versionName": "%s"' % name, s, count=1)
open(path, 'w').write(s)
PY
    echo "版本: $name ($code)"
}

# ---------- 签名 ----------
# sign.env 字段（等号两边不要空格）：
#   STORE_FILE=app.p12        相对 signature/
#   STORE_PWD=xxx
#   KEY_ALIAS=xxx
#   KEY_PWD=xxx
#   CERT_FILE=app.cer         相对 signature/
#   PROFILE_FILE=app-profile.p7b
sign_ready() {
    [ -f "$SIGN_ENV" ] || return 1
    # shellcheck disable=SC1090
    . "$SIGN_ENV"
    [ -f "$SIGN_DIR/$STORE_FILE" ] && [ -f "$SIGN_DIR/$CERT_FILE" ] && [ -f "$SIGN_DIR/$PROFILE_FILE" ]
}

sign_app() {
    local unsigned="$1" signed="$2"
    java -jar "$SIGN_TOOL" sign-app \
        -mode localSign \
        -keystoreFile "$SIGN_DIR/$STORE_FILE" -keystorePwd "$STORE_PWD" \
        -keyAlias "$KEY_ALIAS" -keyPwd "$KEY_PWD" \
        -appCertFile "$SIGN_DIR/$CERT_FILE" \
        -profileFile "$SIGN_DIR/$PROFILE_FILE" \
        -inFile "$unsigned" -signAlg SHA256withECDSA \
        -outFile "$signed" >/dev/null
}

verify_app() {
    local hap="$1" tag="$2"
    local log="$ROOT/.tmp/verify-$tag.log"
    mkdir -p "$ROOT/.tmp"
    if java -jar "$SIGN_TOOL" verify-app \
        -inFile "$hap" \
        -outCertChain "$ROOT/.tmp/verify-$tag.cer" \
        -outProfile "$ROOT/.tmp/verify-$tag.p7b" >"$log" 2>&1; then
        grep -E "Digest verify result|verify: " "$log" || true
        return 0
    fi
    warn "签名校验失败，日志: $log"
    tail -5 "$log"
    return 1
}

# ---------- 本地自签（开发验证用） ----------
# 用 SDK 自带的 OpenHarmony 测试证书链签一个能通过 hap-sign-tool 校验的包。
# 用途：没有 AGC 材料时验证签名链路本身、装模拟器 / OpenHarmony 设备。
# 零售华为设备会拒绝（HapVerify: it do not come from trusted root），这是平台限制。
prepare_dev_signature() {
    local lib="$DEVECO_SDK_HOME/default/openharmony/toolchains/lib"
    [ -f "$lib/OpenHarmony.p12" ] || { echo "SDK 里没有 OpenHarmony 测试签名材料: $lib" >&2; exit 1; }
    log "生成本地自签材料（OpenHarmony 测试链）"
    mkdir -p "$DEV_SIGN_DIR"
    local bundle
    bundle="$(sed -n 's/.*"bundleName"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$APP_JSON" | head -1)"
    [ -n "$bundle" ] || { echo "app.json5 里没读到 bundleName" >&2; exit 1; }

    cp "$lib/OpenHarmony.p12" "$DEV_SIGN_DIR/app.p12"
    python3 "$ROOT/tools/make-dev-cert.py" "$lib" "$DEV_SIGN_DIR" "$bundle" || exit 1

    java -jar "$SIGN_TOOL" sign-profile -mode localSign \
        -keyAlias "openharmony application profile debug" -keyPwd 123456 \
        -profileCertFile "$lib/OpenHarmonyProfileDebug.pem" \
        -inFile "$DEV_SIGN_DIR/profile.json" -signAlg SHA256withECDSA \
        -keystoreFile "$DEV_SIGN_DIR/app.p12" -keystorePwd 123456 \
        -outFile "$DEV_SIGN_DIR/profile.p7b" >/dev/null

    printf '%s\n' \
        'STORE_FILE=app.p12' \
        'STORE_PWD=123456' \
        'KEY_ALIAS="openharmony application release"' \
        'KEY_PWD=123456' \
        'CERT_FILE=app-cert-chain.pem' \
        'PROFILE_FILE=profile.p7b' > "$DEV_SIGN_DIR/sign.env"
    warn "本地自签产物只能装模拟器/OpenHarmony 设备；零售华为设备需要 AGC 证书（放 signature/）"
}

# ---------- 构建 ----------
build_one() {
    local mode="$1" product="$2"
    log "构建 $mode（product=$product）"
    hvigorw assembleHap --mode module \
        -p product="$product" -p buildMode="$mode" --no-daemon

    local out_dir="$ROOT/entry/build/$product/outputs/default"
    local unsigned="$out_dir/entry-default-unsigned.hap"
    [ -f "$unsigned" ] || { echo "没找到产物: $unsigned" >&2; exit 1; }

    local name code hap
    name="$(sed -n 's/^VERSION_NAME=//p' "$VERSION_FILE" | tail -1)"
    mkdir -p "$DIST"
    # debug 与 release 只是构建模式不同，文件名带上模式避免互相覆盖
    hap="$DIST/chat-demo-harmony-$name-$mode.hap"

    if sign_ready; then
        log "签名 $mode"
        sign_app "$unsigned" "$hap"
        verify_app "$hap" "$mode-$name" || true
    else
        warn "没有签名材料（signature/sign.env），只留 unsigned 产物"
        hap="$DIST/chat-demo-harmony-$name-$mode-unsigned.hap"
        cp "$unsigned" "$hap"
    fi

    ls -lh "$hap"
    sha256sum "$hap"
}

case "${1:-all}" in
    debug)   sync_version; build_one debug default ;;
    release) sync_version; build_one release release ;;
    all)     sync_version; build_one debug default; build_one release release ;;
    dev)     sync_version; SIGN_DIR="$DEV_SIGN_DIR"; SIGN_ENV="$SIGN_DIR/sign.env";
             prepare_dev_signature; build_one debug default ;;
    sync)    sync_version; bash "$ROOT/sync-web.sh" ;;
    verify)
        for f in "$DIST"/*.hap; do
            [ -f "$f" ] || continue
            echo "==> $f"
            verify_app "$f" "$(basename "$f" .hap)" || true
        done
        ;;
    *) echo "用法: $0 [debug|release|all|dev|sync|verify]" >&2; exit 2 ;;
esac

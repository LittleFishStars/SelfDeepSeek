#!/usr/bin/env bash
# 拿到 AGC 下发的证书与 profile 后，一条命令完成：校验匹配 → 构建签名 → 装机。
#
#   bash tools/finish-sign.sh              # 校验 + 构建 debug 签名包
#   bash tools/finish-sign.sh --install    # 再顺手 hdc install 到已连接设备
#
# 前置：signature/ 下三件套齐全（sign.env、app.p12、app.cer、app-profile.p7b）。
# 校验的关键是「profile 里写的那张证书」跟「keystore 里的私钥」必须是同一对，
# 否则装机会报证书校验失败，而签名工具只会在很靠后的步骤才抱怨。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SIGN="$ROOT/signature"
ENV_FILE="$SIGN/sign.env"
INSTALL=0
[ "${1:-}" = "--install" ] && INSTALL=1

[ -f "$ENV_FILE" ] || { echo "缺 $ENV_FILE" >&2; exit 1; }
# shellcheck disable=SC1090
. "$ENV_FILE"

for f in "$STORE_FILE" "$CERT_FILE" "$PROFILE_FILE"; do
    [ -f "$SIGN/$f" ] || { echo "缺 signature/$f（AGC 下载后放到这里）" >&2; exit 1; }
done

if [ -z "${DEVECO_SDK_HOME:-}" ] && [ -f "$HOME/harmonyos/env.sh" ]; then
    # shellcheck disable=SC1091
    source "$HOME/harmonyos/env.sh"
fi
SIGN_TOOL="$DEVECO_SDK_HOME/default/openharmony/toolchains/lib/hap-sign-tool.jar"

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "1/4 校验证书与私钥是否同一对"
keytool -exportcert -rfc -alias "$KEY_ALIAS" \
    -keystore "$SIGN/$STORE_FILE" -storepass "$STORE_PWD" -file "$TMP/ks.pem" >/dev/null 2>&1 \
    || { echo "keystore 里没有别名 $KEY_ALIAS（keytool -list 查一下）" >&2; exit 1; }

java -jar "$SIGN_TOOL" verify-profile -inFile "$SIGN/$PROFILE_FILE" -outFile "$TMP/profile.json" >/dev/null
python3 - "$TMP/profile.json" "$TMP/profile.pem" "$ROOT/AppScope/app.json5" <<'PY'
import json, re, sys
prof = json.load(open(sys.argv[1]))
cert = prof['content']['bundle-info']['development-certificate']
open(sys.argv[2], 'w').write(cert)
local = re.search(r'"bundleName"\s*:\s*"([^"]+)"', open(sys.argv[3]).read()).group(1)
remote = prof['content']['bundle-info']['bundle-name']
if local != remote:
    sys.exit(f'包名不一致：工程 {local} / profile {remote}')
print(f'  profile 包名 {remote}，有效期 {prof["content"]["validity"]}')
PY

openssl x509 -in "$TMP/ks.pem" -pubkey -noout > "$TMP/ks.pub"
openssl x509 -in "$TMP/profile.pem" -pubkey -noout > "$TMP/profile.pub"
if ! cmp -s "$TMP/ks.pub" "$TMP/profile.pub"; then
    echo "私钥与 profile 里的证书不是同一对 —— 这个 p12 不能签这张 profile" >&2
    echo "  要么用生成 profile 时那台机器的 p12，要么重新走一次「上传 CSR → 下载证书 → 建 profile」" >&2
    exit 1
fi
echo "  匹配：profile 的证书正是 app.p12 里的私钥对应公钥"

log "2/4 构建并签名"
bash "$ROOT/build.sh" debug

HAP="$ROOT/dist/chat-demo-harmony-$(sed -n 's/^VERSION_NAME=//p' "$ROOT/../android/version.txt" | tail -1)-debug.hap"
[ -f "$HAP" ] || { echo "没找到签名产物: $HAP" >&2; exit 1; }

log "3/4 校验签名"
java -jar "$SIGN_TOOL" verify-app -inFile "$HAP" \
    -outCertChain "$TMP/vc.cer" -outProfile "$TMP/vp.p7b" 2>&1 \
    | grep -E "Digest verify result|verify: " || true

if [ "$INSTALL" = 1 ]; then
    log "4/4 安装到设备"
    source "$HOME/harmonyos/env.sh" 2>/dev/null || true
    hdc list targets
    hdc install -r "$HAP"
    hdc shell aa start -a EntryAbility -b com.example.deepseek
    echo
    echo "看日志： hdc hilog -T chat-demo"
else
    log "4/4 跳过安装（加 --install 可自动装）"
    echo "  hdc install -r $HAP"
fi

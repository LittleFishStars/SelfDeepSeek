#!/usr/bin/env bash
# 构建并部署 chat-server 到 你的服务器（YOUR_SERVER），挂在 https://chat.example.com/chat/
set -euo pipefail
HOST="${HOST:?用法：HOST=root@你的服务器 bash deploy.sh}"
KEY="${DS_API_KEY:?用法: DS_API_KEY=sk-... bash deploy.sh}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
export GOPATH="$ROOT/.gopath" GOMODCACHE="$ROOT/.gopath/pkg/mod" GOCACHE="$ROOT/.gocache"

echo "==> 编译"
mkdir -p .build
go build -trimpath -ldflags="-s -w" -o .build/chat-server .
gzip -9 -c .build/chat-server > .build/chat-server.gz

echo "==> 上传"
ssh "$HOST" 'systemctl stop chat-server 2>/dev/null || true; mkdir -p /opt/chat-server /var/lib/chat-server /var/log'
ssh "$HOST" 'gunzip -c > /opt/chat-server/chat-server && chmod 755 /opt/chat-server/chat-server' < .build/chat-server.gz

echo "==> 环境变量"
# 管理令牌：没传就从服务器已有的环境文件里沿用，没有则生成一个。
# 不能每次部署都换，否则已登录的后台会被踢下去。
ADMIN_TOKEN="${CHAT_ADMIN_TOKEN:-}"
if [ -z "$ADMIN_TOKEN" ]; then
  ADMIN_TOKEN="$(ssh "$HOST" "grep -m1 '^CHAT_ADMIN_TOKEN=' /etc/chat-server.env 2>/dev/null | cut -d= -f2-" || true)"
fi
if [ -z "$ADMIN_TOKEN" ]; then
# 先用 urandom 生成足量字节，再过滤出字母数字后截 32 位；
# 反过来先 head -c 32 再过滤会只剩 6~8 位可打印字符（曾在线上生成过 6 位令牌）。
  ADMIN_TOKEN="$(head -c 256 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9' | head -c 32)"
  ADMIN_TOKEN="${ADMIN_TOKEN:?生成管理令牌失败，请手动传入 CHAT_ADMIN_TOKEN=...}"
  echo "    已生成新的管理令牌"
fi
printf 'DS_API_KEY=%s\nCHAT_BIND=127.0.0.1\nCHAT_PORT=8083\nCHAT_DB=/var/lib/chat-server/chat.db\nCHAT_ALLOW_ANON=1\nCHAT_ADMIN_TOKEN=%s\nSMTP_HOST=%s\nSMTP_PORT=%s\nSMTP_USER=%s\nSMTP_PASS=%s\nSMTP_FROM=%s\nSMTP_TLS=%s\nTINYAUTH_ISSUER=%s\nOIDC_CLIENT_ID=%s\nOIDC_CLIENT_SECRET=%s\nOIDC_REDIRECT=%s\n' \
  "$KEY" "$ADMIN_TOKEN" "${SMTP_HOST:-127.0.0.1}" "${SMTP_PORT:-465}" "${SMTP_USER:-}" "${SMTP_PASS:-}" "${SMTP_FROM:-noreply@example.com}" "${SMTP_TLS:-implicit}" \
  "${TINYAUTH_ISSUER:-https://auth.example.com}" "${OIDC_CLIENT_ID:-chat}" "${OIDC_CLIENT_SECRET:-}" "${OIDC_REDIRECT:-https://api.example.com/auth/v1/auth/oidc/callback}" \
  | ssh "$HOST" 'umask 077; cat > /etc/chat-server.env && chmod 600 /etc/chat-server.env'
echo "    管理后台：https://chat.example.com/chat/admin/"
echo "    管理令牌：$ADMIN_TOKEN"

echo "==> 分享页共用资源"
# 分享页是静态 HTML，CSS/JS 只在 /share/assets/ 放一份，所有分享页共用
ssh "$HOST" 'mkdir -p /opt/chatweb/share/assets /opt/chatweb/share/img'
scp -q "$ROOT/share-assets/share.css" "$ROOT/share-assets/share.js" "$HOST:/opt/chatweb/share/assets/"
ssh "$HOST" 'chmod 644 /opt/chatweb/share/assets/share.css /opt/chatweb/share/assets/share.js'

echo "==> systemd"
ssh "$HOST" 'cat > /etc/systemd/system/chat-server.service' <<'UNIT'
[Unit]
Description=chat-demo backend (DeepSeek messages proxy + conversations)
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/chat-server.env
ExecStart=/opt/chat-server/chat-server
Restart=always
RestartSec=3
MemoryMax=256M
StandardOutput=append:/var/log/chat-server.log
StandardError=append:/var/log/chat-server.log

[Install]
WantedBy=multi-user.target
UNIT

echo "==> nginx 反代：/chat/ 与 /admin/（幂等）"
ssh "$HOST" 'python3 - <<"PY"
path="/etc/nginx/sites-available/deepseek"
src=open(path,encoding="utf-8").read()
changed=[]

if "/chat/" not in src:
    block = """    # chat-server（chat-demo 后端）
    location /chat/ {
        proxy_pass http://127.0.0.1:8083/;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_buffering off;
        proxy_cache off;
        chunked_transfer_encoding on;
        proxy_read_timeout 3600s;
    }

"""
    i = src.rindex("    location / {")
    src = src[:i] + block + src[i:]
    changed.append("/chat/")

# 根路径 /admin/ 归 chat-server 管理页（relay 已下线，不再需要给它留端点）。
# 已是目标形态就跳过；否则整段替换，避免重复插入。
admin_block = """    # chat-server 管理后台（内嵌单页，口令 = CHAT_ADMIN_TOKEN）。
    # 页面用相对路径请求 api/*，挂在这个前缀下即可自行找到接口。
    location /admin/ {
        proxy_pass http://127.0.0.1:8083;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_buffering off;
        proxy_cache off;
    }
"""
if "location /admin/" not in src:
    i = src.rindex("    location / {")
    src = src[:i] + admin_block + src[i:]
    changed.append("/admin/")

if changed:
    open(path,"w",encoding="utf-8").write(src)
    print("已更新：" + "、".join(changed))
else:
    print("nginx 反代已是目标状态")
PY
nginx -t && systemctl reload nginx'

ssh "$HOST" 'systemctl daemon-reload && systemctl enable chat-server >/dev/null && systemctl restart chat-server && sleep 1 && systemctl is-active chat-server'
echo "==> 健康检查"
curl -s -m 10 https://chat.example.com/chat/health || true
echo

#!/usr/bin/env bash
# 部署 TinyAuth 到 你的服务器，作为 OIDC 身份提供方（https://auth.example.com）
#
#   OIDC_SECRET=xxx bash deploy.sh              # 首次部署
#   OIDC_SECRET=xxx ADDUSER=name:password bash deploy.sh   # 追加本地用户
set -euo pipefail
HOST="${HOST:?用法：HOST=root@你的服务器 bash deploy.sh}"
DOMAIN="${DOMAIN:-auth.example.com}"
PORT="${PORT:-8010}"
OIDC_SECRET="${OIDC_SECRET:?需要 OIDC_SECRET（客户端密钥）}"
CLIENT_ID="${CLIENT_ID:-chat}"
CLIENT_NAME="${CLIENT_NAME:-DeepSeek 聊天}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

hash_pw() { python3 -c "import bcrypt,sys;print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt(10)).decode())" "$1"; }

# 现有用户表（本地文件保存，追加 ADDUSER 时合并）
USERS_FILE="$ROOT/users.txt"
touch "$USERS_FILE"
if [ -n "${ADDUSER:-}" ]; then
  name="${ADDUSER%%:*}"; pw="${ADDUSER#*:}"
  sed -i "/^${name}:/d" "$USERS_FILE"
  printf '%s:%s\n' "$name" "$(hash_pw "$pw")" >> "$USERS_FILE"
fi
[ -s "$USERS_FILE" ] || { echo "没有用户：用 ADDUSER=name:password 再跑一次"; exit 1; }

echo "==> 上传二进制"
if [ ! -s tinyauth ]; then
  echo "缺少 tinyauth 二进制（从 releases 下载 tinyauth-amd64 放到本目录）"; exit 1
fi
gzip -9 -c tinyauth > .tinyauth.gz
ssh "$HOST" 'systemctl stop tinyauth 2>/dev/null || true; mkdir -p /opt/tinyauth /var/lib/tinyauth'
ssh "$HOST" 'gunzip -c > /opt/tinyauth/tinyauth && chmod 755 /opt/tinyauth/tinyauth' < .tinyauth.gz
rm -f .tinyauth.gz

echo "==> 证书"
ssh "$HOST" "acme.sh --install-cert -d $DOMAIN --ecc \
  --key-file /etc/nginx/ssl/auth.key --fullchain-file /etc/nginx/ssl/auth.crt \
  --reloadcmd 'systemctl reload nginx' >/dev/null 2>&1 || true"

echo "==> 生成配置"
USERS_YAML=""
while IFS=: read -r n h; do
  [ -z "$n" ] && continue
  USERS_YAML="${USERS_YAML}    - \"$n:$h\"
"
done < "$USERS_FILE"
ssh "$HOST" "umask 077; mkdir -p /etc/tinyauth" <<YAML
appUrl: https://$DOMAIN
labelProvider: none
database:
  driver: sqlite
  path: /var/lib/tinyauth/tinyauth.db
server:
  port: $PORT
  address: 127.0.0.1
auth:
  subdomainsEnabled: false
  users:
$USERS_YAML
oidc:
  clients:
    chat:
      clientId: $CLIENT_ID
      clientSecret: $OIDC_SECRET
      trustedRedirectUris:
        - https://chat.example.com/chat/v1/auth/oidc/callback
YAML
ssh "$HOST" 'chmod 600 /etc/tinyauth/config.yml && printf "TINYAUTH_CONFIGFILE=/etc/tinyauth/config.yml\nTINYAUTH_ANALYTICS_ENABLED=false\n" > /etc/tinyauth.env && chmod 600 /etc/tinyauth.env'

echo "==> systemd"
ssh "$HOST" 'cat > /etc/systemd/system/tinyauth.service' <<UNIT
[Unit]
Description=TinyAuth (OIDC identity provider)
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/var/lib/tinyauth
EnvironmentFile=/etc/tinyauth.env
ExecStart=/opt/tinyauth/tinyauth
Restart=always
RestartSec=3
MemoryMax=128M
StandardOutput=append:/var/log/tinyauth.log
StandardError=append:/var/log/tinyauth.log

[Install]
WantedBy=multi-user.target
UNIT

echo "==> nginx"
ssh "$HOST" "cat > /etc/nginx/sites-available/$DOMAIN" <<NGINX
server {
    listen 80;
    server_name $DOMAIN;
    return 301 https://\$host\$request_uri;
}
server {
    listen 443 ssl;
    http2 on;
    server_name $DOMAIN;
    ssl_certificate     /etc/nginx/ssl/auth.crt;
    ssl_certificate_key /etc/nginx/ssl/auth.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    client_max_body_size 8m;

    location / {
        proxy_pass http://127.0.0.1:$PORT;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_set_header X-Forwarded-Host \$host;
        proxy_buffering off;
        proxy_read_timeout 300s;
    }
}
NGINX
ssh "$HOST" "ln -sf /etc/nginx/sites-available/$DOMAIN /etc/nginx/sites-enabled/$DOMAIN && nginx -t && systemctl reload nginx"

echo "==> 启动"
ssh "$HOST" 'systemctl daemon-reload && systemctl enable tinyauth >/dev/null && systemctl restart tinyauth && sleep 1 && systemctl is-active tinyauth'
echo "==> 发现端点"
curl -s -m 10 "https://$DOMAIN/.well-known/openid-configuration" | head -c 300
echo

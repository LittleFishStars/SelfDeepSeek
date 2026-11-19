#!/usr/bin/env bash
# 部署 PMail 到 你的服务器（域名 example.com，Web mail.example.com）
set -euo pipefail
HOST="${HOST:?用法：HOST=root@你的服务器 bash deploy.sh}"
DOMAIN="${DOMAIN:-example.com}"
WEB_DOMAIN="${WEB_DOMAIN:-mail.example.com}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

echo "==> 上传二进制"
gzip -9 -c pmail_linux_amd64 > .pmail.gz
ssh "$HOST" 'systemctl stop pmail 2>/dev/null || true; mkdir -p /opt/pmail/config /var/lib/pmail /var/log'
ssh "$HOST" 'gunzip -c > /opt/pmail/pmail && chmod 755 /opt/pmail/pmail' < .pmail.gz
rm -f .pmail.gz

echo "==> DKIM"
if [ ! -f dkim.priv ]; then
  openssl genrsa -out dkim.priv 2048 >/dev/null 2>&1
fi
openssl rsa -in dkim.priv -pubout -out dkim.pub >/dev/null 2>&1
PUB=$(grep -v -- "-----" dkim.pub | tr -d '\n')
ssh "$HOST" 'mkdir -p /opt/pmail/config/dkim && cat > /opt/pmail/config/dkim/dkim.priv && chmod 600 /opt/pmail/config/dkim/dkim.priv' < dkim.priv
echo "DKIM 公钥（DNS TXT mail._domainkey.$DOMAIN）:"
echo "v=DKIM1; k=rsa; p=$PUB" | tee dkim.txt

echo "==> 配置"
ssh "$HOST" "umask 077; cat > /opt/pmail/config/config.json" <<JSON
{
  "logLevel": "info",
  "domain": "$DOMAIN",
  "webDomain": "$WEB_DOMAIN",
  "dkimPrivateKeyPath": "config/dkim/dkim.priv",
  "sslType": "1",
  "SSLPrivateKeyPath": "/etc/nginx/ssl/mail.key",
  "SSLPublicKeyPath": "/etc/nginx/ssl/mail.crt",
  "dbDSN": "/var/lib/pmail/pmail.db",
  "dbType": "sqlite",
  "httpsEnabled": 2,
  "spamFilterLevel": 0,
  "httpPort": 8020,
  "httpsPort": 8443,
  "isInit": true
}
JSON

echo "==> systemd"
ssh "$HOST" 'cat > /etc/systemd/system/pmail.service' <<UNIT
[Unit]
Description=PMail (private mail server)
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/pmail
ExecStart=/opt/pmail/pmail
Restart=always
RestartSec=3
Environment=TZ=Asia/Shanghai
StandardOutput=append:/var/log/pmail.log
StandardError=append:/var/log/pmail.log

[Install]
WantedBy=multi-user.target
UNIT

echo "==> nginx（web 后台）"
ssh "$HOST" "cat > /etc/nginx/sites-available/$WEB_DOMAIN" <<NGINX
server {
    listen 80;
    server_name $WEB_DOMAIN;
    return 301 https://\$host\$request_uri;
}
server {
    listen 443 ssl;
    http2 on;
    server_name $WEB_DOMAIN;
    ssl_certificate     /etc/nginx/ssl/mail.crt;
    ssl_certificate_key /etc/nginx/ssl/mail.key;
    client_max_body_size 32m;
    location / {
        proxy_pass http://127.0.0.1:8020;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_read_timeout 600s;
    }
}
NGINX
ssh "$HOST" "ln -sf /etc/nginx/sites-available/$WEB_DOMAIN /etc/nginx/sites-enabled/$WEB_DOMAIN && nginx -t && systemctl reload nginx"

echo "==> 启动"
ssh "$HOST" 'systemctl daemon-reload && systemctl enable pmail >/dev/null && systemctl restart pmail && sleep 2 && systemctl is-active pmail && ss -ltnp | grep -E ":(25|465|587|8020) " | head -6'

#!/usr/bin/env bash
# 切换发信 SMTP（保留其它已配置的密钥），用法：
#   SMTP_HOST=smtpdm.aliyun.com SMTP_PORT=465 SMTP_TLS=implicit \
#   SMTP_USER=noreply@mail.example.com SMTP_PASS=xxxx SMTP_FROM=noreply@mail.example.com \
#   bash set-smtp.sh
set -euo pipefail
HOST="${HOST:?用法：HOST=root@你的服务器 bash deploy.sh}"
: "${SMTP_HOST:?需要 SMTP_HOST}"
: "${SMTP_PORT:?需要 SMTP_PORT}"
: "${SMTP_USER:?需要 SMTP_USER}"
: "${SMTP_PASS:?需要 SMTP_PASS}"

echo "==> 读出现有密钥"
DS=$(ssh "$HOST" 'grep -m1 "^DS_API_KEY=" /etc/chat-server.env | cut -d= -f2-')
OIDC=$(ssh "$HOST" 'grep -m1 "^OIDC_CLIENT_SECRET=" /etc/chat-server.env | cut -d= -f2-')

echo "==> 写入新配置"
DS_API_KEY="$DS" OIDC_CLIENT_SECRET="$OIDC" \
SMTP_HOST="$SMTP_HOST" SMTP_PORT="$SMTP_PORT" SMTP_TLS="${SMTP_TLS:-implicit}" \
SMTP_USER="$SMTP_USER" SMTP_PASS="$SMTP_PASS" SMTP_FROM="${SMTP_FROM:-$SMTP_USER}" \
bash deploy.sh

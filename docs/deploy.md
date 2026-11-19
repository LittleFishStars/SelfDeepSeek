# 部署

目标形态：一台 Linux 服务器（Debian/Ubuntu 为例，无 Docker），
nginx 收口 TLS 与路径，chat-server 走 systemd 常驻，数据落在本地 SQLite。

```
浏览器 / App ──TLS──▶ nginx ──▶ chat-server:8083 ──▶ api.deepseek.com
                        │
                        └─▶ 静态前端 / 分享页（可选，同机另一个 server 块）
```

下面用示例域名 `api.example.com`（接口）与 `chat.example.com`（静态前端）。
全程只需要改域名，其余照抄。

---

## 0. 前置

```bash
# 服务器上
apt update && apt install -y nginx sqlite3 curl
# 本机（构建机）需要 Go 1.24+ 与 docker 无关的交叉编译环境
go version
```

DNS：把 `api.example.com` 的 A 记录指向服务器公网 IP。
证书建议先签好再配 nginx（见第 4 步）。

## 1. 构建并上传

```bash
cd server/chat-server

# 交叉编译（静态二进制，服务器不需要装 Go）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o .build/chat-server .
gzip -9 -c .build/chat-server > .build/chat-server.gz

# 传上去
ssh root@api.example.com 'mkdir -p /opt/chat-server /var/lib/chat-server /opt/chatweb/share/assets'
ssh root@api.example.com 'gunzip -c > /opt/chat-server/chat-server && chmod 755 /opt/chat-server/chat-server' \
  < .build/chat-server.gz
```

沙箱执行器（可选，不装则 `run_rhai` 工具不下发）：

```bash
cd server/rhai-runner && cargo build --release
scp target/release/rhai-runner root@api.example.com:/opt/chat-server/rhai-runner
ssh root@api.example.com 'chmod 755 /opt/chat-server/rhai-runner'
```

分享页的公用样式（用了分享功能才需要）：

```bash
scp share-assets/share.css share-assets/share.js \
    root@api.example.com:/opt/chatweb/share/assets/
ssh root@api.example.com 'chmod 644 /opt/chatweb/share/assets/*'
```

## 2. 配置与启动

```bash
# 至少填 DS_API_KEY；其余按需（见 docs/configuration.md）
scp .env root@api.example.com:/etc/chat-server.env
ssh root@api.example.com 'chmod 600 /etc/chat-server.env'

scp deploy/chat-server.service root@api.example.com:/etc/systemd/system/
ssh root@api.example.com 'systemctl daemon-reload && systemctl enable --now chat-server'
ssh root@api.example.com 'systemctl is-active chat-server && tail -5 /var/log/chat-server.log'
```

> 管理令牌：设置 `CHAT_ADMIN_TOKEN=<随机 32 位>` 后 `/admin/` 才可用，留空则后台整体关闭。
> 生成方式：`head -c 256 /dev/urandom | tr -dc 'A-Za-z0-9' | head -c 32`
> （先取随机字节再过滤；反过来写会只剩几个可打印字符）。
> 令牌不要每次部署都换，否则已登录的后台会被踢下去。

## 3. nginx

```bash
scp deploy/nginx-api.conf.example root@api.example.com:/etc/nginx/sites-available/api.example.com
ssh root@api.example.com '
  sed -i "s/api\.example\.com/你的真实域名/g" /etc/nginx/sites-available/api.example.com
  ln -sf /etc/nginx/sites-available/api.example.com /etc/nginx/sites-enabled/
  nginx -t && systemctl reload nginx'
```

模板里已经处理好三件事：`/chat/` 与 `/auth/` 剥前缀、`/admin/` 保留前缀、
SSE 关闭响应缓冲。**别改成自己的路径** —— 客户端把这三组前缀写死在请求里。

自检：

```bash
curl -s https://api.example.com/health                  # {"ok":true,...}
curl -s -o /dev/null -w '%{http_code}\n' https://api.example.com/chat/v1/sync   # 401 或 200
```

## 4. 证书

```bash
apt install -y certbot python3-certbot-nginx
certbot --nginx -d api.example.com -d chat.example.com --agree-tos -m you@example.com
systemctl list-timers | grep certbot     # 自动续期由 certbot.timer 负责
```

模板里保留了 80 端口的 `/.well-known/acme-challenge/`，`--nginx` 插件能自动完成验证。

## 5. 静态前端（可选）

前端产物就是仓库里的 `client/android/app/assets/`（含自维护的 `login.html` 与 `icon.png`），
接口地址在构建时注入，所以要先把地址改成你自己的（见 [docs/clients.md](clients.md)）：

```bash
python3 tools/set-api-origin.py https://api.example.com

tar -czf /tmp/web.tgz -C client/android/app/assets . -C server/chat-server/ops/icons .
ssh root@api.example.com 'rm -rf /opt/chatweb/www && mkdir -p /opt/chatweb/www'
ssh root@api.example.com 'tar -xzf - -C /opt/chatweb/www' < /tmp/web.tgz
```

nginx 侧直接用 `deploy/nginx-api.conf.example` 末尾被注释掉的那个 server 块（去掉 `#`）。

## 6. 账号体系（三选一或组合）

### 6.1 匿名（默认，最省事）

`CHAT_ALLOW_ANON=1` 时，无令牌请求按 `UA+IP` 派生用户，一人一机互不干扰。
适合纯自用；**公网多用户场景不要开**，否则同一出口 IP 的人会共用一份会话。

### 6.2 邮箱 + 密码

填好 `SMTP_*` 就能用（注册要发 6 位验证码）。没有现成 SMTP 的话：

- 用邮箱服务商的 465 端口（QQ、163、阿里云邮件推送都行）；
- 或自建收件服务器（见 [server/mail/README.md](../server/mail/README.md)）。
  注意云主机常封 25 端口出站，自建服务器可能无法投递到外部邮箱。

### 6.3 OIDC（第三方登录）

设好 `TINYAUTH_ISSUER` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` / `OIDC_REDIRECT` 即可，
任何标准 OIDC 提供方都行。自建 TinyAuth 的步骤见 [server/auth/README.md](../server/auth/README.md)。

**回调地址必须与提供方白名单逐字一致**，且写的是浏览器看到的完整 URL：

```
https://api.example.com/auth/v1/auth/oidc/callback
                  └─ nginx 剥掉 /auth，服务端看到 /v1/auth/oidc/callback
```

改回调要一次性动三处：`/etc/chat-server.env` 的 `OIDC_REDIRECT`、
提供方配置里的 `trustedRedirectUris`、以及 nginx 的 `/auth/` 段。

## 7. 管理后台

`https://api.example.com/admin/`（admin.html 已编进二进制，无需额外部署）。
请求头带 `Authorization: Bearer $CHAT_ADMIN_TOKEN` 才放行；
页面里默认用同源相对路径请求 `/admin/api/*`，所以同域部署**不需要配 CORS**。

若要像「页面放静态站、接口在 API 域」那样分离部署，在页面里先注入：

```html
<script>window.__ADMIN_API_ORIGIN__ = 'https://api.example.com/admin/api';</script>
```

能看的东西：用户列表与 token 消耗、会话详情（Markdown 渲染，可切「看原文」）、
跨会话共享开关、用户画像与更新时间（排查「模型为什么记得这件事」用）。

## 8. 备份与升级

```bash
# 备份（SQLite 在线备份，不要直接 cp）
sqlite3 /var/lib/chat-server/chat.db ".backup /var/backups/chat-$(date +%F).db"
tar czf /var/backups/files-$(date +%F).tgz -C /var/lib/chat-server files

# 升级：重新编译上传，然后重启（数据库结构由服务端启动时自动迁移）
systemctl restart chat-server && tail -f /var/log/chat-server.log
```

附件在 `CHAT_DB` 同级的 `files/`，分享页 HTML 在 `CHAT_SHARE_DIR`，两个目录都要一起备份。

## 9. 排障速查

| 症状 | 先看这里 |
| --- | --- |
| 回答不流式、一次性蹦出来 | nginx 的 `/chat/` 段有没有 `proxy_buffering off` |
| 流里啥都没有但日志正常 | 上游被 `Accept-Encoding` 压缩了；服务端已剔除，检查是不是自己加的中间层 |
| 手机装完 App 提示网络错误 | `tools/set-api-origin.py --check`，确认包内地址与后端域名一致 |
| 后台页面 503 | `CHAT_ADMIN_TOKEN` 为空（后台整体关闭） |
| 注册报 502 | SMTP 没配或发信失败；账号会回滚，可以重试 |
| 长回答半句截断 | `CHAT_MAX_TOKENS`（默认 8192） |
| 服务整体卡住、`/health` 超时 | 大概率是单连接池里嵌套查询；见 [AGENTS.md](../AGENTS.md) |

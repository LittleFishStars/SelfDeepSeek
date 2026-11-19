# tinyauth（OIDC 身份提供方）

部署后：**https://auth.example.com**（systemd `tinyauth`，监听 `127.0.0.1:8010`，
配置 `/etc/tinyauth/config.yml`，库 `/var/lib/tinyauth/tinyauth.db`）。

自己实现的登录页 + 后续要接的第三方登录（GitHub / Google / 微信…）全部集中在这里，
业务服务只按 OIDC 对接，加登录方式不用改业务代码。

## 部署

```bash
ADDUSER=admin:<密码> OIDC_SECRET=<客户端密钥> bash deploy.sh
```

脚本会：上传 `tinyauth` 二进制到 `/opt/tinyauth/` → 装证书（acme.sh，`auth.example.com`）→
生成 `/etc/tinyauth/config.yml`（600）→ 装 systemd → 建 nginx 站点（`auth.example.com`）→ 重载。

## 配置要点

```yaml
appUrl: https://auth.example.com
labelProvider: none          # 没有 Docker，必须显式关掉 label 探测
database: { driver: sqlite, path: /var/lib/tinyauth/tinyauth.db }
server: { port: 8010, address: 127.0.0.1 }
auth:
  subdomainsEnabled: false   # 只给 OIDC 用，不要通配 cookie
  users: ["admin:$2b$10$…"]   # bcrypt 哈希
oidc:
  clients:
    chat:
      clientId: chat
      clientSecret: ta-…
      trustedRedirectUris:
        - https://chat.example.com/chat/v1/auth/oidc/callback
```

两个坑（都踩过）：

1. **`labelProvider: none`** —— 不关的话所有 API 都会 `400 failed to get context from any auth module`，
   因为它以为自己在 Docker 里找 app 标签。
2. **`trustedRedirectUris` 必须写在配置文件里** —— 用环境变量
   `TINYAUTH_OIDC_CLIENTS_CHAT_TRUSTEDREDIRECTURIS` 传不进去（多值时被丢弃），
   不在列表里的回调会 302 到 `/error?error=The provided redirect URI is not trusted`。

## 端点

```
GET  /.well-known/openid-configuration    发现文档
GET  /authorize                           授权（自己会 302 到登录 UI）
POST /api/oidc/token                      换 token（client_secret_post/basic）
GET  /api/oidc/userinfo                   用户信息
GET  /.well-known/jwks.json               JWKS
```

## 加第三方登录（以后）

在 TinyAuth 里配 `PROVIDERS_*` 环境变量（GitHub / Google / Pocket ID 等），
用户仍然走同一个 `/authorize`，业务侧零改动。

## 用户管理

```bash
# 追加/改密（会重写 users.txt 并重新部署配置）
ADDUSER=name:password OIDC_SECRET=<现有密钥> bash deploy.sh
```

`users.txt` 存 `name:bcrypt`，`.credentials.tmp` 存本次生成的密码与客户端密钥（**不进版本库**）。

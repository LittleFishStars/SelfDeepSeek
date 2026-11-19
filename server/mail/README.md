# pmail（自建邮件服务器）

部署后：Web **https://mail.example.com**（PMail v2.9.9，systemd `pmail`），
SMTP 25/465/587、IMAP 993、POP3 110/995，DB `/var/lib/pmail/pmail.db`。

用途：给 [chat-server](../chat-server) 发注册验证码，以后也能当个人域名邮箱。

## 部署

```bash
bash deploy.sh
```

脚本做的事：上传二进制 → 生成 DKIM 密钥（`dkim.priv`）→ 写 `config.json`
（`isInit: true` 跳过网页向导）→ 装 systemd（`Restart=always`）→
nginx 站点 `mail.example.com` 反代到 `127.0.0.1:8020` → 打印 DKIM 公钥。

## DNS（Cloudflare）

| 类型 | 名称 | 值 |
| --- | --- | --- |
| A | `mail` | `YOUR_SERVER_IP`（不代理） |
| MX | `@` | `mail.example.com` 优先级 10 |
| TXT | `@` | `v=spf1 mx ~all` |
| TXT | `mail._domainkey` | `v=DKIM1; k=rsa; p=…`（见 `dkim.txt`） |
| TXT | `_dmarc` | `v=DMARC1; p=none; rua=mailto:postmaster@example.com` |

## 账号

PMail 的密码算法是**固定盐的双重 MD5**（`server/utils/password/Encode`）：

```
md5( md5(password + "pmail") + "pmail2023" )
```

脚本建号（不走网页）：

```bash
H=$(printf '%s' "$PW" | python3 -c 'import sys,hashlib as h;p=sys.stdin.read();print(h.md5((h.md5((p+"pmail").encode()).hexdigest()+"pmail2023").encode()).hexdigest())')
sqlite3 /var/lib/pmail/pmail.db "INSERT INTO user(account,name,password,disabled,is_admin,auth_version)
  VALUES('noreply','站点通知','$H',0,1,0);"
```

现有：`noreply@example.com`（密码见 `.credentials.tmp`，`is_admin=1`）。

## 两个坑

1. **本域投递会走公网回环**：MX 指向 `mail.example.com` → 公网 IP，服务器自己 dial 自己的公网 IP:25 会超时。
   解决：`/etc/hosts` 加 `127.0.0.1 mail.example.com example.com`。
2. **465 隐式 TLS 对部分客户端会 RST**，用 **587 + STARTTLS** 更稳（chat-server 现在就用 587）。

## 25 端口与「用 465 发信」

阿里云客服已明确 ECS 出方向 25 **不可解封**，只能走 465/587 的**提交**通道——而那属于
邮件服务商（如阿里云邮件推送/QQ/163）的角色，PMail 只能收信 + 本地投递。
所以现在的分工：

- PMail：收外部来信（入方向 25）+ 本地信箱 + Web 后台
- chat-server 发验证码：指向外部服务商的 465（见 [../chat-server/README.md](../chat-server/README.md)）

## 25 端口

- **入方向**：安全组放行 25/465/587/993/995 后外部才能投递进来。
- **出方向**：阿里云对新购 ECS 默认封禁 25 出站，**安全组放行不够，必须提工单解封**
  （控制台 → 工单 → 网络与安全 → 「申请解封25端口」）。实测放行安全组后
  `gmail-smtp-in.l.google.com:25 / mx1.qq.com:25` 仍然 timeout。
  解封前：本域投递正常，外域（如 QQ/163/Gmail）投递会 `451 Downstream delivery temporarily failed`。

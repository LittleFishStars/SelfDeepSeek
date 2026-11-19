# chat-server

给 [chat-demo](https://github.com/deepseek-ai/chat-demo) 用的聊天后端（Go，SQLite）。
客户端只跟它说话，**DeepSeek 的 Key 只存在服务端**，客户端不需要自己的 Key。

部署后：接口 `https://api.example.com/`（对话 `/chat/*`、账号 `/auth/*`），
静态前端与后台页面 `https://chat.example.com/`（systemd `chat-server`，DB `/var/lib/chat-server/chat.db`）。

## 它干什么

| 路由 | 行为 |
| --- | --- |
| `GET /models` | 代理官方 `/models`（注入服务端 Key） |
| `POST /anthropic/v1/messages` | Messages API，SSE 流式转发；**边转发边解析**，把对话落库 |
| `POST /anthropic/v1/files`、`DELETE /anthropic/v1/files/{id}` | 附件接口代理 |
| `* /v1/tools/*` | 语音等工具接口代理 |
| `GET /v1/conversations` | 当前用户的会话列表（标题取首条用户消息） |
| `GET /v1/conversations/{id}` | 会话内的消息（含 thinking） |
| `GET /health` | 计数 |

## 用户与鉴权

- 客户端请求头 `x-api-key` 或 `Authorization: Bearer` 里的值=用户令牌；
  服务端按 `sha256(token)[:16]` 建 user，`users` 表留档令牌。
- 客户端首次启动会自动生成 `dev-xxxx` 设备令牌并写入 localStorage，
  所以每台设备一个用户，会话互不可见。
- 没有令牌时按 `UA+IP` 派生匿名用户（`CHAT_ALLOW_ANON=1`，默认开）。

## 落库

- 客户端每轮都发**全量历史**，服务端以最后一次为准 `replaceMessages`；
  流结束后再用 SSE 里解析出的 `text` / `thinking` 追加一条 assistant 消息。
- 会话 id 来自请求头 `X-Conversation-Id`（客户端已适配），没有就只代理不落库。
- 表：`users` / `conversations` / `messages`，SQLite WAL。

## 部署

```bash
DS_API_KEY=sk-... bash deploy.sh
```

脚本：交叉编译 → gzip 上传到 `/opt/chat-server/` → 写 `/etc/chat-server.env`（600）→
装 `chat-server.service`（`Restart=always`、`MemoryMax=256M`、日志 `/var/log/chat-server.log`）→
在 nginx 的 `deepseek` 站点里**幂等插入** `location /chat/`（SSE 不缓冲）→ 重载 nginx → 健康检查。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `DS_API_KEY` | 必填 | 上游 Key |
| `CHAT_PORT` / `CHAT_BIND` | `8083` / `127.0.0.1` | 监听 |
| `CHAT_DB` | `/var/lib/chat-server/chat.db` | SQLite |
| `CHAT_ALLOW_ANON` | `1` | 允许无令牌访问（按 UA+IP 分用户） |
| `CHAT_UPSTREAM_TIMEOUT` | `300` | 上游超时（秒） |

## 账号体系（邮箱 + 密码）

| 接口 | 说明 |
| --- | --- |
| `POST /v1/auth/register` `{email,password}` | 建号 + 发 6 位验证码（bcrypt 存密码；邮件发不出去会回滚账号） |
| `POST /v1/auth/code` `{email}` | 重发验证码（15 分钟有效） |
| `POST /v1/auth/verify` `{email,code}` | 校验验证码 → 返回登录令牌 |
| `POST /v1/auth/login` `{email,password}` | 密码登录 → 返回登录令牌 |
| `GET /v1/auth/me` | 当前账号（令牌在 `Authorization: Bearer`） |

- 令牌 = 64 位随机 hex，存在 `users.token`；客户端把它当 `x-api-key` 用，
  所以登录后所有对话请求自动归到该账号（`userFor` 先按 token 查库）。
- 令牌换发即顶掉旧会话（单端登录）。

### 邮件（SMTP）

用环境变量接任意 SMTP（PMail、QQ、阿里云邮件推送都行）：

```
SMTP_HOST  SMTP_PORT  SMTP_USER  SMTP_PASS  SMTP_FROM  SMTP_TLS=implicit|starttls
```

`implicit` 走 465 隐式 TLS，`starttls` 走 587。没配 SMTP 时注册会直接返回 502
「验证码邮件发送失败」，不会留下半个账号。

线上现在指向本机 PMail：

```
SMTP_HOST=127.0.0.1 SMTP_PORT=587 SMTP_TLS=starttls
SMTP_USER=noreply@example.com SMTP_PASS=… SMTP_FROM=noreply@example.com
```

注意：连 `127.0.0.1` 时服务器证书必然对不上（证书签给域名），代码里对 IP/localhost
自动跳过校验；连真实域名则正常校验。

> ⚠️ 阿里云 ECS 默认**封禁 25 端口出站**，自建 PMail 无法直接投递到外部邮箱
> （我实测 `gcp-smtp-in.l.google.com:25` 不可达）。三条路：提交工单解封 25；
> 把 SMTP 指向现成邮箱服务商（465）；或让用户在 PMail 同域信箱（本地投递不需要 25）。

## 用 TinyAuth 登录（OIDC）

业务侧只做 OIDC 客户端，账号与第三方登录都交给 [../tinyauth](../tinyauth)（https://auth.example.com）。

```
GET /v1/auth/oidc/start?app_origin=http://127.0.0.1:12345
    → 302 到 https://auth.example.com/authorize?client_id=chat&...
GET /v1/auth/oidc/callback?code=&state=
    → 换 token / 取 userinfo / 建用户 / 发本服务令牌
    → 302 回 <app_origin>/login.html#token=<token>
```

- `state` 在内存里记 10 分钟，带着 `app_origin`，回调时据此跳回 App 自己的源
  （App 的页面是 WebView 里的 `http://127.0.0.1:<随机端口>`，服务端事先不知道）。
- 用户标识用 OIDC 的 `sub`（邮箱为空时用 `preferred_username@oidc.local`），
  建出来的用户 `verified=1`，和邮箱密码用户共用同一套令牌。
- 环境变量：`TINYAUTH_ISSUER`、`OIDC_CLIENT_ID`、`OIDC_CLIENT_SECRET`、`OIDC_REDIRECT`、`OIDC_SCOPE`。

以后在 TinyAuth 里加 GitHub/Google 等登录方式，这边**一行都不用改**。

### 为什么不能用自建 PMail 发外部邮件

SMTP 里两个端口职责不同：

- **25**：服务器 → 收件人 MX，**投递**用。阿里云对 ECS 出方向永久封禁（客服明确不给解封）。
- **465 / 587**：客户端 → 服务商，**提交**（submission）用。它只负责「把信交给服务商」，
  真正的投递还是服务商自己用 25 完成。

所以发外部邮箱只有一条路：**把 SMTP 指向一个支持 465/587 的邮件服务商**。
[pmail](../pmail) 继续留着收信（入方向 25 是通的）和本地信箱，不再负责外发。

### 接阿里云邮件推送（DirectMail）

控制台开通「邮件推送」→ 验证发信域名（按提示加 DNS 记录）→ 创建发信地址并设 SMTP 密码，然后：

```
SMTP_HOST=smtpdm.aliyun.com
SMTP_PORT=465
SMTP_TLS=implicit
SMTP_USER=发信地址（如 noreply@mail.example.com）
SMTP_PASS=控制台设置的 SMTP 密码
SMTP_FROM=发信地址
```

`SMTP_TLS` 支持 `implicit`(465) / `starttls`(587) / `none`(smtpdm.aliyun.com:80 明文)。
改完直接重部署：

```bash
SMTP_HOST=smtpdm.aliyun.com SMTP_PORT=465 SMTP_TLS=implicit \
SMTP_USER=noreply@mail.example.com SMTP_PASS=控制台密码 \
SMTP_FROM=noreply@mail.example.com bash set-smtp.sh
```

`set-smtp.sh` 会从服务器读回 `DS_API_KEY` 和 `OIDC_CLIENT_SECRET`，只替换 SMTP 部分。
实测服务器到 `smtpdm.aliyun.com:465`、`smtp.qq.com:465`、`smtp.163.com:465` 的 TLS 握手都正常。

## 对话记录存储（服务端，树形 + 编辑历史）

存储是**消息树**，不是线性列表 —— 这样才能支持「编辑消息 / 重新生成 / 切分支」，
和 DeepSeek 官方那种「1/2 版本」一致。

| 表 | 关键字段 |
| --- | --- |
| `conversations` | `id, user_id, title, model, tokens_in, tokens_out, active_leaf` |
| `messages` | `id, conv_id, parent_id, role, content, thinking, stop_reason, created_at` |

- `parent_id` 指向上一条消息（根为 NULL），`conversations.active_leaf` 是当前选中的叶子
- 客户端每轮发**全量历史**，服务端取当前分支与之求**最长公共前缀**，
  只把超出的部分挂成新节点（parent = 上一条）
- 于是「把第 1 条改掉再发」会生成一个**同父的新兄弟节点**，旧版本一条不丢；
  「重新生成」同理，会产生同一父节点下的多个 assistant 版本
- `stop_reason` 记 `end_turn` / `max_tokens`；只有思考没有正文的轮次也存

实测（同一会话内先正常聊两轮，再把首条改掉）：

```
15 parent=None user      说一个水果        ← 版本 1
17 parent=15   assistant 苹果。          ← 版本 2（重新生成产生的兄弟）
18 parent=17   user      再说一个
19 parent=18   assistant 香蕉。
20 parent=None user      说一个蔬菜        ← 版本 2（编辑首条产生的新根）
21 parent=20   assistant 西兰花
```

### 查询接口

```
GET    /v1/conversations                     列表（消息数、token 累计、标题）
GET    /v1/conversations/{id}                当前分支（每条带 id/version/versions）
GET    /v1/conversations/{id}?export=1       下载 JSON
GET    /v1/conversations/{id}/tree           整棵树（含全部历史版本，parent_id 串起来）
POST   /v1/conversations/{id}/active         {"message_id": N} 切到某条消息所在的最深分支
DELETE /v1/conversations/{id}                删除会话及其消息
```

每条消息返回里都带 `version`（第几版）和 `versions`（共几版），
客户端据此就能画「‹ 1/2 ›」切换器。实测切换：

```
切到 message_id=15 → 说一个水果 / 苹果。(2/2) / 再说一个 / 香蕉。
切到 message_id=20 → 说一个蔬菜 / 西兰花
```

都用 `Authorization: Bearer <登录令牌>`，只能看到自己的数据。

实测（`conv-store-2`）：

```
user      | 帮我写一句关于秋天的诗
assistant | 秋叶落尽，风把思念吹薄了一层。
user      | 再来一句更短的
assistant | 秋深，月凉。            stop=end_turn
列表：4 条 / 56 in / 143 out tokens
```

### 长回答被截断（2026-11-18）

chat-demo 前端**不发送 `max_tokens`**，于是上游用默认值，长回答会在半句处 `stop_reason=max_tokens` 截断。
代理层已补默认值：请求里没有该字段时注入 **8192**（可用 `CHAT_MAX_TOKENS` 调整，模型上限远大于此）。
实测同样的问题描述：修前几十字节即断，修后 `output_tokens=412 / stop_reason=end_turn`。

### 上下文压缩（撞 1M 上下文前自动折叠）

deepseek-flash 支持约 1M 上下文，但长对话总会撞上限。做法参照 DSH 的 compaction：

- 估算 token（中日韩字符 ≈1 token/字，其余 ≈4 字符/token），达到**上下文窗口的 80%** 时触发
  （`CHAT_CONTEXT_WINDOW` 默认 1048576、`CHAT_COMPACT_RATIO` 默认 0.8 → **838860 tokens**）
- 把**较早的消息**交给模型压成一份结构化中文摘要（主题 / 已达成结论 / 用户偏好与约束 / 未完成事项，≤600 字）
- 用「摘要 + 最近 `CHAT_COMPACT_KEEP`（默认 8）条原文」继续对话，保证角色交替
- **数据库里始终保留完整原文**，压缩只影响发给模型的上下文；每次压缩写一行 `compactions` 表便于追溯
- 摘要失败（网络/上游错误）自动降级为原文，不影响本次对话

实测（把阈值临时调到 2000 触发）：

```
上下文压缩 conv=compact-test-1 早期 12 条 → 摘要 282 字，保留最近 7 条
compactions 表：compact-test-1 | tokens_before=1716 | messages_before=12 | summary 282 字
```

参数：`CHAT_CONTEXT_WINDOW`、`CHAT_COMPACT_RATIO`、`CHAT_COMPACT_THRESHOLD`（直接指定则优先生效）、
`CHAT_COMPACT_KEEP`、`CHAT_COMPACT_MODEL`。启动日志会打印当前阈值。

### 账号管理接口

| 接口 | 说明 |
| --- | --- |
| `GET  /v1/auth/account` | 返回邮箱、验证状态、注册时间、会话数、消息数、是否已设密码 |
| `POST /v1/auth/password` `{old,new}` | 改密码（bcrypt 校验旧密码；**成功后换发令牌**，旧令牌立即失效） |
| `POST /v1/auth/email` `{email,password}` | 改邮箱（需密码确认，邮箱已被占用返回 409） |

客户端在「设置 → 数据管理」顶部有账号管理区，展示账号/密码/使用情况三行，点「修改」弹窗。

## 后台管理

`https://chat.example.com/admin/` —— 一个页面看全站用户、对话与 token 消耗。

页面在 **deepseek 域名**，数据接口在 **`https://api.example.com/admin/api/`**，两者跨域：
服务端 `withCORS` 回显 `Origin` 并放行，所以页面能直接取数。`api.example.com/admin/`
仍可打开同一页面（自用备用入口）。

页面与数据接口都在同一个二进制里（`admin.go` + 内嵌 `admin.html`，无构建步骤、
无第三方前端依赖）；只是经由不同域名暴露。原 `/chat/admin/` 路径已随 `/chat/`
反代一起撤掉（现返回 404）。

会话详情按**分支树**展示：重新生成产生的多条版本带 `‹ n/m ›`，点箭头切换看不同版本；
「显示全部分支」可展开整棵树（非当前分支压暗）。空正文标「（空消息）」，
避免空白块被当成渲染失败。

各视图走**真路径**，可直接访问和刷新（不用 `#`）：

```
/admin/            仪表盘
/admin/users       用户
/admin/conversations  对话
/admin/usage       Token 消耗
```

后端对 `/admin/` 下所有非 `api/` 路径都回同一个页面（单页应用 fallback），前端读
`location.pathname` 决定视图；站内跳转用 `pushState`，不整页刷新，前进后退正常。

> **接口基址**：写死 `API_ORIGIN = 'https://api.example.com/admin/api'`。
> 页面与接口不同源，**相对路径不能用**（会打到 deepseek 域名上，那里没接口）。
> 早先同源部署时踩过两个坑：写死 `/admin/api` 在 `/chat/admin/` 下会打到站点根部
> （被 relay 的 IP 白名单 403，表现为「页面能开、数据全空」）；裸相对路径 `api/x`
> 在 `/admin/users/` 这类带尾斜杠的 URL 下会解析成 `/admin/users/api/x`。
> 视图路由仍按 pathname 拆「前缀 + 视图」两段共用，避免跳转越叠越深。

| 接口 | 说明 |
| --- | --- |
| `GET  /admin/api/overview?days=30` | 仪表盘汇总、每日趋势、排行榜、模型分布、最近会话 |
| `GET  /admin/api/users?page&size&q&sort` | 用户列表（`sort` 取 `seen`/`created`/`tokens`/`convs`） |
| `GET  /admin/api/users/{id}` | 用户详情 + 他的会话 |
| `POST /admin/api/users/{id}/disable` `{disabled}` | 禁用 / 恢复 |
| `DELETE /admin/api/users/{id}` | 删除用户及其会话、消息、令牌、流水、缓存 |
| `GET  /admin/api/conversations?page&size&q&user` | 全站会话列表 |
| `GET  /admin/api/conversations/{id}` | 会话正文：`messages`（当前分支）+ `tree`（全树，带 `version`/`versions` 供前端画分支），单条截断 4000 字 |
| `DELETE /admin/api/conversations/{id}` | 删除会话（按原 owner 记 tombstone，客户端会同步到） |
| `GET  /admin/api/usage?days=30` | 按天 / 用户 / 模型的 token 聚合 + 最近 50 次调用 |

鉴权用独立的 `CHAT_ADMIN_TOKEN`（`Authorization: Bearer`），和业务令牌完全隔离。
**没配置这个变量时整个后台关闭**（API 返 503），不会退化成人人可进；页面本身能打开，但拿不到数据。

### token 流水

`conversations.tokens_in/out` 只是每会话的累计值，画不出趋势，所以另记一张流水表：

```sql
usage_log(id, user_id, conv_id, model, tokens_in, tokens_out, stop_reason, source, created_at)
```

每轮助手回复落库时（`appendAssistant`）写一条，`source='live'`。老库首次启动会把已有累计值
按「一个会话一条」补进来（`source='backfill'`，时间取会话更新时间）；幂等，靠
`NOT EXISTS (SELECT 1 FROM usage_log WHERE conv_id = c.id)` 保证不重复。

> 注意：回填粒度是会话级的，所以历史数据在趋势图里会集中在「会话最后更新时间」那一天，
> 只有部署之后的真实调用才是逐轮的。

### 部署

`deploy.sh` 会自动处理令牌：优先用环境变量 `CHAT_ADMIN_TOKEN`，其次沿用服务器
`/etc/chat-server.env` 里已有的值，都没有才新生成一个，并在末尾打印出来。
**不能每次部署都重新生成**，否则已登录的后台会被踢下去。

## 网页端（与 App 同一套前端）

```
https://chat.example.com/               ← 登录页（登录后进聊天）
https://chat.example.com/index.html
```

本站**只放静态前端**，接口一律在 `https://api.example.com/`（编译期写死）。
nginx 里 `/api/`、`/chat/` 两条反代已撤掉，deepseek 域名下的未知路径直接 404，
不会再把不存在的接口伪装成 200。

### 图标

浏览器标签页、iOS 添加到主屏、Android 桌面快捷方式都用这一套：

| 文件 | 用途 |
| --- | --- |
| `favicon-32x32.png` / `favicon-16x16.png` | 标签页（页面里声明时 PNG 放在最前，比 ico 清晰） |
| `favicon.ico` | 老浏览器 / 直接访问 `/favicon.ico` 的客户端 |
| `apple-touch-icon.png` (180) | iOS 添加到主屏 |
| `icon-192.png` / `icon-512.png` + `site.webmanifest` | Android / PWA |
| `icon.png` | 上游前端构建产物里自带的，登录后主界面用 |

源码在 `ops/icons/`，由 `ops/deploy-web.sh` 跟着静态站一起下发 —— **不能**放进
`app/android/app/assets`，那是前端构建产物，`sync-web.sh` 每次会清空重拷。

图标从 `app/android/app/assets/icon.png` 裁掉四周空白后重采样生成：原图鲸鱼只占
画布 52%×40%，直接缩到 16px 会糊成一团，所以先按内容包围盒裁剪、再正方形化留
6% 边距。重新生成时照这个流程来，别直接 resize 原图。

nginx 里有两条相关 location：一条 regex 管图片和 ico，`site.webmanifest` 单独一条
exact 并强制 `application/manifest+json`（塞进 regex 用 `types` 改会覆盖继承的
mime.types，把 png/ico 也标成 manifest 类型）。deepseek 与 api 两个域名都配了，
因为后台页面在两个域名下都能打开。

前端把接口拆成两组前缀，都从同一个域名派生：

| 前缀 | 用途 | 构建变量 |
| --- | --- | --- |
| `https://api.example.com/chat` | 对话与会话（消息、同步、缓存、会话、文件、导入、语音） | `API_ORIGIN` |
| `https://api.example.com/auth` | 账号（登录、注册、改密码、改邮箱、账号信息、OIDC） | `AUTH_ORIGIN` |

两者都由 `__API_ORIGIN__`（构建参数 `CHAT_API_ORIGIN`）派生，换域名只改构建变量，
但**必须重新打包** APK 与网页端。登录页只用账号接口，所以它的 `var API` 直接写 `/auth`。

发布：

```bash
bash ops/deploy-web.sh <chat-demo/dist 目录>     # 上传到 /opt/chatweb/chat/app，并写 nginx
```

要点：

- 页面与 API **同源**（都在 `/chat/` 下），因此没有跨域、没有预检，附件上传也不受 4MB 限制影响（已放宽到 32m）
- nginx 里 **不能用 `index index.html` 做根路径内部跳转**：它和 `/chat/` 的 proxy 块会互相递归，直接 500
  （`rewrite or internal redirection cycle`）。正确写法是精确匹配 + `try_files`：

```nginx
location = /chat/app/ {
    root /opt/chatweb;
    try_files /chat/app/login.html =404;
}
location /chat/app/ {
    root /opt/chatweb;
}
```

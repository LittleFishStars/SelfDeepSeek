# 配置参考

全部配置走环境变量，只有 `DS_API_KEY` 必填。示例见仓库根的 [.env.example](../.env.example)。

```bash
set -a && . ./.env && set +a && ./chat-server
```

## 上游 API

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `DS_API_KEY` | **必填** | DeepSeek API Key，只存在服务端 |
| `DS_API_BASE` | `https://api.deepseek.com` | 上游基址，换兼容 Anthropic Messages 协议的服务时改这里 |
| `DS_MODEL` | `deepseek-flash` | 默认模型；会话列表里存的是创建时的模型 |
| `CHAT_MAX_TOKENS` | `1048576` | 前端不发 `max_tokens` 时补的默认值（1m）。上游 `deepseek-flash` 输出上限 393216；给小了会让深度思考 + 多轮工具在正文写出来前就被 `max_tokens` 截断（界面显示「已停止」+「继续生成」） |
| `CHAT_UPSTREAM_TIMEOUT` | `300` | 单次上游请求超时（秒），流式任务整轮上限 |

## 监听与存储

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CHAT_BIND` | `127.0.0.1` | 监听地址。前面有 nginx 就保持回环 |
| `CHAT_PORT` | `8083` | 监听端口 |
| `CHAT_DB` | `/var/lib/chat-server/chat.db` | SQLite 路径（WAL 模式，会自动建目录） |
| `CHAT_SHARE_DIR` | `/opt/chatweb/share` | 分享页 HTML 与共用静态资源的落盘目录 |
| `CHAT_SHARE_BASE` | `https://chat.example.com/share/` | 分享链接前缀（返回给客户端的 URL）；域名不同必须覆盖 |
| `CHAT_WEB_BASE` | `https://chat.example.com` | 分享页里的站点地址（品牌链接） |

附件目录固定取 `CHAT_DB` 同级的 `files/`，没有独立变量。

## 鉴权

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CHAT_ALLOW_ANON` | `1` | `1` = 无令牌也放行（按 `UA+IP` 派生用户）。个人自用可以开，公网多用户应关掉 |
| `CHAT_TOKEN` | 空 | 全局固定令牌：带这个值的请求都算同一个用户。仅适合单人自用 |
| `CHAT_ADMIN_TOKEN` | 空 | 后台令牌。**留空 = 后台整体关闭**（`/admin/` 返回 503） |

客户端令牌模型：

- 每个设备首次启动生成自己的令牌（`dev-xxxx` 之类），存在浏览器 localStorage；
- 登录态令牌按**设备类型**（`web` / `android` / `harmony`）分槽位，同类型重登会旋转旧令牌；
- 服务端按 `sha256(token)[:16]` 建用户，令牌原文留在 `users` 表里做对照。

## 注册与验证码邮件

不配 SMTP 时，注册接口会返回 502（「验证码邮件发送失败」），密码登录仍可用。
已有账号想重置密码才依赖邮件。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `SMTP_HOST` | `127.0.0.1` | SMTP 服务器 |
| `SMTP_PORT` | `465` | 端口 |
| `SMTP_TLS` | `implicit` | `implicit`（465 隐式 TLS）/ `starttls`（587）/ `none`（25，仅内网自建） |
| `SMTP_USER` | 空 | 登录名（同时用于「未配 SMTP」的启动告警判断） |
| `SMTP_PASS` | 空 | 密码 |
| `SMTP_FROM` | `noreply@example.com` | 发件地址 |
| `SMTP_FROM_NAME` | `SelfDeepSeek \| SDS` | 发件人显示名 |

连 `127.0.0.1` / `localhost` 时自动跳过证书校验（自建收件服务器的证书签给域名，
IP 必然对不上）；连真实域名则正常校验证书。

> 云主机常见坑：阿里云 ECS 默认封禁 25 端口出站，自建收件服务器无法直接投递到外部邮箱。
> 要么提交工单解封，要么把 `SMTP_*` 指向现成邮箱服务商的 465 端口。

## OIDC 登录（可选）

与「邮箱 + 密码」并存；配置后登录页多一个「用 XX 登录」入口。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `TINYAUTH_ISSUER` | `https://auth.example.com` | issuer，必须与提供方声明完全一致（含协议、不含子路径） |
| `OIDC_CLIENT_ID` | `chat` | 客户端 ID |
| `OIDC_CLIENT_SECRET` | 空 | 机密客户端才需要 |
| `OIDC_SCOPE` | `openid profile email` | 申请的 scope |
| `OIDC_REDIRECT` | `https://api.example.com/auth/v1/auth/oidc/callback` | 回调地址，必须与提供方白名单一字不差 |

回调地址的实际位置取决于 nginx 怎么剥前缀：这里写的是**浏览器看到的**完整 URL。

## 上下文压缩

长对话撞上下文上限前，把早期消息折叠成一段中文摘要，用「摘要 + 最近 N 条原文」继续。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CHAT_CONTEXT_WINDOW` | `1048576` | 模型上下文窗口（token），仅用于日志参考 |
| `CHAT_COMPACT_RATIO` | `0.8` | 触发比例：估算 token > `CHAT_MAX_TOKENS` × 比例时压缩 |
| `CHAT_COMPACT_THRESHOLD` | 空 | 直接指定阈值（token），填了优先于上两项 |
| `CHAT_COMPACT_KEEP` | `8` | 压缩后保留最近多少条原文（保证角色交替） |
| `CHAT_COMPACT_MODEL` | 同 `DS_MODEL` | 跑摘要用的模型 |

压缩在**发出请求前**判定：达到阈值时本轮照常完整输出，压缩只对**下一次**请求生效，
不会打断正在进行的生成。

数据库里始终保留完整原文，压缩只影响发给上游的上下文；每次压缩写一行 `compactions` 表便于追溯。

## 跨会话记忆

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CHAT_PROFILE_AUTO_SUMMARIZE` | `1` | `0` = 关闭自动总结（画像与上一段摘要都不生成） |
| `CHAT_PROFILE_MODEL` | 同 `DS_MODEL` | 总结用的模型 |
| `CHAT_PROFILE_MAX_CHARS` | `3000` | 画像字数上限；超了先让模型压缩，压不下来才截断 |
| `CHAT_PROFILE_CONTEXT_TOKENS` | `700000` | 一次调用能塞进多少 token 的会话全文；超出则分段提炼再汇总 |
| `CHAT_PROFILE_REBUILD_LIMIT` | `50` | 手动重建画像时取最近多少段会话 |
| `CHAT_SUMMARY_MAX_INPUT` | `24000` | 单次总结输入的字符截断上限 |

开关本身存在服务端（`user_settings.share_context`），由前端设置页调用
`POST /chat/v1/profile/switch` 切换，因为注入与工具下发都在服务端做。

## 沙箱脚本工具

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CHAT_RHAI_ENABLE` | `1` | `0` = 不下发 `run_rhai` 工具 |
| `CHAT_RHAI_BIN` | `/opt/chat-server/rhai-runner` | 执行器路径；文件不存在时工具同样不下发 |

隔离靠四层：Rhai 引擎不注册任何 IO；`max_operations` 打断死循环；Go 按超时 kill 进程；
脚本的 `print` 重定向到缓冲区。详见 [server/rhai-runner](../server/rhai-runner)。

## 超限图片自动切分

上游对图片有**单边最长 8192 像素**的硬限制（一次带 ≥15 张图时降为 4096），
长截图会直接 400。后端在 Files 代理层自动处理，客户端无需改动：

- 仅长或宽超限 → 不缩放，沿超限方向切成恰好满足条件的最少张数，片间留重叠；
- 两边都超限 → 先等比压缩到短边恰好等于上限，再切分。
- 客户端拿到的是聚合 id `slice-xxxx`，真实上游 id 列表存在 `image_slices` 表，
  发消息时展开成多个 image 块。切分失败会回退原样上传，让上游给出权威报错。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CHAT_TRACE_DIR` | 空 | 非空则把上游原始事件落盘，排查「流里没内容」这类问题用 |

## 调试

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CHAT_TRACE_DIR` | 空 | 非空则把上游原始事件落盘，排查「流里没内容」这类问题用 |
| `CHAT_TRACE_ALL` | 空 | `1` = 连非 messages 请求也记录 |

生产环境别常开：SSE 事件量大，很容易把磁盘写满。
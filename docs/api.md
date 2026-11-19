# 接口契约

**客户端认死了路径形状**，改路由要同时改前端（前端是编译产物，见 [clients.md](clients.md)）。

## 前缀与转发

| 对外路径 | nginx 处理 | 服务端看到的路径 |
| --- | --- | --- |
| `/chat/*` | 剥掉 `/chat` | `/v1/*`、`/anthropic/*` |
| `/auth/*` | 剥掉 `/auth` | `/v1/auth/*` |
| `/admin/*` | **保留** | `/admin/*` |
| `/health` | 转发 | `/health` |

下表全部是**剥掉前缀后**的服务端路由。

鉴权：请求头 `x-api-key: <令牌>` 或 `Authorization: Bearer <令牌>`。
令牌 = 设备令牌（客户端首次启动自生成）或登录令牌（邮箱登录 / OIDC 之后拿到）。
`CHAT_ALLOW_ANON=1` 时无令牌请求按 `UA+IP` 派生用户。

## 对话与生成

| 接口 | 语义 |
| --- | --- |
| `POST /anthropic/v1/messages` | **主入口**。请求体按 Anthropic Messages 协议；服务端落库 user 消息、建任务、立即返回 SSE 头 + `X-Job-Id`。请求头可带 `X-Conversation-Id`（会话归属）与 `X-Resume: 1`（断点续播） |
| `GET /models` | 代理上游模型列表（注入服务端 Key） |
| `POST /anthropic/v1/files`、`DELETE /anthropic/v1/files/{id}` | 附件接口代理（图片/文档上传） |

一次生成的生命周期：

1. 服务端先补 `max_tokens`（默认 8192），清洗历史（丢空 assistant、合并连续 user）；
2. 落库 user 消息 → 建任务 → 返回 SSE；
3. 任务在**独立 context** 里跑上游，事件先缓冲，见到正文或首轮思考内容才外发；
4. 客户端断开不影响生成；没人完整收下的回复写进 `reply_cache`；
5. 客户端重连带 `X-Resume: 1`，靠 `GET /v1/cache` 补上缺失部分。

工具轮次（联网搜索 / `history_search` / `run_rhai`）由服务端**整轮合成**再外发：
不转发上游原始事件，合成块索引从 1000 起，并屏蔽后续请求的 `message_start`。
详见 [architecture.md](architecture.md)。

## 会话与同步

| 接口 | 语义 |
| --- | --- |
| `GET /v1/sync` | 增量同步。`mode=incr`（默认，取 `last_sync` 之后）/ `full` / `range&from=&to=`；返回 `conversations` + `changes[{conv_id,op}]` + `since/now/mode` |
| `GET /v1/cache` | SSE。先一次性下发 `reply_cache` 全部积压（写成功即删），随后持续推送新产生的 |
| `GET /v1/conversations/{id}` | 会话数据：`messages`（当前分支）+ `tree`（全树）+ `title`；`?export=1` 下载 JSON |
| `POST /v1/conversations/{id}/active` | 切分支，体 `{"message_id": N}` |
| `DELETE /v1/conversations/{id}` | 删除会话及其消息（同时写变更日志） |
| `DELETE /v1/sync` | 清空该用户的同步状态 |
| `GET /v1/search` | 会话搜索：`?q=&limit=`，返回命中会话 + 消息 id + 片段 + 命中词（同一会话最多 3 条） |

存储是**消息树**而不是线性列表：`messages.parent_id` 指向父消息，
`conversations.active_leaf` 是当前选中的叶子。客户端每轮发全量历史，
服务端取当前分支与它求最长公共前缀，只把多出来的部分挂成新节点 ——
于是「编辑首条重发」生成同父的新兄弟，「重新生成」生成同父的多个 assistant 版本，
旧版本一条不丢。每条消息返回里带 `version`（第几版）与 `versions`（共几版），
前端据此画「‹ 1/2 ›」。

> 关键约束：`/v1/sync` 返回的每条会话的 `messages` 计数必须是**接口视图的条数**
> （与 `/v1/conversations/{id}` 一致），不是库里行数。客户端拿它跟本地条数比来决定是否重拉，
> 一旦不一致，客户端永远看不出差异，会一直用旧缓存渲染。

## 附件

| 接口 | 语义 |
| --- | --- |
| `POST /v1/files` | 上传附件，返回 `{id,...}`；元数据随消息存进 `messages.files` |
| `GET /v1/files/{id}` | 下载附件 |

附件落盘在 `CHAT_DB` 同级的 `files/`。

## 导入（手动搬历史）

| 接口 | 语义 |
| --- | --- |
| `POST /v1/import/chunk` | 4MB 分片上传 ZIP |
| `POST /v1/import/finish` | 结束上传，触发异步解析 |
| `GET /v1/import/status` | 进度：`parsing / summarizing / summary_done / summary_total` |
| `POST /v1/import/zip` | 一次性上传（小包用） |

导入完成后会把这批会话的全文拼起来生成用户画像（按 token 总量决定一次调用还是分段汇总）。

## 跨会话共享

| 接口 | 语义 |
| --- | --- |
| `GET /v1/profile` | 开关状态 + 用户画像 + 汇总任务进度 |
| `POST /v1/profile/switch` | 切换开关，体 `{"enabled": bool}` |
| `POST /v1/profile/rebuild` | 用最近 N 段会话重算画像，`?limit=N` |

开启后：新会话第一轮把「用户画像 + 上一段对话摘要」注入 system
（顺序是原 system → 画像 → 上次摘要；只在第一轮注入，避免每轮改 system 打掉前缀缓存），
同时随 tools 下发 `history_search`（关键词 LIKE 检索历史，命中片段作为 tool_result 回灌）。

## 账号（`/auth/*`，剥前缀后是 `/v1/auth/*`）

| 接口 | 语义 |
| --- | --- |
| `POST /v1/auth/register` | `{email,password}` 建号并发 6 位验证码（bcrypt 存密码；邮件发不出去会回滚账号） |
| `POST /v1/auth/code` | `{email}` 重发验证码（15 分钟有效） |
| `POST /v1/auth/verify` | `{email,code}` 校验并返回登录令牌 |
| `POST /v1/auth/login` | `{email,password}` 密码登录 → 登录令牌 |
| `GET /v1/auth/me` | 当前账号信息 |
| `POST /v1/auth/password`、`/v1/auth/email` | 改密码 / 改邮箱 |
| `GET /v1/auth/oidc/start`、`/v1/auth/oidc/callback` | OIDC 登录入口与回调 |

登录令牌按**设备类型**（`web` / `android` / `harmony`）分槽位发放，同类型重登会旋转旧令牌。

## 分享

| 接口 | 语义 |
| --- | --- |
| `POST /v1/conversations/{id}/share` | 建分享（已存在则复用，不重复生成），返回 `{id, url, reused}` |
| `GET /v1/conversations/{id}/share` | 查当前分享状态：`{shared:false}` 或 `{shared:true, id, url}` |
| `DELETE /v1/conversations/{id}/share` | 取消分享（删文件 + 清字段） |

分享链接前缀由 `CHAT_SHARE_BASE` 决定（默认 `https://chat.example.com/share/`）。

分享页是服务端直接渲染的静态 HTML，落在 `CHAT_SHARE_DIR`，
链接不带 `.html`，CSS/JS 全站共用 `/share/assets/share.{css,js}`。
公开可读，所以别把含敏感内容的会话分享出去。

## 管理后台（`/admin/*`，不剥前缀）

| 接口 | 语义 |
| --- | --- |
| `GET /admin/` | 内嵌单页（`go:embed`） |
| `GET /admin/api/users` | 用户列表 + token 消耗 |
| `GET /admin/api/users/{id}` | 用户详情：会话、`share_context`、`profile`、`profile_updated` |
| `DELETE /admin/api/users/{id}` | 删除用户（连带清画像、摘要、设置） |

鉴权：`Authorization: Bearer $CHAT_ADMIN_TOKEN`，与业务令牌完全隔离；
`CHAT_ADMIN_TOKEN` 为空时后台整体返回 503。

## 健康检查

```
GET /health → {"ok":true,"users":N,"conversations":M,...}
```

用于探活与排障。**注意**：如果服务因为单连接池被某个请求卡住，这个接口也会一起超时 ——
这本身就是最有用的信号。
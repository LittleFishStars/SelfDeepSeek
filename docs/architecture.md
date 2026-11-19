# 对话流程（时序图）

> 部署结构：客户端 → `api.example.com` → nginx → chat-server → DeepSeek。
> 生成由**服务端任务**负责，客户端只是订阅者；断线可续传，未送达的回答进离线缓存。

```mermaid
sequenceDiagram
    autonumber
    participant C as 客户端<br/>(App WebView / 浏览器)
    participant N as nginx<br/>api.example.com
    participant S as chat-server
    participant J as 生成任务<br/>(goroutine)
    participant U as api.deepseek.com
    participant D as SQLite<br/>(conversations/messages)

    Note over C,S: ① 启动与鉴权
    C->>S: GET /v1/auth/me（Bearer 设备令牌）
    S-->>C: 200 {email, verified} / 401 → 回登录页
    C->>S: GET /v1/conversations（增量：last_sync 之后）
    S-->>C: 200 {conversations, since, now, full}
    C->>S: GET /v1/pending（离线补发）
    S-->>C: 200 {replies: [...]}
    C->>S: POST /v1/pending/ack?conv_id=…（落地后回执删除）

    Note over C,S: ② 发送一轮对话
    C->>S: POST /files（附件原件，id 复用本地 id）
    C->>S: POST /v1/messages<br/>X-Conversation-Id + attachment_meta
    S->>S: 清洗历史（去空 assistant、合并连续 user、剥离工具块）<br/>补 max_tokens=8192<br/>落库 user 消息 + 附件元数据
    S->>S: 建任务 key = 会话ID + 请求体哈希
    S-->>C: 200 立即返回（SSE 头 + X-Job-Id）

    Note over J,U: ③ 服务端任务生成（与客户端连接解耦）
    S->>J: go runJob(body)
    J->>U: POST /anthropic/v1/messages（HTTP/1.1）
    U-->>J: SSE: thinking / server_tool_use / web_search_result / text
    J->>J: 缓冲事件，见到正文才开始外发
    alt 上游断流且 0 正文
        J->>U: 去掉 web_search 重试（最多 3 次）
    end
    J->>D: appendAssistant(text, thinking, blocks, stop)
    J-->>C: 事件按序推送（2 秒心跳保活）

    Note over C,J: ④ 断线与续传
    C-xJ: 连接中断（切后台/断网/杀进程）
    Note right of J: 任务继续跑完并落库
    J->>D: reply_cache（若无人完整收到）
    C->>S: 重新 POST /v1/messages + X-Resume
    S-->>C: 从任务缓冲重放全部事件
    C->>C: 重置块 → 重新应用 → 得到完整回答
    C->>S: 下次启动 GET /v1/pending → 落地 → ack

    Note over C,S: ⑤ 打开历史会话
    C->>S: GET /v1/conversations/{id}/tree
    S-->>C: 树（含 blocks、files 元数据）
    C->>S: GET /files/{id}（按需下载附件原件）
    C->>C: 写入 IndexedDB，重建消息与搜索/浏览过程
```

## 关键约定

| 环节 | 设计 |
| --- | --- |
| 入口 | API 走 `api.example.com`（DNS-only + Let's Encrypt），静态页仍可走 `chat.example.com` |
| 生成 | 服务端任务持有完整事件流；客户端断开不影响生成与落库 |
| 重试 | 上游 0 正文断流时，任务内部去搜索重试 3 次，失败尝试不外发 |
| 续传 | 同一任务可被 `X-Resume` 重放；未送达则进 `reply_cache`，由 `/v1/pending` 补发 |
| 同步 | `client_tokens.last_sync` 记录每设备同步点，只下发其后更新的会话 |
| 正文 | 只同步列表，正文与附件在打开会话时按需拉取 |
| 消息 | `text` / `thinking` / `blocks`（工具过程）/ `files`（附件元数据）分列存储 |

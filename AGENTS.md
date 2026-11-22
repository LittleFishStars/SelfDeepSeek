# AGENTS.md — 在本仓库干活的约定

给 AI 代理和新加入的人：这里是**约定、命令、红线**。产品背景见 [README.md](README.md)，
一轮对话的完整时序见 [docs/architecture.md](docs/architecture.md)。

## 1. 结构

```
server/chat-server/   Go 后端（唯一后端）
server/rhai-runner/   Rhai 沙箱执行器（Rust，被后端按需拉起）
server/auth/          TinyAuth 部署编排（OIDC 提供方，第三方二进制不入库）
server/mail/          PMail 部署编排（收信，第三方二进制不入库）
client/android/       WebView 安卓壳（Java）+ 打包脚本
client/harmony/       HarmonyOS 壳（ArkTS）
deploy/               nginx / systemd 模板
docs/                 文档（含 frontend-patches/：前端源码不在本仓库，
                      必须在前端做的改动记在那里，重建时照此应用）
tools/                改接口地址、生成更新清单
```

**前端源码不在本仓库**：`client/android/app/assets` 是上游 chat-demo 的构建产物
（接口基址在构建期注入）。改界面要在 chat-demo 源码里改，再 `sync-web.sh` 同步。

## 2. 常用命令

```bash
# 后端
cd server/chat-server && gofmt -w . && go build -trimpath -ldflags="-s -w" -o .build/chat-server .
cd server/chat-server && go test ./...

# 沙箱执行器
cd server/rhai-runner && cargo build --release

# 安卓包（版本号唯一来源 client/android/version.txt）
cd client/android && bash bump.sh && bash build.sh --api-origin https://api.你的域名

# 前端接口地址批量改写（安卓 assets + 鸿蒙 rawfile）
python3 tools/set-api-origin.py https://api.你的域名 --check
```

## 3. 版本与提交

- 版本唯一来源 `client/android/version.txt`（`VERSION_NAME` / `VERSION_CODE`），
  **VersionCode 必须单调递增**，否则手机端收不到更新。
- 功能改动进 minor，修补走 patch。
- 每次改动都要 commit，提交信息写清「做了什么 + 为什么」。
- 提交前确认没把密钥、`*.jks`、`*.p12`、`chat.db`、二进制加进来（见 `.gitignore`）。

## 4. 接口契约（改动前必读）

对外路径分三组前缀（nginx 剥前缀后转发到 8083）：

| 前缀 | 内容 | 是否剥前缀 |
| --- | --- | --- |
| `/chat/*` | 对话与会话：消息、同步、缓存、文件、导入、搜索、画像、分享 | 剥 |
| `/auth/*` | 账号：登录、注册、验证码、改密码、改邮箱、账号信息、OIDC | 剥 |
| `/admin/*` | 后台（含内嵌的 admin.html） | **不剥** |
| `/health` | 探活 | 不剥 |

剥掉前缀后的服务端路由见 [docs/api.md](docs/api.md)。

原则：

- **服务端自己维护对话**：客户端只拉不推；库里记录只来自真实经过服务端的请求 + 手动导入。
- **接口收敛优先**：不保留兼容层；删旧路径要同步改客户端。
- **同步只拉列表**，正文/附件在打开会话时按需拉。

## 5. 生成链路（最易出问题的地方）

1. 客户端 `POST /chat/anthropic/v1/messages` → 服务端清洗历史 → 落库 user 消息 →
   建任务 → 立即返回 SSE 头 + `X-Job-Id`。
2. 任务（`job.go`）用**独立 context** 跑上游，事件先缓冲，
   **见到正文或首轮的思考内容才外发**；客户端断开不影响生成。
3. 上游 0 正文断流 → 内部去掉 `web_search` 重试（最多 3 次），失败尝试不外发。
4. 任务结束：有人完整收下就丢弃；没人收到就写 `reply_cache`，由 `/chat/v1/cache` 补发。
5. 客户端中断会自动带 `X-Resume: 1` 重连重放。

**工具循环不设业务预算**：模型改脚本重试是正常行为，想调几轮调几轮，只留宽松的
硬上限 `toolRoundHardLimit`(30) 防跑飞；触到上限是「注入提示 + 去掉工具让模型直接
回答」，绝不发 `error` 把输出中断。

**上游 4xx 不当断流**：408/429/5xx 才重试，其余 4xx 是请求本身不合法（重试必然
还是同样的错），直接把上游 `error.message` 透传给客户端——用户要看到真实原因，
而不是笼统的「上游生成中断」。

**压缩阈值 = `max_tokens` 的 80%**：达到阈值时**本轮照常完整输出**，压缩在下一轮
请求发出前才生效，绝不打断正在进行的生成。

改这块之前先看 `job.go` / `replycache.go` 的注释，别重新引入「空流也发送」。

## 6. 红线（都踩过）

- **`Accept-Encoding` 不能透传**：浏览器带 `br`，上游会压缩，SSE 解析全空。
- **空 `assistant` 消息会让上游立刻断流**，连续两条 `user` 也不行；
  `sanitizeHistory` 负责丢空 assistant 并合并连续 user。
- **`storeMu` 不可重入**：`recordChange` 用独立 `changeMu`，
  「持锁再调会加锁的函数」会死锁（删除接口曾卡 60s）。
- **上游用 HTTP/1.1**（`ForceAttemptHTTP2: false`）：HTTP/2 的 stream reset
  表现为「思考完直接 EOF」。
- **APK 里 WebView 端口必须固定 18888**：origin 一变 localStorage 就换一套，登录态丢失。
- **`created_at` / `updated_at` 是 REAL**，Go 侧一律 `float64` 扫。
- **`db` 是单连接池**（`SetMaxOpenConns(1)`）：`rows.Next()` 循环里**不能**再发查询，
  否则整个服务挂在这一个请求上。要算就先读完、`Close()`，再二次循环。
- **`/chat/v1/sync` 的 `messages` 计数必须是视图条数**（与 conversation 详情一致）；
  否则客户端永远看不出差异，会一直用旧缓存渲染。
- **admin.html 是 `//go:embed` 的**：改完必须重新 `go build`，只替换旧二进制没用。
- 生产构建会移除 `console.*`，排查前端用全局变量（如 `window.__dbg`）。
- **超限图片要在 Files 代理层切分**：上游单边最长 8192 像素，长截图直接 400。
  `imagefit.go` 按「仅一边超限就切、两边都超限先缩到短边等于上限再切」处理，
  片间留重叠；客户端拿聚合 id `slice-xxxx`，发消息时展开成多个 image 块。
  两个坑：分片上传要走 `/anthropic/v1/files`（图片不传 purpose），且**必须带
  `anthropic-beta: files-api-2025-04-14`**——缺这个头连接会被直接重置，
  看着像网络抖动但重试多少次都没用。
- **分享页的公式要服务端先「保出来」**：Markdown 会把 `\Delta` 当转义吃掉、
  `$$...$$` 当普通文本。做法见 `share.go` 的 `protectMath`：先按代码块切开，
  把公式换成占位符 → 渲染 → 再换回 `<span class="math">`，交给 KaTeX 排版。
  KaTeX **自托管**在 `share-assets/katex/`（别走 CDN，国内常失败）。
  分享页静态资源 URL 要带内容指纹 `?v=`，否则改了样式页面不动（边缘缓存 4 小时）。
- **分享页不要放跳回自家站点的链接**：品牌名用纯文本，页脚只留免责声明。

## 7. 验证要求

- 改服务端：`gofmt -w . && go build ...`，再起服务跑一次 `/health` 与一条带联网搜索的对话。
- 改客户端：打完包后用 `python3 -c "import zipfile; ..."` 确认关键字符串进包（例如接口路径）。
- 改鸿蒙壳：`bash client/harmony/build.sh debug`（ArkTS 编译必须过），
  签名链路用 `bash build.sh dev && bash build.sh verify` 自检。
- 破坏性操作（清库、改 nginx）先备份：`sqlite3 chat.db ".backup backup-<ts>.db"`。

# 前端补丁：断线持续重连 + 恢复同步（含 1.7.6 追修）

前端源码不在本仓库（chat-demo 上游 + `CHAT_API_ORIGIN` 编译期注入），这里记录
必须在源码里做的改动，重新构建前端时照此应用。

## 背景与两次迭代

**1.7.5（第一次修复，不够）**：`streamReply` 原本只用 `X-Resume` 重连一次，
改为无限重连；`initChat` 加网络守护。但用户实测重进仍拿不到后台跑完的正文。

**1.7.6（追修，找到真根因）**：三处缺陷叠加：

1. **服务端 `/v1/sync` 无条件推进 `last_sync`**（已改服务端，见 `main.go`）：
   网络守护每 30s 用 `/v1/sync` 探测，把游标一路推过会话的 `updated_at`，
   此后增量同步再也拉不到这个会话 —— 重进看到的还是断线前的旧内容。
2. **网络守护用 `/v1/sync` 探活**（有害）：改成 `/health`，绝不碰同步状态。
3. **`applyCachedStream` 只在 full 同步时调用**：日常重进走增量同步，
   离线缓存（后台跑完的正文）永远不被消费。改为增量同步也消费，且放在
   占位会话创建之后（否则 `applyCachedReply` 找不到本地会话直接丢弃）。
   另外 `initChat` 启动改做一次**全量**同步兜底。

## 服务端配套改动（本仓库）

`server/chat-server/main.go` 的 `handleConversations`：只有 `len(collected)>0 ||
len(changes)>0` 时才推进 `last_sync`，空结果（探活/无变化）不再推进。

## 前端补丁（完整 diff）

```diff
@@ -45,6 +45,9 @@
 const FILE_RETRY_LIMIT = 2
 /** 退避基数，第 n 次重试等待 2^n 倍；连续失败达到上限就放弃，把原始报错展示出来 */
 const FILE_RETRY_BASE_DELAY = 1000
+/** 断线重连：指数退避 1s→2s→4s…封顶 30s，无限重试直到连上（用户主动停止才结束） */
+const RECONNECT_BASE_DELAY = 1000
+const RECONNECT_MAX_DELAY = 30_000
 
 /** 阅读链接始终开启，智能搜索开关控制是否另行声明搜索工具（含边想边搜）。 */
 const serverTools = (search: boolean): CreateMessageRequest['tools'] => {
@@ -456,6 +459,11 @@
       set(() => ({ sessions: sortSessions(summaries), sessionCache: {} }))
     }
 
+    // 缓存补发：增量同步也要消费。服务端把「生成完但没人收到」的回答写进 reply_cache，
+    // 只有拉它才能拿到正文。必须放在占位会话创建之后，否则 applyCachedReply 找不到
+    // 本地会话会直接丢弃。缓存是「发成功即删」，重复调用幂等。
+    void applyCachedStream(headers)
+
     // 已存在的会话同样要跟进：服务端可能在这个会话里新增了消息（另一台设备发的，
     // 或者重新生成过）。原来只处理「本地没有」的会话，已存在的一律跳过 ——
     // 于是更新的会话里新增的内容永远同步不下来，标题和时间也不会刷新。
@@ -537,9 +545,6 @@
     }
     const serverIds = new Set((data.conversations ?? []).map((c) => c.id))
 
-    // 缓存获取：服务端一次性下发所有缓存回答，之后继续推送新的；收到即落地
-    void applyCachedStream(headers)
-
     // 全量同步：直接用服务端列表覆盖本地
     //   - 服务端没有的本地会话 → 删除（含正文缓存）
     //   - 服务端有、本地没有 → 建占位
@@ -627,9 +632,12 @@
       const summaries = await db.loadSessionSummaries()
       if (epoch === historyEpoch) {
         set({ ready: true, sessions: sortSessions(summaries) })
+        startNetworkGuard()
         void verifySession()
-        // 登录后后台把服务端会话合并进来（网页端与 App 共用同一份服务端数据）
-        void syncFromServer()
+        // 登录后后台做一次**全量**同步：增量同步依赖 last_sync 游标，断线期间
+        // 完成生成、且游标已被探测推进过的会话会被漏掉；全量同步返回所有会话，
+        // 建好占位后 reply_cache 才能落地，保证断线重进一定拿回后台跑完的正文。
+        void syncFromServer(undefined, true)
       }
     } catch (error) {
       if (epoch === historyEpoch) {
@@ -645,6 +653,45 @@
   return initTask
 }
 
+/**
+ * 网络恢复守护：断线后持续探测，连上就同步。与 streamReply 的 X-Resume 重连互补。
+ */
+let netGuardStarted = false
+const NET_PROBE_INTERVAL = 30_000
+
+const probeAndSync = async (): Promise<void> => {
+  const token = useSettings.getState().apiKey
+  if (!token) {
+    return
+  }
+  try {
+    // 用 /health 探活，**绝不能用 /v1/sync**：服务端每次 sync 都会把 last_sync
+    // 推进到 now，探测请求会把真正的变化「吃掉」——生成完成后会话 updated_at
+    // 落在探测时刻之前，下一次增量同步就永远拉不到它（表现为重进没正文）。
+    const res = await fetch(`${API_ORIGIN}/health`, { cache: 'no-store' })
+    if (res.ok) {
+      void syncFromServer()
+    }
+  } catch {
+    // 探测失败：下个周期再试
+  }
+}
+
+const startNetworkGuard = (): void => {
+  if (netGuardStarted || typeof window === 'undefined') {
+    return
+  }
+  netGuardStarted = true
+  window.addEventListener('online', () => {
+    void probeAndSync()
+  })
+  window.setInterval(() => {
+    if (useSettings.getState().apiKey && !get().importingHistory) {
+      void probeAndSync()
+    }
+  }, NET_PROBE_INTERVAL)
+}
+
 /** 同一会话合并并发读取；切换只改变 activeId，旧请求不会把界面切回去。 */
 /** 从服务端拉取单个会话的正文，补进本地占位会话 */
 export const loadRemoteSession = async (id: string): Promise<boolean> => {
@@ -1522,10 +1569,16 @@
 }
 
 /** 重传前的退避等待；停止生成时立即返回，由调用方按已停止收尾 */
-const waitForRetry = (attempt: number, signal: AbortSignal) =>
+const waitForRetry = (
+  attempt: number,
+  signal: AbortSignal,
+  base = FILE_RETRY_BASE_DELAY,
+  max = Infinity
+) =>
   new Promise<void>((resolve) => {
     function onAbort() {
       clearTimeout(timer)
+      signal.removeEventListener('abort', onAbort)
       resolve()
     }
     const timer = setTimeout(
@@ -1533,7 +1586,7 @@
         signal.removeEventListener('abort', onAbort)
         resolve()
       },
-      FILE_RETRY_BASE_DELAY * 2 ** attempt
+      Math.min(base * 2 ** attempt, max)
     )
     signal.addEventListener('abort', onAbort, { once: true })
   })
@@ -1556,6 +1609,7 @@
   let reply = initial
   let interruptRetry = 0
   let resumeUsed = false
+  let reconnectAttempts = 0
   let frame = 0
   const flush = () => {
     cancelAnimationFrame(frame)
@@ -1655,6 +1709,11 @@
           }
         }
         reply = settleReply(reply, 'done', Date.now())
+        if (reconnectAttempts > 0) {
+          void syncFromServer()
+          const token = useSettings.getState().apiKey ?? ''
+          void applyCachedStream({ Authorization: `Bearer ${token}`, 'x-api-key': token })
+        }
         break
       } catch (error) {
         if (controller.signal.aborted) {
@@ -1701,16 +1760,21 @@
           }
           continue
         }
-        // 流被中断（缺 message_stop / 连接被掐）：服务端仍在生成，重连一次从头重放
+        // 连接断开：服务端仍在后台生成，持续重连直到成功。
+        // 指数退避 1s→2s→4s…封顶 30s，只有用户主动停止才退出。
         if (
-          attempt < 1 &&
-          !resumeUsed &&
           (error instanceof ApiError) &&
           (error.kind === 'interrupted' || error.kind === 'network')
         ) {
+          reconnectAttempts += 1
           resumeUsed = true
           reply = { ...initial, blocks: [] }
           updateReply(sessionId, reply)
+          await waitForRetry(reconnectAttempts - 1, controller.signal, RECONNECT_BASE_DELAY, RECONNECT_MAX_DELAY)
+          if (controller.signal.aborted) {
+            reply = settleReply(reply, 'stopped', Date.now())
+            break
+          }
           attempt = -1
           continue
         }
```

## 构建（务必带生产域名）

```bash
CHAT_API_ORIGIN=https://api.example.com pnpm build
# dist/ → bash app/android/sync-web.sh <dist> → cd app/harmony && bash sync-web.sh
```

`pnpm type` 必须过；lint 的 no-console/no-non-null-assertion 是上游既有基线，不要新增。

## 2026-11-22 追补：SSE 错误也能触发失效附件重传

超限长图（单边 >8192 像素）在服务端被切成多片，但历史会话里缓存的旧
`serverId` 仍指向未切分原图。服务端在收到「image is too large」时会把错误
改写成前端可识别的 `file_ids do not exist or are not created under your
account: <ids>`（见 job.go 的 appendUpstreamError），触发本仓库的
`reuploadInvalidatedFiles` 自愈链路。

但 `src/services/api/errors.ts` 的 `getFileErrorCode` 原来只认
`error.status === 400`；SSE 的 error 帧经 `streamMessage` 抛出的是
`ApiError('stream', message)`、`status=undefined`，自愈不会触发。改为：

```diff
 export const getFileErrorCode = (error: unknown): FileErrorCode | null => {
  if (!(error instanceof ApiError)) {
    return null
  }
- if (error.status !== 400) {
+ // HTTP 400 或 SSE error 帧（status=undefined）都按文案前缀判断
+ if (error.status !== 400 && error.status !== undefined) {
    return null
  }
  return FILE_ERROR_PATTERNS.find(([, pattern]) => pattern.test(error.message))?.[0] ?? null
 }
```

这样「超限错误 → 重置 serverId → 重新上传 → 服务端切分」才闭环。
版本 1.7.7(92)，全端同步。

## 2026-11-22 追补：同步官方 v0.1.3 附件安全修复（选择性移植）

官方 deepseek-ai/chat-demo v0.1.1 → v0.1.3 有两组更新：
1. `4ee2972` 附件预览/回复链接安全（SVG 缩略图 data: 隔离、`useFileView`、
   `isWebUrl` URL 白名单、附件链接带 target/rel）——**已选择性移植**；
2. `b53b77f` 超大图片上传前缩放（resizeUploadImage，长边 4096）——**未移植**：
   我们用服务端切分方案（见 AGENTS.md「超限图片自动切分」），缩放会损失长截图细节，
   与用户定的方案相悖。

**不全量同步**：官方分支没有我们的 `syncFromServer`/`openSessionById`/`focusMessage`/
`remotePending`/HistoryStep/RhaiStep/上传进度环等自研功能，直接覆盖会全冲掉。
逐文件手动把安全逻辑并入我们的版本，保留所有自研代码：
- 新增 `src/utils/url.ts`（isWebUrl，toLocal.ts 原本地实现改为统一 import）、
  `src/components/files/useFileView.ts`（依赖我们补的 `attachments.getFileViewType`）；
- `attachments.ts` 新增 `getFileViewType` + `VIEW_MIME_BY_EXT`（纯新增，不碰上传逻辑）；
- FileCard / MessageFile 用官方版（无自研冲突）；
- DraftAttachments 保留自研进度环/previewUrl，只换安全 url/link；
- view.ts 保留自研类型，registry.add 加 isWebUrl 白名单（返回 null 的调用点全部判空）；
- UserMessage 保留 data-mid/stopReadAloud，只把展示 MIME 判定改为 getFileViewType；
- ReplyArea 保留 HistoryRow/RhaiRow，只加 PageLink 的 `!page.url` 失败态。

版本 1.7.8(93)，`pnpm type` 通过、SVG/noreferrer 已确认进包，全端已发布。

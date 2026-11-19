# 前端补丁：同步陈旧判定改为「条数不等」

前端源码不在本仓库（chat-demo 上游 + `CHAT_API_ORIGIN` 编译期注入），这里记录
必须在源码里做的改动，重新构建前端时照此应用，否则 App 端会一直用旧缓存渲染。

## 背景

服务端把「工具轮的空正文 assistant 消息」合并成一条再返回视图
（`server/chat-server/store.go` 的 `coalesceToolSteps`）。合并后视图条数会**变少**
（一次联网搜索能少三四条）。

App 端有本地缓存，打开会话时不会无条件重拉；`syncFromServer` 里用
「服务端条数 vs 本地条数」判断要不要标 `remotePending` 重拉，原判据是：

```ts
if (serverCount <= localCount && serverUpdated <= summary.updatedAt + 2000) continue
```

只判「服务端变多」——视图条数变少的情况永远命中不了，于是 App（安卓/鸿蒙）
重启后仍是旧缓存，网页端因为首次拉取反而正常。

## 补丁

文件：`src/services/chat/store.ts`，`syncFromServer` 里构造 `staleIds` 的那段。

```diff
       const serverUpdated = Math.round((conv.updated_at ?? 0) * 1000)
-      if (serverCount <= localCount && serverUpdated <= summary.updatedAt + 2000) {
+      // 条数**不等**就重拉，不能只判 serverCount <= localCount：
+      // 服务端把工具轮的空正文消息合并成一条之后，视图条数是会变少的
+      // （一次联网搜索能少三四条）。只判「变多」会让客户端一直拿旧缓存渲染，
+      // 表现为服务端改完了、重启 App 还是老样子。
+      if (serverCount === localCount && serverUpdated <= summary.updatedAt + 2000) {
         continue
       }
```

前提：服务端 `/v1/sync` 返回的 `messages` 必须是**视图条数**（与
`/v1/conversations/{id}` 的 `tree` 长度一致），见 `viewMessageCount`。

## 构建

```bash
cd <chat-demo 源码目录>
CHAT_API_ORIGIN=https://api.example.com pnpm build     # 注意不要漏 CHAT_API_ORIGIN
cd <repo>/app/android && bash sync-web.sh <源码目录>/dist
cd <repo>/app/harmony && bash sync-web.sh
```

`CHAT_API_ORIGIN` 漏了会打成上游默认的 `https://chat.example.com/chat`，
App 里接口全 404。

# 客户端

三个壳承载**同一套静态前端**：安卓 WebView、鸿蒙 ArkWeb、浏览器。
下面只讲怎么把它们指向你自己的后端。

## 0. 先改接口地址

前端产物里的接口基址是**编译期**写死的（构建时注入 `CHAT_API_ORIGIN`），所以：

```bash
python3 tools/set-api-origin.py https://api.example.com   # 就地改写 assets / rawfile / 网页端登录页
python3 tools/set-api-origin.py --check                   # 确认三处一致
```

它只改「`https://host` 后面紧跟 `/chat` 或 `/auth`」的位置，其余链接不动。
安卓打包也可以不改工作树，直接在打包时注入：

```bash
cd client/android && bash build.sh --api-origin https://api.example.com
```

## 1. 安卓

```bash
cd client/android
bash bump.sh                      # 版本号 +1（可选，函数级改动才需要）
bash build.sh                     # → dist/deepseek-1.0.0.apk
```

- 依赖（apktool/android.jar/r8/uber-apk-signer）首次构建自动下载到 `tools/`。
- 签名用脚本自建的 `tools/deepseek.jks`（口令 `deepseek123`），仅供侧载；
  要上架或长期分发，换自己的密钥库并把口令从脚本里挪出去。
- 产物自检会打印包内接口地址 —— **这是最容易出错的地方**，一定看一眼。

实现要点（改壳之前必读）：

- **内置静态服务的端口必须固定 18888**：WebView 的 localStorage / IndexedDB 按
  origin（含端口）隔离，端口一变登录态和本地记录就全丢。`MiniServer` 先重试再退回随机端口。
- 页面通过 `http://127.0.0.1:18888/` 提供，而不是 `file://` —— file 协议下 WebView
  会限制存储。
- `onShowFileChooser` 必须自己接，否则聊天页的「图片 / 文件 / 拍照」点了没反应；
  `capture` 属性要走系统相机。
- APK 里不能出现 lambda（javac 在 android.jar 上找不到 `LambdaMetafactory`）。

应用内更新：`Updater.java` 的 `MANIFEST_URL` **默认留空 = 关闭**。
要开就填自己的清单地址，并用 `tools/make-manifest.py` 生成清单（别手写，size/sha256 必须对得上）。
清单格式：

```json
{
  "version": "1.0.0",
  "versionCode": 1,
  "notes": "改了什么",
  "platforms": {
    "android": {"versionCode": 1, "url": "https://…/deepseek-1.0.0.apk",
                "size": 2030845, "sha256": "…"}
  }
}
```

## 2. 鸿蒙

需要 DevEco Studio 的 SDK（`DEVECO_SDK_HOME`）与 `hvigorw`，本仓库不自带。

```bash
cd client/harmony
bash sync-web.sh                  # 把安卓壳的前端产物同步进 rawfile（清空后拷贝）
bash build.sh dev                 # 本地自签，能装模拟器
bash build.sh                     # 有 signature/ 材料时出签名包，可装真机
```

- `signature/` 目录（p12 / cer / profile）**不入库**，要自己从 AGC 申请；
  零售华为设备只认 AGC 证书链，自签链会被 `HapVerify: it do not come from trusted root` 拒掉。
- `bundleName` 必须与 AGC 应用一致（默认 `com.example.deepseek`，按需改）。
- 页面不是 `$rawfile`，而是用 `onInterceptRequest` 拦截 `https://appassets.local/` 后喂
  rawfile 内容 —— 同样是 origin 稳定性问题。**非本域请求必须 `return null`**，
  否则接口请求和 SSE 会被拦掉，表现为页面一直转圈。
- 鸿蒙没有 `REQUEST_INSTALL_PACKAGES` 等价能力，第三方应用拉不起 HAP 安装器，
  所以「立即更新」只能 `openLink(下载页)`，由用户手动安装。

## 3. 网页端

不需要构建，直接把 `client/android/app/assets` 当静态站点发布即可（改好接口地址之后）：

```bash
python3 tools/set-api-origin.py https://api.example.com
scp -r client/android/app/assets/* root@chat.example.com:/opt/chatweb/www/
```

nginx 模板见 [deploy/nginx-api.conf.example](../deploy/nginx-api.conf.example) 末尾那个
被注释掉的 server 块。登录页 `login.html` 也在这套静态文件里。

## 4. 想改界面？

前端源码不在本仓库。流程是：

1. clone 上游 [chat-demo](https://github.com/deepseek-ai/chat-demo)，按
   [docs/frontend-patches](frontend-patches) 打补丁（不打会一直用旧缓存渲染）；
2. `pnpm build`；
3. `cd client/android && bash sync-web.sh <chat-demo/dist>` 同步进 assets
   （脚本会保留自维护的 `login.html` 与 `icon.png`，其余整体替换，避免旧哈希 chunk 堆积）；
4. 重新打包。

鸿蒙侧再跑 `bash client/harmony/sync-web.sh` 把 assets 同步进 rawfile。

## 5. 版本规则

- 唯一来源 `client/android/version.txt`，`VERSION_CODE` 必须单调递增
  （客户端只认 code，code 不涨就等于没发版）。
- 鸿蒙的版本号由 `client/harmony/build.sh` 从同一个文件写入 `AppScope/app.json5`，
  两个平台一起涨。
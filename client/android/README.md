# Android 壳

把 [deepseek-ai/chat-demo](https://github.com/deepseek-ai/chat-demo)（纯静态 React 应用）
装进一个 WebView 壳，做成可安装的 APK —— 但没有 8MB 的 Gradle 工程：
`build.sh` 直接走 aapt2 官方链路，只需要 JDK。

产物：`dist/deepseek-<版本>.apk`（约 1.9 MB，自签名，可直接侧载）。

```bash
bash build.sh --api-origin https://api.example.com   # 打包时注入你的后端地址
bash bump.sh && bash build.sh                        # 发版：先抬版本号再打
```

## 关键设计

- **本地服务用固定端口 18888**：WebView 的 `localStorage` / `IndexedDB` 按 origin（含端口）
  隔离，用随机端口会导致**每次启动都是新 origin**，登录状态和聊天记录全部丢失、必须重新登录。
  端口被占用时先重试若干次（上一次进程可能还在收尾），实在不行才退回随机端口 ——
  此时只能保证本次会话内可用。
- **不用 `file://`**：chat-demo 把令牌放 `localStorage`、会话放 `IndexedDB`，
  WebView 在 `file://` 下会限制这两者。所以 App 内起了一个只监听 `127.0.0.1`
  的极简静态服务（`MiniServer.java`），页面通过 `http://127.0.0.1:18888/index.html` 打开。
- 网页资源全部打进 `assets/`（约 153 个文件，来自 chat-demo 的 `dist/`）。
- `network_security_config.xml` 只对 `127.0.0.1` / `localhost` 放开明文，其余仍强制 HTTPS。
- 申请了 `INTERNET` 与 `RECORD_AUDIO`（语音输入），`onPermissionRequest` 授权给网页。
- 返回键先让 WebView 后退，退不动再退出 App。

## 接口地址怎么进去的

前端产物里的接口基址是**编译期**写死的（上游用 `CHAT_API_ORIGIN` 注入），
所以本壳在打包时做等价的事：

```
bash build.sh --api-origin https://api.example.com
```

它把 `app/assets` 复制到 `.work/assets`（**不动工作树**），把「`https://host` 后面紧跟
`/chat` 或 `/auth`」的位置替换掉，再用这个副本打包。
不传 `--api-origin` 就用 assets 里现成的地址（仓库里是占位 `api.example.com`）。

仓库根的 `tools/set-api-origin.py` 做同一件事，但会**就地改动工作树**，
适合一次改好、连网页端一起发布。两个入口别混着用。

打包结束会打印**从 APK 里读回来的接口地址** —— 这是最贵的错误，一定扫一眼。

## 更新网页部分

chat-demo 的新构建产物要同步进 `app/assets`，用脚本做，别用 `cp -a dist/. assets/` 覆盖：

```bash
bash sync-web.sh /path/to/chat-demo/dist
```

原因：rspack 每次构建的 chunk 文件名带哈希，直接覆盖会**保留上一版的旧 chunk**，
assets 越滚越大、APK 也跟着变大（曾从 1.98 MB 涨到 2.38 MB）。
`sync-web.sh` 只保留我们自维护的 `login.html` / `icon.png`，其余全清再拷；
`build.sh` 开头也会检查，发现多份 `main.*.js` 直接报错。

## 版本号

版本集中在 `version.txt`（`VERSION_NAME` / `VERSION_CODE`），构建脚本读它注入 manifest：

```bash
bash bump.sh && bash build.sh     # 1.0.2(3) → 1.0.3(4)
```

产物名带版本号：`dist/deepseek-<versionName>.apk`。
**`VERSION_CODE` 必须单调递增** —— 客户端的自动更新只比 code，code 不涨就等于没发版。

## 构建链路

`javac` + `d8` 编出 `classes.dex` → `aapt2 compile/link`（`-A assets` 一并打包）→
合并 dex → `zipalign` + v1/v2/v3 签名 → 产物。

- 依赖（apktool.jar / android.jar / r8 / uber-apk-signer）首次构建自动下载到 `tools/`；
  aapt2 直接从 `apktool.jar` 的 `prebuilt/linux/aapt2_64` 取出，不需要 Android SDK build-tools。
- 签名用脚本自建的 `tools/deepseek.jks`（口令 `deepseek123`），仅供侧载 ——
  要上架或长期分发，换成自己的密钥库，并把口令从脚本里挪到环境变量。
- **代码里不能用 lambda**：javac 以 `android.jar` 为 bootclasspath 时找不到
  `java.lang.invoke.LambdaMetafactory`，会直接编译失败。

## 自动更新（默认关闭）

`Updater.java` 的 `MANIFEST_URL` 默认留空 = 不检查更新，设置页的「检查更新」会提示未配置。
要开启：

1. 把产物传到自己的下载站；
2. 用 `tools/make-manifest.py` 生成 `latest.json`（**别手写**，`size` / `sha256` 必须对得上）；
3. 把 `MANIFEST_URL` 改成你的清单地址，重新打包。

只比 `versionCode`：比本地大就弹窗（立即更新 / 稍后 / 忽略此版本），
下载到 `cacheDir/update/`、按 `size` 校验完整后经 `content://`（`ApkProvider`）
交给系统安装器。「忽略此版本」记在 SharedPreferences，同一个 code 不再打扰。
Android 8+ 装应用需要 `REQUEST_INSTALL_PACKAGES` + 用户授权，未授权时跳设置页。

## 装不上怎么办

MIUI / HyperOS 的安装器（尤其开着「纯净模式 / 应用安装校验」）会直接报
「无效安装包 / 与操作系统不兼容 / 请使用官方版本进行安装」，这跟包本身无关：

```bash
# 1) 走 adb，绕过安装器 UI，失败时也会打印真实原因
adb install -r dist/deepseek-1.0.0.apk

# 2) 或手机关掉：设置 → 应用设置 → 应用管理 → 右上角 → 安装未知应用 / 纯净模式 / 应用校验
```

**纯血鸿蒙（HarmonyOS NEXT）装不了 Android APK**，用 `../harmony` 那份工程出 `.hap`。

## 交互上的两个坑（已修，改前端时别改回去）

1. **「+」面板里拍照 / 相册 / 文件点了没反应** —— 网页里的 `<input type="file">` 在 WebView 中
   必须由 `WebChromeClient.onShowFileChooser` 接住，否则点击被静默吞掉。
   现在用 `FileChooserParams.createIntent()` 起系统选择器（自带 accept / multiple / capture），
   在 `onActivityResult` 里把 URI 回传给 `ValueCallback<Uri[]>`；
   `capture=environment` 时改走系统相机。
2. **语音按钮一按就结束** —— 指针流程结束后浏览器会**补发一次 click**，
   导致「按下开始录音 → 抬起触发 click → 又 toggle 一次立刻停止」。
   修法：`pointerup` 时置 `suppressClickRef`，click 处理器第一次直接吞掉；
   按住不足 300ms 视为误触不转写；按钮加 `touch-action: none` 防滚动打断。

## 上传与麦克风

- **大图上传 413**：nginx 的 `client_max_body_size` 默认 4m，手机照片经常超；
  `deploy/nginx-api.conf.example` 里已放宽到 64m。
- **语音没反应**：`RECORD_AUDIO` 光在清单里声明不够，还要运行时申请，
  否则 WebView 里 `getUserMedia` 直接失败。`MainActivity.onCreate` 里已处理。
- 图片上传**不要带 `purpose`**：上游只接受 `user_data` / `text_recognition` /
  `rich_layout_recognition`，传 `vision` 会 400。

## 登录页

`assets/login.html` 是自建的登录 / 注册页（两个 tab，注册要邮箱验证码）。
`MainActivity` 启动时先加载 `index.html`，页面内的守卫发现没令牌才跳登录页；
拿到令牌后写进 `chat-demo:api-key`，之后所有请求自动带上，账号在服务端按令牌区分。

三种入口：

1. **OIDC 登录** —— 跳到 `TINYAUTH_ISSUER`，回调带 `#token=` 回到 App 自动存下；
2. 邮箱 + 密码（`/auth/v1/auth/login`）；
3. 邮箱注册（`/auth/v1/auth/register`，需要服务端配好 SMTP）。

接口见 [../../docs/api.md](../../docs/api.md)。

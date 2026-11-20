# chat-demo HarmonyOS 壳

把 [deepseek-ai/chat-demo](https://github.com/deepseek-ai/chat-demo) 的静态站点装进
ArkWeb（`Web` 组件），做成一个鸿蒙原生应用 —— 与 `../android` 的安卓壳功能对齐，
跑同一套后端（[../../server/chat-server](../../server/chat-server)）。

鸿蒙 5（API 12+），ArkTS 声明式 UI，无第三方依赖。

## 与安卓壳的对应关系

| 能力 | 安卓（`../android`） | 鸿蒙（本目录） |
| --- | --- | --- |
| 页面来源 | 内置 `MiniServer`，`http://127.0.0.1:18888` | `onInterceptRequest` 拦截 `https://appassets.local/`，直接返回 rawfile |
| 为什么不用 `file://` | WebView 在 file:// 下 localStorage 受限 | 同：`resource://` 源下 ArkWeb 的 localStorage 不可靠 |
| 登录态 | origin 固定（端口 18888）⇒ localStorage 持久 | origin 固定（自定义 https 域）⇒ localStorage 持久 |
| 设备类型 | `login.html` 按 hostname=127.0.0.1 判 `android` | 同页按 hostname=appassets.local 判 `harmony` |
| 文件选择 | `WebChromeClient.onShowFileChooser` + 系统相机 | `onShowFileSelector` + 相册/文档选择器 + `cameraPicker` |
| 麦克风 | `onPermissionRequest` → `grant` | 同回调，但先 `requestPermissionsFromUser` 再由我们 `grant` |
| 返回键 | `WebView.canGoBack` → `goBack` | `onBackPress` → `controller.accessBackward/backward` |
| 外部链接 | 非 http(s) 交给系统 `ACTION_VIEW` | `onLoadIntercept` → `context.openLink` |
| 检查更新 | 拉 latest.json 比 versionCode，下载 APK 交安装器 | 拉同一份 latest.json，弹窗后**打开下载页**（见下方限制） |
| 网页调用原生 | `addJavascriptInterface(UpdateBridge, "chatUpdate")` | `javaScriptProxy({name:'chatUpdate'})`，方法名一致 |

## 目录结构

```
AppScope/app.json5                       bundleName（org.selfdeepseek.app）/ 版本 / 图标
entry/src/main/module.json5              权限（INTERNET、MICROPHONE）、入口 Ability
entry/src/main/ets/entryability/         应用入口（窗口、状态栏）
entry/src/main/ets/pages/Index.ets       Web 组件 + 各回调
entry/src/main/ets/common/AssetServer.ets  rawfile → WebResourceResponse（MIME/SPA 回退/缓存）
entry/src/main/ets/common/FilePicker.ets   相册 / 文档 / 相机
entry/src/main/ets/common/Updater.ets      清单拉取、版本比较、弹窗、跳过记录
entry/src/main/ets/common/AppInfo.ets      versionCode / versionName
entry/src/main/resources/rawfile/        前端产物（= ../android/app/assets 的副本）
build.sh / sync-web.sh                   构建与同步脚本
tools/make-dev-cert.py                   生成 OpenHarmony 测试证书链与 profile
signature/                               签名材料（不入库，见下）
```

## 构建

前置：鸿蒙命令行工具链（Linux 上官方只提供 CLI）。装在 `~/harmonyos/command-line-tools`，
`~/harmonyos/env.sh` 提供 `hvigorw` / `hdc` / `DEVECO_SDK_HOME`。

```bash
bash sync-web.sh          # 从 ../android/app/assets 同步前端到 rawfile
bash build.sh             # debug + release（无签名材料时出 unsigned）
bash build.sh debug       # 只 debug
bash build.sh release     # 只 release
bash build.sh dev         # 本地自签（OpenHarmony 测试链），可装模拟器
bash build.sh verify      # 校验 dist/ 里已有产物的签名
```

- 版本号唯一来源是 `../android/version.txt`，`build.sh` 构建前把它写进 `AppScope/app.json5`
  （两个平台同版本同 versionCode）。
- hvigor 的缓存目录默认 `~/.hvigor`；只读环境（沙箱 / CI）里自动退到 `./.tmp/hvigor-home`。
- 产物在 `dist/chat-demo-harmony-<版本>-<模式>[-unsigned].hap`。

## 签名

**零售华为设备只认 AGC（AppGallery Connect）签发的证书链**，自签 CA 与 SDK 自带的
OpenHarmony 测试链都会被 `HapVerify` 拒掉（`it do not come from trusted root`）。
所以分两条路：

### 1) 真机安装：AGC 材料

在 DevEco Studio「自动签名」里生成一次（需要登录华为账号），把三件套拷到 `signature/`：

```
signature/
├── sign.env                下面的字段
├── app.p12                 含应用私钥的 keystore
├── app.cer                 AGC 下发的应用证书
└── app-profile.p7b         AGC 下发的 profile
```

```ini
# signature/sign.env —— 口令含空格也没关系，值加引号
STORE_FILE=app.p12
STORE_PWD="你的keystore口令"
KEY_ALIAS="你的别名"
KEY_PWD="你的密钥口令"
CERT_FILE=app.cer
PROFILE_FILE=app-profile.p7b
```

`bundleName` 必须与 AGC 应用一致（现在是 `org.selfdeepseek.app`，`agconnect-services.json`
里的 `package_name` 也是它）—— 不一致时安装会报 profile 与包名不匹配。

```bash
bash build.sh debug       # 有 signature/sign.env 时自动签名并校验
hdc install dist/chat-demo-harmony-<版本>-debug.hap
```

demo 用的调试 profile 里 `debug-info.device-ids` 若限定了 UDID，要先
`hdc shell bm get --udid` 取本机 UDID 再回 AGC 重签 profile。

### 2) 没有 AGC 材料：本地自签

```bash
bash build.sh dev         # 用 SDK 自带 OpenHarmony 测试链签名
```

能通过 `hap-sign-tool verify-app`（`Digest verify result: true`），可装模拟器 /
OpenHarmony 开发板；**装不到零售华为设备**。它同时是签名链路的自检：
这条通过而真机装不上，就说明材料不匹配而不是配置写错了。

## 关键实现点（改之前先看）

- **origin 必须恒定**：`APP_ORIGIN = https://appassets.local`（`AssetServer.ets`）。
  ArkWeb 的 localStorage / IndexedDB 按 origin 隔离，换域名或换端口 = 用户重新登录、会话列表清空。
- **拦截只应答自己的域**：`onInterceptRequest` 对**所有**请求触发，非本域一律 `return null`
  放行，否则接口请求会被吞掉（表现是页面一直转圈）。
- **SPA 回退**：路径没有扩展名时回退 `index.html`，否则刷新 `/xxx` 会 404。
- **二进制响应不要设 encoding**：`setResponseEncoding('utf-8')` 只对文本类型设，
  否则图片 / 字体可能被当文本处理。
- **rawfile 首屏缓存**：`AssetServer` 把读过的文件留在内存 Map 里，首屏几十个 chunk 只读一次。
- **`javaScriptProxy` 要在 build 前有对象**：`UpdateBridge` 无参构造，`aboutToAppear`
  里 `attach(updater)`，否则窗口一建就要读 `window.chatUpdate` 时拿不到。
- **弹窗走 UIContext**：全局 `promptAction.showDialog` / `getContext()` 在 API 12 已废弃，
  统一用 `this.getUIContext().getPromptAction()` / `getHostContext()`。
- **文件回填是异步的**：`onShowFileSelector` 必须同步 `return true`，选完再
  `result.handleFileList(uris)`；用户取消要回填空数组，否则网页一直等。

## 已知限制

- **不能自助安装新版本**：鸿蒙没有安卓 `REQUEST_INSTALL_PACKAGES` 的等价能力，
  第三方应用无法拉起 HAP 安装器。所以「立即更新」只会打开下载页，用户下载后在
  文件管理里手动安装（或上架应用市场，走市场更新）。
- **未在真机验证**：本机没有鸿蒙设备 / 模拟器，ArkTS 编译与签名校验已通过，
  拦截式静态资源与各回调的实际行为需要真机冒烟（重点：首屏是否白屏、
  登录态是否跨次启动保留、语音输入权限弹窗）。
- 清单里的 `platforms.harmony` 由 `tools/make-manifest.py` 生成；没有该字段时
  回退到 `platforms.android` 的 versionCode（两平台版本号同步递增）。

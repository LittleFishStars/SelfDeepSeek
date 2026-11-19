import { webview } from '@kit.ArkWeb';
import { abilityAccessCtrl, common, PermissionRequestResult, Permissions } from '@kit.AbilityKit';
import { AppInfo } from '../common/AppInfo';
import { APP_ORIGIN, AssetServer, START_PAGE } from '../common/AssetServer';
import { FilePicker } from '../common/FilePicker';
import { UpdateBridge, Updater } from '../common/Updater';
import { Log } from '../common/Logger';

const MICROPHONE: Permissions = 'ohos.permission.MICROPHONE';

/**
 * 主页面：一个 Web 组件承载 chat-demo 静态站点。
 *
 * 静态资源不走文件系统也不走端口，而是由 AssetServer 在
 * onInterceptRequest 里直接应答 https://appassets.local/ 的请求 ——
 * 这样 origin 固定，localStorage（登录令牌、会话）才能长期存活。
 * 其余请求（接口 https://api.example.com、登录跳转）返回 null 放行。
 */
@Entry
@Component
struct Index {
  private controller: webview.WebviewController = new webview.WebviewController();
  private assets: AssetServer | null = null;
  private updater: Updater | null = null;
  private bridge: UpdateBridge = new UpdateBridge();

  aboutToAppear(): void {
    const ctx = this.hostContext();
    this.assets = new AssetServer(ctx.resourceManager);
    this.updater = new Updater(ctx, this.getUIContext());
    this.bridge.attach(this.updater);
    // 自动更新：延迟几秒在后台查清单，不参与首屏渲染，失败静默
    this.updater.checkOnStartup();
  }

  /** 取本页所在的 UIAbilityContext（getContext 全局函数在 API 12 已废弃） */
  private hostContext(): common.UIAbilityContext {
    return this.getUIContext().getHostContext() as common.UIAbilityContext;
  }

  build() {
    Column() {
      Web({ src: START_PAGE, controller: this.controller })
        .width('100%')
        .height('100%')
        .backgroundColor('#FFFFFF')
        .javaScriptAccess(true)
        .domStorageAccess(true)
        .databaseAccess(true)
        .fileAccess(true)
        .imageAccess(true)
        .onlineImageAccess(true)
        .mixedMode(MixedMode.All)
        .mediaPlayGestureAccess(false)
        .zoomAccess(false)
        .horizontalScrollBarAccess(false)
        .verticalScrollBarAccess(false)
        .darkMode(WebDarkMode.Off)
        .forceDarkAccess(false)
        .onControllerAttached(() => {
          this.tuneUserAgent();
        })
        .javaScriptProxy({
          object: this.bridge,
          name: 'chatUpdate',
          methodList: ['available', 'versionName', 'check'],
          controller: this.controller
        })
        .onInterceptRequest((event: OnInterceptRequestEvent) => {
          const url: string = event.request.getRequestUrl();
          if (this.assets === null || !url.startsWith(APP_ORIGIN + '/')) {
            // 官方语义：返回 null 表示不拦截，交给 Web 内核走真实网络
            return null;
          }
          return this.assets.respond(url);
        })
        .onShowFileSelector((event: OnShowFileSelectorEvent) => {
          this.pickAndSubmit(event);
          return true;
        })
        .onPermissionRequest((event: OnPermissionRequestEvent) => {
          this.grantWebPermission(event);
        })
        .onLoadIntercept((event: OnLoadInterceptEvent) => {
          return this.openExternalIfNeeded(event.data.getRequestUrl());
        })
    }
    .width('100%')
    .height('100%')
  }

  /** 返回键：优先网页历史后退，退到底才退出应用 */
  onBackPress(): boolean {
    try {
      if (this.controller.accessBackward()) {
        this.controller.backward();
        return true;
      }
    } catch (e) {
      Log.warn('后退失败: ' + JSON.stringify(e));
    }
    return false;
  }

  /**
   * 在默认 UA 后追加应用标识。
   * 前端 login.html 靠 UA 里的 ArkWeb / HarmonyOS 判定设备类型（harmony），
   * 即使这里失败也不影响判定 —— ArkWeb 默认 UA 本身就带这些字样。
   */
  private tuneUserAgent(): void {
    try {
      const ua: string = this.controller.getUserAgent();
      this.controller.setCustomUserAgent(ua + ' chat-demo-harmony/' + AppInfo.versionName());
    } catch (e) {
      Log.warn('设置 UA 失败: ' + JSON.stringify(e));
    }
  }

  /**
   * 接管 <input type=file>。
   * 必须同步返回 true，然后异步弹选择器，选完再回填给内核；
   * 回填空数组表示用户取消，网页那边会收到空 FileList。
   */
  private pickAndSubmit(event: OnShowFileSelectorEvent): void {
    try {
      const ctx = this.hostContext();
      const accept: string[] = event.fileSelector.getAcceptType();
      const capture: boolean = event.fileSelector.isCapture();
      const mode: FileSelectorMode = event.fileSelector.getMode();
      const multiple: boolean = mode === FileSelectorMode.FileOpenMultipleMode;
      // 这条日志是排查「点了文件却弹相册」的关键：没有它说明回调根本没被调用，
      // 用户看到的是 ArkWeb 自带的 file chooser 面板，而不是我们做的这一步
      Log.info('fileSelector: accept=' + JSON.stringify(accept) + ' capture=' + capture + ' mode=' + mode);
      const result: FileSelectorResult = event.result;
      FilePicker.pick(ctx, accept, capture, multiple).then((uris: string[]) => {
        Log.info('fileSelector: 选中 ' + uris.length + ' 个文件');
        result.handleFileList(uris);
      }).catch((e: Error) => {
        Log.warn('文件选择失败: ' + e.message);
        result.handleFileList([]);
      });
    } catch (e) {
      Log.error('文件选择回调异常: ' + JSON.stringify(e));
      event.result.handleFileList([]);
    }
  }

  /**
   * 网页请求麦克风等能力：先向系统申请（未授权时会弹框），再授予网页。
   * 不申请直接 grant 是无效的 —— ArkWeb 只转达请求，真正的权限还得系统给。
   */
  private grantWebPermission(event: OnPermissionRequestEvent): void {
    const request: PermissionRequest = event.request;
    const resources: string[] = request.getAccessibleResource();
    const ctx = this.hostContext();
    const manager: abilityAccessCtrl.AtManager = abilityAccessCtrl.createAtManager();
    manager.requestPermissionsFromUser(ctx, [MICROPHONE])
      .then((result: PermissionRequestResult) => {
        if (result.authResults.length > 0 && result.authResults[0] === 0) {
          request.grant(resources);
        } else {
          Log.warn('麦克风权限被拒绝');
          request.deny();
        }
      })
      .catch((e: Error) => {
        Log.warn('申请麦克风权限失败: ' + e.message);
        request.deny();
      });
  }

  /**
   * 非 http(s) 跳转（tel:、mailto:、weixin:// 等）交给系统处理，页面不加载。
   * http(s) 一律放行，站内跳转与 OIDC 回调都靠它。
   */
  private openExternalIfNeeded(url: string): boolean {
    if (url.startsWith('http://') || url.startsWith('https://') || url.startsWith('about:')) {
      return false;
    }
    try {
      const ctx = this.hostContext();
      ctx.openLink(url).catch((e: Error) => {
        Log.warn('打开外部链接失败: ' + e.message);
      });
    } catch (e) {
      Log.warn('外部链接异常: ' + JSON.stringify(e));
    }
    return true;
  }
}

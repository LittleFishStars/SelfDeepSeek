import { common } from '@kit.AbilityKit';
import { http } from '@kit.NetworkKit';
import { preferences } from '@kit.ArkData';
import { promptAction, UIContext } from '@kit.ArkUI';
import { BusinessError } from '@kit.BasicServicesKit';
import { AppInfo } from './AppInfo';
import { Log } from './Logger';

/** 更新清单（由 app/android/publish.sh 生成，两平台共用一个文件） */
/** 留空 = 关闭自动更新；要开启就填自己的 latest.json 地址 */
const MANIFEST_URL: string = '';
const PREF_NAME: string = 'updater';
const KEY_SKIP_CODE: string = 'skip_version_code';
/** 启动后延迟多久检查：避开首屏渲染，别和登录校验抢网络 */
const STARTUP_DELAY_MS: number = 3000;

interface ManifestPlatform {
  versionCode?: number;
  version?: string;
  url?: string;
  size?: number;
  sha256?: string;
}

interface ManifestPlatforms {
  android?: ManifestPlatform;
  harmony?: ManifestPlatform;
}

interface Manifest {
  version?: string;
  versionCode?: number;
  notes?: string;
  platforms?: ManifestPlatforms;
}

/**
 * 应用自动更新。
 *
 * 与安卓版的根本差异：**鸿蒙不允许应用自己拉起安装器**。
 * 安卓版下载 APK 后用 content:// 交给 PackageInstaller；鸿蒙没有等价能力
 * （第三方应用无法安装 HAP，只能由应用市场或用户在文件管理里手动安装）。
 * 所以这里的「立即更新」是打开下载页，由用户下载后手动安装。
 *
 * 其他行为与安卓版保持一致：
 *   · 只比 versionCode（versionName 给人看，可能回退）
 *   · 启动检查失败静默，手动检查一定有反馈
 *   · 「忽略此版本」记在本机，同一 versionCode 不再打扰
 */
export class Updater {
  private ctx: common.UIAbilityContext;
  /** 弹窗必须走 UIContext：全局 promptAction 在 API 12 已废弃，多实例下会弹错窗口 */
  private ui: UIContext;
  private pref: preferences.Preferences | null = null;

  constructor(ctx: common.UIAbilityContext, ui: UIContext) {
    this.ctx = ctx;
    this.ui = ui;
  }

  /** 启动时调用：延迟检查，任何异常都不打扰用户 */
  checkOnStartup(): void {
    setTimeout(() => {
      this.check(false).catch((e: Error) => {
        Log.info('启动检查更新失败（忽略）: ' + e.message);
      });
    }, STARTUP_DELAY_MS);
  }

  /** 设置页「检查更新」：无论有没有新版都要给反馈 */
  checkManually(): void {
    this.check(true).catch((e: Error) => {
      this.toast('检查更新失败：' + e.message);
    });
  }

  private async check(manual: boolean): Promise<void> {
    if (MANIFEST_URL.length === 0) {
      if (manual) {
        this.toast('本版本未配置更新清单（MANIFEST_URL 为空）');
      }
      return;
    }
    let text: string;
    try {
      text = await this.httpGet(MANIFEST_URL);
    } catch (e) {
      if (manual) {
        throw e as Error;
      }
      return;
    }

    let manifest: Manifest;
    try {
      manifest = JSON.parse(text) as Manifest;
    } catch (e) {
      if (manual) {
        this.toast('检查更新失败：清单格式异常');
      }
      return;
    }

    const platform: ManifestPlatform | undefined = this.pickPlatform(manifest);
    if (platform === undefined) {
      if (manual) {
        this.toast('已是最新版本（' + AppInfo.versionName() + '）');
      }
      return;
    }

    const remoteCode: number = platform.versionCode === undefined ? 0 : platform.versionCode;
    const remoteName: string = platform.version !== undefined
      ? platform.version
      : (manifest.version === undefined ? '' : manifest.version);
    const url: string = platform.url === undefined ? '' : platform.url;
    const size: number = platform.size === undefined ? 0 : platform.size;
    const notes: string = manifest.notes === undefined ? '' : manifest.notes;

    if (remoteCode <= 0 || url.length === 0 || remoteCode <= AppInfo.versionCode()) {
      if (manual) {
        this.toast('已是最新版本（' + AppInfo.versionName() + '）');
      }
      return;
    }
    if (await this.skipCode() === remoteCode) {
      if (manual) {
        this.toast('已是最新版本（' + AppInfo.versionName() + '）');
      }
      return;
    }
    this.prompt(remoteCode, remoteName, url, size, notes);
  }

  /** 优先鸿蒙产物；清单里没有 harmony 块时退回 android 块（两平台版本号同步递增） */
  private pickPlatform(manifest: Manifest): ManifestPlatform | undefined {
    const platforms: ManifestPlatforms | undefined = manifest.platforms;
    if (platforms === undefined) {
      return undefined;
    }
    if (platforms.harmony !== undefined) {
      return platforms.harmony;
    }
    return platforms.android;
  }

  private prompt(remoteCode: number, remoteName: string, url: string,
    size: number, notes: string): void {
    let message: string = '版本 ' + remoteName + '（' + this.humanSize(size) + '）';
    if (notes.length > 0) {
      message += '\n\n' + notes;
    }
    message += '\n\n下载后请在「文件管理」里打开 HAP 完成安装。';

    const buttons: promptAction.Button[] = [
      { text: '立即更新', color: '#4d6bfe' },
      { text: '稍后', color: '#8a8a8e' },
      { text: '忽略此版本', color: '#8a8a8e' }
    ];
    this.ui.getPromptAction().showDialog({
      title: '发现新版本',
      message: message,
      buttons: buttons
    }).then((result: promptAction.ShowDialogSuccessResponse) => {
      if (result.index === 0) {
        this.openDownload(url);
      } else if (result.index === 2) {
        this.saveSkipCode(remoteCode);
      }
    }).catch((e: BusinessError) => {
      Log.warn('更新弹窗失败: ' + e.message);
    });
  }

  private openDownload(url: string): void {
    this.ctx.openLink(url).catch((e: BusinessError) => {
      this.toast('打开下载页失败：' + e.message);
    });
  }

  private toast(message: string): void {
    this.ui.getPromptAction().showToast({ message: message, duration: 2500 });
  }

  private async prefStore(): Promise<preferences.Preferences | null> {
    if (this.pref !== null) {
      return this.pref;
    }
    try {
      this.pref = await preferences.getPreferences(this.ctx, PREF_NAME);
      return this.pref;
    } catch (e) {
      return null;
    }
  }

  private async skipCode(): Promise<number> {
    const store: preferences.Preferences | null = await this.prefStore();
    if (store === null) {
      return 0;
    }
    const v: preferences.ValueType = await store.get(KEY_SKIP_CODE, 0);
    return typeof v === 'number' ? v : 0;
  }

  private saveSkipCode(code: number): void {
    this.prefStore().then((store: preferences.Preferences | null) => {
      if (store === null) {
        return;
      }
      store.put(KEY_SKIP_CODE, code).then(() => store.flush()).catch(() => {
      });
    });
  }

  private async httpGet(url: string): Promise<string> {
    const client: http.HttpRequest = http.createHttp();
    try {
      const resp: http.HttpResponse = await client.request(url, {
        method: http.RequestMethod.GET,
        header: { 'Accept': 'application/json, */*' },
        connectTimeout: 8000,
        readTimeout: 15000,
        expectDataType: http.HttpDataType.STRING
      });
      if (resp.responseCode < 200 || resp.responseCode >= 300) {
        throw new Error('HTTP ' + resp.responseCode);
      }
      return resp.result as string;
    } finally {
      client.destroy();
    }
  }

  private humanSize(n: number): string {
    if (n <= 0) {
      return '未知大小';
    }
    if (n < 1024 * 1024) {
      return Math.floor(n / 1024) + ' KB';
    }
    return (n / 1024 / 1024).toFixed(1) + ' MB';
  }
}

/**
 * 暴露给网页的 window.chatUpdate。
 *
 * 前端设置页会读 window.chatUpdate 判断「是否有原生更新入口」，
 * 安卓版通过 addJavascriptInterface 注册同名对象，鸿蒙这里用 javaScriptProxy。
 * 方法必须同步、参数与返回值只能是基础类型或 JSON 可序列化对象。
 *
 * updater 由页面在 aboutToAppear 里注入：javaScriptProxy 的对象要在
 * build 之前就存在，但 Updater 需要先拿到 UIAbilityContext。
 */
export class UpdateBridge {
  private updater: Updater | null = null;

  attach(updater: Updater): void {
    this.updater = updater;
  }

  available(): boolean {
    return true;
  }

  versionName(): string {
    return AppInfo.versionName();
  }

  check(): void {
    if (this.updater !== null) {
      this.updater.checkManually();
    }
  }
}

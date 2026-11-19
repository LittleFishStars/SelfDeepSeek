import { bundleManager } from '@kit.AbilityKit';

/**
 * 本应用的版本信息（来自 app.json5）。
 *
 * 更新检查只比 versionCode：versionName 是给人看的，可能回退或改格式。
 */
export class AppInfo {
  /** 版本号；读不到时返回 0（更新检查会因此保守地认为「有新版」而不是崩溃） */
  static versionCode(): number {
    try {
      const info = bundleManager.getBundleInfoForSelfSync(bundleManager.BundleFlag.GET_BUNDLE_INFO_DEFAULT);
      return info.versionCode;
    } catch (e) {
      return 0;
    }
  }

  /** 版本名；读不到时返回「未知」 */
  static versionName(): string {
    try {
      const info = bundleManager.getBundleInfoForSelfSync(bundleManager.BundleFlag.GET_BUNDLE_INFO_DEFAULT);
      return info.versionName;
    } catch (e) {
      return '未知';
    }
  }
}

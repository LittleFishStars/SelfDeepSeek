import { AbilityConstant, UIAbility, Want } from '@kit.AbilityKit';
import { window } from '@kit.ArkUI';
import { BusinessError } from '@kit.BasicServicesKit';
import { Log } from '../common/Logger';

/**
 * 应用入口。
 *
 * 只做三件事：建窗口、把状态栏刷成浅色（网页是白/深自适应，系统默认深色栏
 * 在浅色主题下会突兀）、加载首页。
 */
export default class EntryAbility extends UIAbility {
  onCreate(want: Want, launchParam: AbilityConstant.LaunchParam): void {
    Log.info('onCreate');
  }

  onWindowStageCreate(windowStage: window.WindowStage): void {
    windowStage.loadContent('pages/Index', (err: BusinessError) => {
      if (err.code) {
        Log.error('loadContent 失败: ' + JSON.stringify(err));
        return;
      }
      this.applySystemBar(windowStage);
    });
  }

  /**
   * 状态栏跟随网页主色（浅色底 + 深色图标）。
   * 必须在 loadContent 之后调：窗口还没创建时设置会被忽略。
   */
  private applySystemBar(windowStage: window.WindowStage): void {
    try {
      const mainWindow: window.Window = windowStage.getMainWindowSync();
      mainWindow.setWindowSystemBarProperties({
        statusBarColor: '#FFFFFF',
        statusBarContentColor: '#000000',
        navigationBarColor: '#FFFFFF',
        navigationBarContentColor: '#000000'
      }).catch((e: BusinessError) => {
        Log.warn('状态栏设置失败: ' + e.message);
      });
    } catch (e) {
      Log.warn('取主窗口失败: ' + JSON.stringify(e));
    }
  }
}

import { hilog } from '@kit.PerformanceAnalysisKit';

/** 统一日志出口：hdc hilog -T chat-demo 就能看到全部输出 */
const DOMAIN: number = 0x1010;
const TAG: string = 'chat-demo';

export class Log {
  static info(msg: string): void {
    hilog.info(DOMAIN, TAG, '%{public}s', msg);
  }

  static warn(msg: string): void {
    hilog.warn(DOMAIN, TAG, '%{public}s', msg);
  }

  static error(msg: string): void {
    hilog.error(DOMAIN, TAG, '%{public}s', msg);
  }
}

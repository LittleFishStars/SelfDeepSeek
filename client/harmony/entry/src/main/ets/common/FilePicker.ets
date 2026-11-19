import { common } from '@kit.AbilityKit';
import { photoAccessHelper } from '@kit.MediaLibraryKit';
import { picker } from '@kit.CoreFileKit';
import { camera, cameraPicker } from '@kit.CameraKit';
import { Log } from './Logger';

/**
 * 网页里 <input type="file"> 的落地实现。
 *
 * ArkWeb 不会自己弹选择器：不实现 onShowFileSelector 的话，聊天页的
 * 「+ 图片 / 文件 / 拍照」点了完全没反应（安卓壳里对应的是
 * WebChromeClient.onShowFileChooser）。
 *
 * 三类入口分开走：
 *   · capture（网页传了 capture=environment）→ 系统相机
 *   · accept 含 image/video → 相册安全选择器（无需读存储权限）
 *   · 其余 → 文档选择器
 */
/** ArkWeb 会把 image/* 展开成这些扩展名；video/* 同理，按常见集合兜住 */
const IMAGE_EXTS: Set<string> = new Set<string>([
  '.jpg', '.jpeg', '.jpe', '.jfif', '.pjpeg', '.pjp', '.png', '.apng', '.gif',
  '.webp', '.avif', '.bmp', '.ico', '.svg', '.svgz', '.xbm', '.tif', '.tiff',
  '.heic', '.heif', '.hevc', '.dng', '.raw', '.cr2', '.nef', '.arw', '.psd', '.jxl'
]);

const VIDEO_EXTS: Set<string> = new Set<string>([
  '.mp4', '.m4v', '.mov', '.qt', '.avi', '.mkv', '.webm', '.flv', '.f4v',
  '.3gp', '.3g2', '.mpg', '.mpeg', '.mpe', '.ts', '.m2ts', '.mts', '.ogv',
  '.rmvb', '.wmv', '.asf'
]);

/** 超过这么多项就认为不是「只要媒体」：聊天「文件」入口的白名单有几百项 */
const MEDIA_ACCEPT_LIMIT: number = 30;

export class FilePicker {
  /**
   * 按网页给的 accept 弹对应的选择器，返回选中的文件 URI 列表。
   * 用户取消时返回空数组（不能抛错，否则网页那边会一直转圈）。
   */
  static async pick(ctx: common.UIAbilityContext, accept: string[],
    capture: boolean, multiple: boolean): Promise<string[]> {
    if (capture) {
      return await FilePicker.takePhoto(ctx);
    }
    if (FilePicker.looksLikeMedia(accept)) {
      return await FilePicker.pickPhoto(ctx, multiple);
    }
    return await FilePicker.pickDocument(ctx, accept, multiple);
  }

  /** 拍照：系统相机 picker，返回拍好的图片 URI */
  private static async takePhoto(ctx: common.UIAbilityContext): Promise<string[]> {
    const profile: cameraPicker.PickerProfile = {
      cameraPosition: camera.CameraPosition.CAMERA_POSITION_BACK
    };
    try {
      const result: cameraPicker.PickerResult =
        await cameraPicker.pick(ctx, [cameraPicker.PickerMediaType.PHOTO], profile);
      if (result.resultUri.length > 0) {
        return [result.resultUri];
      }
      return [];
    } catch (e) {
      Log.warn('拍照失败: ' + JSON.stringify(e));
      return [];
    }
  }

  /** 相册：图片 + 视频 */
  private static async pickPhoto(ctx: common.UIAbilityContext, multiple: boolean): Promise<string[]> {
    try {
      const options: photoAccessHelper.PhotoSelectOptions = new photoAccessHelper.PhotoSelectOptions();
      // 图片与视频都允许：聊天里既发图也发视频
      options.MIMEType = photoAccessHelper.PhotoViewMIMETypes.IMAGE_VIDEO_TYPE;
      options.maxSelectNumber = multiple ? 9 : 1;
      const pickerInstance: photoAccessHelper.PhotoViewPicker = new photoAccessHelper.PhotoViewPicker();
      const result: photoAccessHelper.PhotoSelectResult = await pickerInstance.select(options);
      return result.photoUris;
    } catch (e) {
      Log.warn('选择图片失败: ' + JSON.stringify(e));
      return [];
    }
  }

  /** 文档：按 accept 里的扩展名过滤 */
  private static async pickDocument(ctx: common.UIAbilityContext, accept: string[],
    multiple: boolean): Promise<string[]> {
    try {
      const options: picker.DocumentSelectOptions = new picker.DocumentSelectOptions();
      options.maxSelectNumber = multiple ? 9 : 1;
      const filters: string[] = FilePicker.suffixFilters(accept);
      if (filters.length > 0) {
        options.fileSuffixFilters = filters;
      }
      const pickerInstance: picker.DocumentViewPicker = new picker.DocumentViewPicker(ctx);
      const uris: string[] = await pickerInstance.select(options);
      return uris;
    } catch (e) {
      Log.warn('选择文件失败: ' + JSON.stringify(e));
      return [];
    }
  }

  /**
   * 只有 accept **全部**是图片/视频时才走相册。
   *
   * 两个来源都要认：
   *   · 网页写的 `accept="image/*"` —— ArkWeb 会把它**展开成一串具体扩展名**
   *     （实测：.tif .pjp .pjpeg .jfif .bmp .ico .xbm .svgz .gif .svg .apng
   *      .tiff .png .jpg .webp .hevc .jpe .avif .heif .jpeg .heic），
   *     所以判断必须按扩展名走，不能只看有没有 "image" 字样；
   *   · 聊天「文件」入口给的几百项白名单（含 .png 也含 .go），项数就把它筛掉。
   */
  private static looksLikeMedia(accept: string[]): boolean {
    if (accept.length === 0 || accept.length > MEDIA_ACCEPT_LIMIT) {
      return false;
    }
    for (let i = 0; i < accept.length; i++) {
      const a: string = accept[i].toLowerCase().trim();
      if (a.length === 0 || a === '*/*' || a === '*') {
        return false;
      }
      if (a.startsWith('.')) {
        if (!IMAGE_EXTS.has(a) && !VIDEO_EXTS.has(a)) {
          return false;
        }
      } else if (!a.startsWith('image/') && !a.startsWith('video/')) {
        return false;
      }
    }
    return true;
  }

  /**
   * 把 HTML 的 accept（"image/png,.pdf"）翻成 DocumentViewPicker 的过滤串。
   * 该字段的格式是「名称|.ext1,.ext2」，只认扩展名，MIME 一律丢掉。
   */
  private static suffixFilters(accept: string[]): string[] {
    const exts: string[] = [];
    for (let i = 0; i < accept.length; i++) {
      const a: string = accept[i].trim();
      if (a.startsWith('.') && a.length > 1) {
        exts.push(a);
      }
    }
    // 白名单可能几百项（聊天「文件」入口），过滤串过长没意义，直接放开
    if (exts.length === 0 || exts.length > 30) {
      return [];
    }
    return ['文件|' + exts.join(',')];
  }
}

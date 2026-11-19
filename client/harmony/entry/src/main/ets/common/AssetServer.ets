import { webview } from '@kit.ArkWeb';
import { resourceManager } from '@kit.LocalizationKit';
import { Log } from './Logger';

/**
 * 把 rawfile 里的静态站点通过「拦截 https 请求」喂给 Web 组件。
 *
 * 为什么不直接把 Web 的 src 指向 $rawfile('index.html')：
 * rawfile 的源是 resource://，ArkWeb 对它的 localStorage / IndexedDB 不可靠，
 * 而 chat-demo 把登录令牌、会话、主题全放在 localStorage —— 存不住就等于
 * 每次启动都要重新登录。这里用一个固定的 https 源（APP_ORIGIN），
 * origin 恒定 ⇒ 存储持久；同时不占用任何端口，也不依赖本机 HTTP 服务。
 *
 * 与安卓版的差异：安卓壳起了一个 127.0.0.1:18888 的 MiniServer，
 * 鸿蒙这边用 onInterceptRequest 在 Web 内核层直接应答，省掉一整套
 * TCP 服务（ArkTS 里手写 HTTP 解析既慢又容易在二进制响应上出错）。
 */
export const APP_HOST: string = 'appassets.local';
export const APP_ORIGIN: string = 'https://' + APP_HOST;
export const START_PAGE: string = APP_ORIGIN + '/index.html';

const MIME: Map<string, string> = new Map<string, string>([
  ['html', 'text/html'],
  ['htm', 'text/html'],
  ['js', 'application/javascript'],
  ['mjs', 'application/javascript'],
  ['css', 'text/css'],
  ['json', 'application/json'],
  ['svg', 'image/svg+xml'],
  ['png', 'image/png'],
  ['jpg', 'image/jpeg'],
  ['jpeg', 'image/jpeg'],
  ['webp', 'image/webp'],
  ['gif', 'image/gif'],
  ['ico', 'image/x-icon'],
  ['woff', 'font/woff'],
  ['woff2', 'font/woff2'],
  ['ttf', 'font/ttf'],
  ['otf', 'font/otf'],
  ['wasm', 'application/wasm'],
  ['txt', 'text/plain'],
  ['map', 'application/json'],
  ['webmanifest', 'application/manifest+json']
]);

/** 这些类型的 body 是文本，需要显式声明 UTF-8；图片/字体不能声明编码 */
const TEXTUAL: Set<string> = new Set<string>([
  'text/html', 'application/javascript', 'text/css', 'application/json',
  'image/svg+xml', 'text/plain', 'application/manifest+json'
]);

export class AssetServer {
  private rm: resourceManager.ResourceManager;
  /** rawfile 读一次就够了：首屏要拉几十个 chunk，重复 IO 会拖慢白屏 */
  private cache: Map<string, ArrayBuffer> = new Map<string, ArrayBuffer>();

  constructor(rm: resourceManager.ResourceManager) {
    this.rm = rm;
  }

  /**
   * 处理一次拦截请求。
   *
   * 返回 null 表示「不是我的请求」，交给 Web 内核走真实网络
   * （前端所有接口都直连 https://api.example.com，必须放行）。
   */
  respond(url: string): WebResourceResponse | null {
    if (!url.startsWith(APP_ORIGIN + '/')) {
      return null;
    }
    // 去掉 query 与 hash：静态资源不带查询参数，但缓存破坏串（?v=…）会出现
    let path: string = url.substring(APP_ORIGIN.length);
    const q: number = path.indexOf('?');
    if (q >= 0) {
      path = path.substring(0, q);
    }
    const h: number = path.indexOf('#');
    if (h >= 0) {
      path = path.substring(0, h);
    }
    let asset: string = path.startsWith('/') ? path.substring(1) : path;
    if (asset.length === 0) {
      asset = 'index.html';
    }

    let data: ArrayBuffer | null = this.load(asset);
    if (data === null && asset.indexOf('.') < 0) {
      // 前端是 SPA：没有扩展名的路径（/chat/xxx 之类）回退到首页
      data = this.load('index.html');
      asset = 'index.html';
    }
    if (data === null) {
      // 图标 / manifest 这类可选资源缺了不影响功能（login.html 沿用了网页端的引用），
      // 用 info 级记录，别让它们把真正的 404 刷下去
      if (AssetServer.isOptional(asset)) {
        Log.info('可选资源缺失（忽略）: ' + asset);
      } else {
        Log.warn('静态资源不存在: ' + asset);
      }
      return this.notFound();
    }
    return this.ok(asset, data);
  }

  /** favicon / manifest / 分享图标：缺了只是图标不显示，不算故障 */
  private static isOptional(asset: string): boolean {
    const name: string = asset.toLowerCase();
    return name.startsWith('favicon') || name.startsWith('site.webmanifest') ||
      name.startsWith('apple-touch-icon') || name.startsWith('icon-') ||
      name.startsWith('android-chrome');
  }

  private load(asset: string): ArrayBuffer | null {
    const hit: ArrayBuffer | undefined = this.cache.get(asset);
    if (hit !== undefined) {
      return hit;
    }
    try {
      const u8: Uint8Array = this.rm.getRawFileContentSync(asset);
      // 复制一份再取 buffer：getRawFileContentSync 返回的视图可能带偏移
      const buf: ArrayBuffer = u8.slice().buffer as ArrayBuffer;
      this.cache.set(asset, buf);
      return buf;
    } catch (e) {
      return null;
    }
  }

  private ok(asset: string, data: ArrayBuffer): WebResourceResponse {
    const mime: string = this.mimeOf(asset);
    const resp: WebResourceResponse = new WebResourceResponse();
    resp.setResponseCode(200);
    resp.setResponseMimeType(mime);
    if (TEXTUAL.has(mime)) {
      resp.setResponseEncoding('utf-8');
    }
    const headers: Header[] = [
      { headerKey: 'Cache-Control', headerValue: 'no-store' },
      { headerKey: 'Access-Control-Allow-Origin', headerValue: APP_ORIGIN }
    ];
    resp.setResponseHeader(headers);
    resp.setResponseData(data);
    return resp;
  }

  private notFound(): WebResourceResponse {
    const resp: WebResourceResponse = new WebResourceResponse();
    resp.setResponseCode(404);
    resp.setResponseMimeType('text/plain');
    resp.setResponseEncoding('utf-8');
    resp.setResponseData('not found');
    return resp;
  }

  private mimeOf(asset: string): string {
    const dot: number = asset.lastIndexOf('.');
    if (dot < 0) {
      return 'application/octet-stream';
    }
    const ext: string = asset.substring(dot + 1).toLowerCase();
    const mime: string | undefined = MIME.get(ext);
    return mime !== undefined ? mime : 'application/octet-stream';
  }
}

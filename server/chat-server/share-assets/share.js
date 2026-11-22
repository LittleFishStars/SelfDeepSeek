/* 会话分享页的共用脚本：代码块复制 + 图片灯箱。所有分享页引同一份。 */
(function () {
  'use strict';

  // 代码块右上角加「复制」：移动端没有 hover，点一下按钮即可
  document.querySelectorAll('.body pre').forEach(function (pre) {
    var code = pre.querySelector('code');
    if (!code) return;
    var btn = document.createElement('button');
    btn.className = 'copy-btn';
    btn.type = 'button';
    btn.textContent = '复制';
    btn.addEventListener('click', function () {
      var text = code.innerText;
      var done = function () {
        btn.textContent = '已复制';
        setTimeout(function () { btn.textContent = '复制'; }, 1400);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, fallback);
      } else {
        fallback();
      }
      function fallback() {
        var ta = document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand('copy'); done(); } catch (e) { btn.textContent = '复制失败'; }
        document.body.removeChild(ta);
      }
    });
    pre.appendChild(btn);
  });

  // 图片点击放大
  function openLightbox(src) {
    var box = document.createElement('div');
    box.className = 'lightbox';
    var img = document.createElement('img');
    img.src = src;
    box.appendChild(img);
    box.addEventListener('click', function () { document.body.removeChild(box); });
    document.body.appendChild(box);
  }

  document.querySelectorAll('.body img, .shots img').forEach(function (img) {
    img.addEventListener('click', function () { openLightbox(img.src); });
  });

  document.addEventListener('keydown', function (event) {
    if (event.key !== 'Escape') return;
    var box = document.querySelector('.lightbox');
    if (box) document.body.removeChild(box);
  });

  // ---- 数学公式 ----
  // 服务端已经把公式包成 <span class="math">原文</span>（见 share.go 的 protectMath），
  // 这里用 KaTeX auto-render 排版。
  //
  // KaTeX **自托管**在 /share/assets/katex/<版本>/（只带 woff2 字体，约 596K）：
  // 原先走 jsdelivr CDN，国内访问经常失败，公式就一直停在源码文本看着"没渲染"
  // （2026-11-22 踩过）。自托管后与分享页同源，不依赖第三方。
  // 目录名带版本号，升级 KaTeX 时改 KATEX_BASE 即可，URL 变化天然绕开缓存。
  // 加载失败仍保持源码显示，不影响正文其它部分。
  var KATEX_BASE = '/share/assets/katex/0.16.11';
  var katexState = 0; // 0 未加载 1 加载中 2 可用 3 失败
  var katexQueue = [];
  function ensureKatex(cb) {
    if (katexState === 2) { cb(); return; }
    if (katexState === 3) { return; }
    katexQueue.push(cb);
    if (katexState === 1) { return; }
    katexState = 1;
    var link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = KATEX_BASE + '/katex.min.css';
    document.head.appendChild(link);
    var s1 = document.createElement('script');
    s1.src = KATEX_BASE + '/katex.min.js';
    s1.onload = function () {
      var s2 = document.createElement('script');
      s2.src = KATEX_BASE + '/auto-render.min.js';
      s2.onload = function () {
        katexState = 2;
        katexQueue.splice(0).forEach(function (f) { f(); });
      };
      s2.onerror = function () { katexState = 3; katexQueue = []; };
      document.head.appendChild(s2);
    };
    s1.onerror = function () { katexState = 3; katexQueue = []; };
    document.head.appendChild(s1);
  }

  function renderMath() {
    if (!document.querySelector('.math')) return;
    ensureKatex(function () {
      if (!window.renderMathInElement) return;
      document.querySelectorAll('.body').forEach(function (box) {
        try {
          window.renderMathInElement(box, {
            delimiters: [
              { left: '$$', right: '$$', display: true },
              { left: '$', right: '$', display: false }
            ],
            // 代码块里的 $ 不能当公式（服务端已跳过，这里再兜一层）
            ignoredTags: ['pre', 'code', 'script', 'style', 'textarea'],
            throwOnError: false
          });
        } catch (e) {
          // 单段公式失败不能拖累正文
        }
      });
      document.body.classList.add('math-ready');
    });
  }

  renderMath();
})();

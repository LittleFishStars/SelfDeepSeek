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
})();

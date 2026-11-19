// 首帧引导：与 app.js currentThemeMode() 同口径归一化存储值（非法值归
// auto），跟随档优先读 fnOS 父窗口 body[theme-mode]（同源内嵌时），
// 不可读再退 prefers-color-scheme，避免首帧闪错色。
// theme-color 与 styles.css 的 --bg 同值：手机浏览器地址栏等宿主外壳
// 取该色，跟随明暗同步，深色页面下不再顶条闪白。
(function () {
  function parentThemeMode() {
    try {
      var parent = window.parent;
      if (parent === window || !parent.document || !parent.document.body) return '';
      var mode = (parent.document.body.getAttribute('theme-mode') || '').trim().toLowerCase();
      return mode === 'dark' || mode === 'light' ? mode : '';
    } catch (error) { return ''; /* 父窗口跨域不可读 */ }
  }
  try {
    var mode = localStorage.getItem('tad-theme');
    if (mode !== 'dark' && mode !== 'light' && mode !== 'auto') mode = 'auto';
    var theme = mode;
    if (mode === 'auto') {
      theme = parentThemeMode() || (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    }
    document.documentElement.dataset.theme = theme;
    var themeColor = document.querySelector('meta[name="theme-color"]');
    if (themeColor) themeColor.setAttribute('content', theme === 'dark' ? '#141519' : '#f3f4f6');
  } catch (error) { /* localStorage 不可用时跟随系统 */ }
})();

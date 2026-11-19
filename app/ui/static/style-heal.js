// 样式自愈：升级包后浏览器可能仍持有带长缓存的远古样式表（无 CSS 变量，
// 表现为图表坐标线消失、文字变黑、主题切换无反应）。绕过缓存拉取服务端
// 现行 styles.css，缺少主题变量标记则把引用换成一次性地址强制刷新。
(function () {
  if (!window.fetch) return;
  var link = document.querySelector('link[rel="stylesheet"][href*="styles.css"]');
  if (!link) return;
  fetch(link.href, { cache: 'reload' }).then(function (res) { return res.text(); }).then(function (text) {
    if (text.indexOf('chart-grid-line') !== -1 || link.dataset.healed === '1') return;
    link.dataset.healed = '1';
    link.href = link.href.replace(/([?&])v=[^&]*/, '$1v=' + Date.now());
  }).catch(function () { /* 网络失败时保持现状 */ });
})();

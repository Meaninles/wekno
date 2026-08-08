/**
 * 茅台智汇嵌入式挂件入口。
 * 实现仍由兼容脚本承载；这里复制 data-* 参数并加载它，以免旧集成失效。
 */
(function (global, document) {
  'use strict';

  var entry = document.currentScript;
  if (!entry || !entry.src) return;

  var script = document.createElement('script');
  script.src = new URL('./weknora-widget.js', entry.src).href;
  Array.prototype.forEach.call(entry.attributes, function (attribute) {
    if (attribute.name.indexOf('data-') === 0) {
      script.setAttribute(attribute.name, attribute.value);
    }
  });
  script.onload = function () {
    global.ZhiHui = global.ZhiHui || global.WeKnora;
  };
  entry.parentNode.insertBefore(script, entry.nextSibling);
})(window, document);

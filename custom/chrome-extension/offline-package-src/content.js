(function () {
  'use strict';

  // 防止重复注入 — 用版本号区分
  var KA_VERSION = 11;
  if (window.__kaContentVersion === KA_VERSION) {
    // 已注入过相同版本，但确保消息监听器仍然存在
    if (typeof window.ensureMessageListener === 'function') {
      window.ensureMessageListener();
    }
    return;
  }
  window.__kaContentVersion = KA_VERSION;

  // === 检测扩展上下文是否仍然有效 ===
  function isRuntimeValid() {
    try {
      return !!(chrome && chrome.runtime && chrome.runtime.id);
    } catch (e) {
      return false;
    }
  }

  function showRefreshHint() {
    // 创建一个醒目的提示条，提醒用户刷新页面
    var hint = document.createElement('div');
    hint.className = 'ka-notification ka-notification-error';
    hint.style.cssText = 'cursor:pointer;min-width:320px;text-align:center;';
    hint.innerHTML = '扩展已更新，请<b>刷新页面</b>后重试 <span style="font-size:12px;opacity:0.8;">（点击刷新）</span>';
    hint.addEventListener('click', function () {
      location.reload();
    });
    document.body.appendChild(hint);

    requestAnimationFrame(function () {
      hint.classList.add('ka-notification-show');
    });

    // 8 秒后自动消失
    setTimeout(function () {
      hint.classList.remove('ka-notification-show');
      setTimeout(function () { hint.remove(); }, 300);
    }, 8000);
  }

  // 包装 chrome.runtime.sendMessage，自动检测上下文失效
  function safeSendMessage(message, callback) {
    if (!isRuntimeValid()) {
      showRefreshHint();
      if (typeof callback === 'function') {
        callback(null);
      }
      return false;
    }
    try {
      chrome.runtime.sendMessage(message, function (resp) {
        var err = chrome.runtime.lastError;
        if (err && err.message && err.message.indexOf('Extension context invalidated') !== -1) {
          showRefreshHint();
          if (typeof callback === 'function') callback(null);
          return;
        }
        if (typeof callback === 'function') callback(resp);
      });
      return true;
    } catch (e) {
      if (e.message && e.message.indexOf('Extension context invalidated') !== -1) {
        showRefreshHint();
      } else {
        showNotification('通信失败: ' + e.message, 'error');
      }
      if (typeof callback === 'function') callback(null);
      return false;
    }
  }

  // === 登录 + 知识库状态管理 ===
  var kaUserAuth = null;   // null 表示未登录, { type, name, avatar }
  var kaClipKbId = '';      // 剪藏目标知识库 ID
  var kaClipKbName = '';    // 剪藏目标知识库名称
  var kaSelBubbleEnabled = true; // 选中文字气泡开关，默认开
  var kaDisabledPages = {};      // 当前页面禁用集合 { url: true }
  var kaAllPagesDisabled = false; // 所有页面禁用（当前浏览器会话）

  // 从 background 获取最新状态
  function refreshAuthState(callback) {
    safeSendMessage({ type: 'GET_AUTH' }, function (resp) {
      if (resp && resp.success) {
        kaUserAuth = resp.data || null;
      } else {
        kaUserAuth = null;
      }
      // 同时读取知识库选择
      try {
        chrome.storage.local.get(['clipKbId', 'clipKbName', 'ka_sel_bubble_enabled'], function (data) {
          kaClipKbId = (data && data.clipKbId) || '';
          kaClipKbName = (data && data.clipKbName) || '';
          kaSelBubbleEnabled = data && data.ka_sel_bubble_enabled === false ? false : true;

          // 从 session storage 加载禁用状态
          try {
            chrome.storage.session.get(['ka_disabled_pages', 'ka_all_pages_disabled'], function (sessionData) {
              if (sessionData && sessionData.ka_disabled_pages) {
                kaDisabledPages = sessionData.ka_disabled_pages;
              }
              if (sessionData && sessionData.ka_all_pages_disabled) {
                kaAllPagesDisabled = true;
              }
              if (typeof callback === 'function') callback();
            });
          } catch (e) {
            if (typeof callback === 'function') callback();
          }
        });
      } catch (e) {
        // storage 访问失败（上下文可能失效）
        if (typeof callback === 'function') callback();
      }
    });
  }

  // 检查功能是否可用：已登录 + 已选知识库
  function isFunctionReady() {
    return !!(kaUserAuth && kaClipKbId);
  }

  // 功能不可用时的提示
  function showAuthGuardHint() {
    if (!kaUserAuth) {
      showNotification('请先在扩展中登录账号', 'error');
    } else if (!kaClipKbId) {
      showNotification('请先在扩展中选择知识库', 'error');
    }
  }

  // 初始化时获取状态
  refreshAuthState();

  // === 智能剪藏 ===

  // 辅助：从 <meta> 标签提取内容
  function getMetaContent(name) {
    var el = document.querySelector(
      'meta[name="' + name + '"], meta[property="' + name + '"]'
    );
    return el ? (el.getAttribute('content') || '') : '';
  }

  // 使用 Defuddle 一次性提取内容和元数据
  function extractWithDefuddle() {
    if (typeof Defuddle !== 'undefined') {
      try {
        var DefuddleClass = Defuddle.default || Defuddle;
        var createMarkdown = Defuddle.createMarkdownContent;
        var defuddled = new DefuddleClass(document, { url: location.href }).parse();

        var markdown = '';
        if (defuddled.content && createMarkdown) {
          markdown = createMarkdown(defuddled.content, location.href);
        } else if (defuddled.content) {
          var tmp = document.createElement('div');
          tmp.innerHTML = defuddled.content;
          markdown = tmp.textContent.trim();
        }

        return {
          markdown: markdown,
          title: defuddled.title || document.title || location.hostname,
          author: defuddled.author || '',
          description: defuddled.description || '',
          site: defuddled.site || location.hostname,
          published: defuddled.published || '',
          wordCount: defuddled.wordCount || 0
        };
      } catch (e) {
      }
    }
    // Defuddle 不可用，回退
    return null;
  }

  function smartClip() {
    if (!isFunctionReady()) { showAuthGuardHint(); return; }

    // 优先用 Defuddle 提取
    var extracted = extractWithDefuddle();
    var content, title, meta;

    if (extracted) {
      content = extracted.markdown;
      title = extracted.title;
      meta = {
        url: location.href,
        title: title,
        author: extracted.author,
        description: extracted.description,
        siteName: extracted.site,
        publishedTime: extracted.published
      };
    } else {
      // 降级方案
      content = extractMainContent();
      title = document.title || location.hostname;
      meta = {
        url: location.href,
        title: title,
        author: getMetaContent('author') || getMetaContent('article:author') || '',
        description: getMetaContent('description') || getMetaContent('og:description') || '',
        siteName: getMetaContent('og:site_name') || location.hostname,
        publishedTime: getMetaContent('article:published_time') || ''
      };
    }

    // 构建带元数据的 Markdown
    var header = '';
    if (meta.author) header += '> 作者: ' + meta.author + '\n';
    if (meta.siteName) header += '> 来源: [' + meta.siteName + '](' + location.href + ')\n';
    if (meta.publishedTime) header += '> 发布时间: ' + meta.publishedTime + '\n';
    if (header) header += '\n';

    var fullContent = header + content + '\n\n---\n来源: ' + location.href;
    var defaultTitle = title.replace(/\.+$/, '') + '.md';

    var editorOpts = { defaultTitle: defaultTitle, headerTitle: '智能剪藏' };

    // 弹出编辑器让用户确认和编辑，使用回调保存
    openClipEditor(fullContent, null, function (finalContent) {
      var clipData = {
        type: 'smart-clip',
        content: finalContent,
        title: defaultTitle.replace(/\.md$/, ''),
        meta: meta
      };

      safeSendMessage({
        type: 'SAVE_CLIP',
        payload: clipData
      }, function (resp) {
        if (resp && resp.success) {
          var msg = '智能剪藏成功';
          if (resp.syncedToKb && resp.kbName) {
            msg = '智能剪藏成功，已同步到「' + resp.kbName + '」';
          } else if (resp.syncError) {
            msg = '保存失败，请稍后重试';
            showNotification(msg, 'error');
            safeSendMessage({
              type: 'CLIP_RESULT',
              payload: { title: clipData.title, content: finalContent }
            });
            return;
          }
          showNotification(msg, 'success');
          safeSendMessage({
            type: 'CLIP_RESULT',
            payload: { title: clipData.title, content: finalContent }
          });
        } else {
          showNotification('剪藏失败: ' + ((resp && resp.error) || '未知错误'), 'error');
        }
      });
    }, editorOpts);
  }

  function extractMainContent() {
    // 使用 Defuddle 进行智能内容提取和 Markdown 转换
    // Defuddle 是 Obsidian Clipper 使用的核心提取库，包含：
    // - 智能内容评分系统（基于文本密度、段落比例、链接密度等）
    // - 200+ 个精确选择器去除广告/导航/侧边栏等噪音
    // - 高质量 Turndown 转换（表格、代码块、图片 srcset、脚注等 20+ 自定义规则）
    if (typeof Defuddle !== 'undefined') {
      try {
        var DefuddleClass = Defuddle.default || Defuddle;
        var createMarkdown = Defuddle.createMarkdownContent;

        // 用 Defuddle 解析页面，提取主内容 HTML
        var defuddled = new DefuddleClass(document, { url: location.href }).parse();
        var contentHtml = defuddled.content || '';

        if (contentHtml && createMarkdown) {
          // 用 Defuddle 内置的 createMarkdownContent 转换
          // 包含完整的 Turndown 自定义规则（表格、列表、代码块、图片、脚注等）
          var markdown = createMarkdown(contentHtml, location.href);
          return markdown;
        }

        // createMarkdownContent 不可用时，退回纯文本
        if (contentHtml) {
          var tmp = document.createElement('div');
          tmp.innerHTML = contentHtml;
          return tmp.textContent.trim();
        }
      } catch (e) {
      }
    }

    // Defuddle 不可用时的降级方案
    var selectors = [
      'article', '[role="article"]', 'main', '[role="main"]',
      '.post-content', '.article-content', '.entry-content',
      '.post-body', '.article-body',
      '.content', '#content'
    ];
    var contentEl = null;
    for (var i = 0; i < selectors.length; i++) {
      var el = document.querySelector(selectors[i]);
      if (el && el.textContent.trim().length > 200) {
        contentEl = el;
        break;
      }
    }
    if (!contentEl) contentEl = document.body;

    var clone = contentEl.cloneNode(true);
    clone.querySelectorAll('script, style, noscript, nav, header, footer, .ads, .sidebar, .comments, [role="navigation"], [role="banner"]').forEach(function (r) { r.remove(); });

    return clone.textContent.trim();
  }

  // === 选择剪藏 — 截屏式区域框选 ===
  var clipState = {
    active: false,
    overlay: null,
    startX: 0,
    startY: 0,
    rect: null,
    toolbar: null,
    dimTop: null,
    dimBottom: null,
    dimLeft: null,
    dimRight: null
  };

  function startSelectClip() {
    if (clipState.active) return;
    clipState.active = true;

    // 创建全屏遮罩
    var overlay = document.createElement('div');
    overlay.id = 'ka-clip-overlay';
    overlay.innerHTML =
      '<div class="ka-clip-dim ka-clip-dim-top"></div>' +
      '<div class="ka-clip-dim ka-clip-dim-bottom"></div>' +
      '<div class="ka-clip-dim ka-clip-dim-left"></div>' +
      '<div class="ka-clip-dim ka-clip-dim-right"></div>' +
      '<div class="ka-clip-selection"></div>' +
      '<div class="ka-clip-tip">拖拽鼠标框选要截取的区域，松开后确认</div>';
    document.body.appendChild(overlay);
    clipState.overlay = overlay;

    clipState.dimTop = overlay.querySelector('.ka-clip-dim-top');
    clipState.dimBottom = overlay.querySelector('.ka-clip-dim-bottom');
    clipState.dimLeft = overlay.querySelector('.ka-clip-dim-left');
    clipState.dimRight = overlay.querySelector('.ka-clip-dim-right');
    clipState.rect = overlay.querySelector('.ka-clip-selection');

    // 初始暗幕覆盖全屏
    var w = window.innerWidth;
    var h = window.innerHeight;
    setDims(0, 0, w, h);
    clipState.rect.style.display = 'none';

    overlay.addEventListener('mousedown', onClipMouseDown);
    document.addEventListener('keydown', onClipKeyDown);
  }

  function setDims(x, y, x2, y2) {
    var w = window.innerWidth;
    var h = window.innerHeight;
    // 确保有效范围
    var left = Math.min(x, x2);
    var top = Math.min(y, y2);
    var right = Math.max(x, x2);
    var bottom = Math.max(y, y2);

    // 如果选区大小为0，全屏暗幕
    if (right - left < 2 || bottom - top < 2) {
      clipState.dimTop.style.cssText = 'top:0;left:0;width:' + w + 'px;height:' + h + 'px;';
      clipState.dimBottom.style.cssText = 'display:none;';
      clipState.dimLeft.style.cssText = 'display:none;';
      clipState.dimRight.style.cssText = 'display:none;';
      return;
    }

    // 上方暗幕
    clipState.dimTop.style.cssText = 'top:0;left:0;width:' + w + 'px;height:' + top + 'px;';
    // 下方暗幕
    clipState.dimBottom.style.cssText = 'top:' + bottom + 'px;left:0;width:' + w + 'px;height:' + (h - bottom) + 'px;';
    // 左侧暗幕
    clipState.dimLeft.style.cssText = 'top:' + top + 'px;left:0;width:' + left + 'px;height:' + (bottom - top) + 'px;';
    // 右侧暗幕
    clipState.dimRight.style.cssText = 'top:' + top + 'px;left:' + right + 'px;width:' + (w - right) + 'px;height:' + (bottom - top) + 'px;';
  }

  function onClipMouseDown(e) {
    if (e.button !== 0) return;
    // 如果点击在工具栏或 handle 上，不要重新框选
    if (e.target.closest && (e.target.closest('.ka-clip-toolbar') || e.target.closest('.ka-clip-handle'))) return;
    e.preventDefault();
    e.stopPropagation();

    // 移除之前的工具栏和 handles
    if (clipState.toolbar) {
      clipState.toolbar.remove();
      clipState.toolbar = null;
    }
    var oldHandles = clipState.overlay.querySelectorAll('.ka-clip-handle');
    oldHandles.forEach(function (h) { h.remove(); });

    clipState.startX = e.clientX;
    clipState.startY = e.clientY;
    clipState.rect.style.display = 'block';
    clipState.rect.style.left = e.clientX + 'px';
    clipState.rect.style.top = e.clientY + 'px';
    clipState.rect.style.width = '0';
    clipState.rect.style.height = '0';

    // 隐藏提示
    var tip = clipState.overlay.querySelector('.ka-clip-tip');
    if (tip) tip.style.display = 'none';

    document.addEventListener('mousemove', onClipMouseMove);
    document.addEventListener('mouseup', onClipMouseUp);
  }

  function onClipMouseMove(e) {
    e.preventDefault();
    var x = Math.min(e.clientX, clipState.startX);
    var y = Math.min(e.clientY, clipState.startY);
    var w = Math.abs(e.clientX - clipState.startX);
    var h = Math.abs(e.clientY - clipState.startY);

    clipState.rect.style.left = x + 'px';
    clipState.rect.style.top = y + 'px';
    clipState.rect.style.width = w + 'px';
    clipState.rect.style.height = h + 'px';

    setDims(x, y, x + w, y + h);
  }

  function onClipMouseUp(e) {
    document.removeEventListener('mousemove', onClipMouseMove);
    document.removeEventListener('mouseup', onClipMouseUp);

    var x = Math.min(e.clientX, clipState.startX);
    var y = Math.min(e.clientY, clipState.startY);
    var w = Math.abs(e.clientX - clipState.startX);
    var h = Math.abs(e.clientY - clipState.startY);

    if (w < 10 || h < 10) {
      // 太小，忽略
      clipState.rect.style.display = 'none';
      setDims(0, 0, window.innerWidth, window.innerHeight);
      var tip = clipState.overlay.querySelector('.ka-clip-tip');
      if (tip) tip.style.display = 'block';
      return;
    }

    // 允许拖拽调整大小 — 添加 resize handles
    addResizeHandles(x, y, w, h);

    // 显示确认工具栏
    showClipToolbar(x, y, w, h);
  }

  function addResizeHandles(x, y, w, h) {
    // 给选区添加 8 个拖拽点
    var handles = ['nw', 'n', 'ne', 'e', 'se', 's', 'sw', 'w'];
    handles.forEach(function (pos) {
      var handle = document.createElement('div');
      handle.className = 'ka-clip-handle ka-clip-handle-' + pos;
      handle.dataset.pos = pos;
      clipState.rect.appendChild(handle);

      handle.addEventListener('mousedown', function (e) {
        e.preventDefault();
        e.stopPropagation();
        startResize(pos, e);
      });
    });
  }

  function startResize(pos, e) {
    var startX = e.clientX;
    var startY = e.clientY;
    var origLeft = parseInt(clipState.rect.style.left);
    var origTop = parseInt(clipState.rect.style.top);
    var origW = parseInt(clipState.rect.style.width);
    var origH = parseInt(clipState.rect.style.height);

    function onMove(ev) {
      ev.preventDefault();
      var dx = ev.clientX - startX;
      var dy = ev.clientY - startY;
      var newLeft = origLeft, newTop = origTop, newW = origW, newH = origH;

      if (pos.indexOf('e') !== -1) newW = Math.max(20, origW + dx);
      if (pos.indexOf('w') !== -1) { newW = Math.max(20, origW - dx); newLeft = origLeft + dx; }
      if (pos.indexOf('s') !== -1) newH = Math.max(20, origH + dy);
      if (pos.indexOf('n') !== -1) { newH = Math.max(20, origH - dy); newTop = origTop + dy; }

      clipState.rect.style.left = newLeft + 'px';
      clipState.rect.style.top = newTop + 'px';
      clipState.rect.style.width = newW + 'px';
      clipState.rect.style.height = newH + 'px';

      setDims(newLeft, newTop, newLeft + newW, newTop + newH);

      // 更新工具栏位置
      if (clipState.toolbar) {
        clipState.toolbar.style.left = newLeft + 'px';
        clipState.toolbar.style.top = (newTop + newH + 8) + 'px';
      }
    }

    function onUp() {
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
    }

    document.removeEventListener('mousemove', onClipMouseMove);
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  }

  function showClipToolbar(x, y, w, h) {
    if (clipState.toolbar) clipState.toolbar.remove();

    var bar = document.createElement('div');
    bar.className = 'ka-clip-toolbar';
    bar.style.left = x + 'px';
    bar.style.top = (y + h + 8) + 'px';

    // 选区尺寸提示
    bar.innerHTML =
      '<span class="ka-clip-size">' + Math.round(w) + ' × ' + Math.round(h) + '</span>' +
      '<button class="ka-clip-btn ka-clip-btn-cancel">取消 <kbd>Esc</kbd></button>' +
      '<button class="ka-clip-btn ka-clip-btn-confirm">确认截取 <kbd>↵</kbd></button>';

    clipState.overlay.appendChild(bar);
    clipState.toolbar = bar;

    bar.querySelector('.ka-clip-btn-cancel').addEventListener('click', cancelClip);
    bar.querySelector('.ka-clip-btn-confirm').addEventListener('click', confirmClip);
  }

  function onClipKeyDown(e) {
    if (!clipState.active) return;
    if (e.key === 'Escape') {
      cancelClip();
    } else if (e.key === 'Enter') {
      confirmClip();
    }
  }

  function confirmClip() {
    if (!clipState.rect) return;

    var rectLeft = parseInt(clipState.rect.style.left);
    var rectTop = parseInt(clipState.rect.style.top);
    var rectW = parseInt(clipState.rect.style.width);
    var rectH = parseInt(clipState.rect.style.height);

    if (isNaN(rectW) || isNaN(rectH) || rectW < 10 || rectH < 10) {
      showNotification('选区太小，请重新框选', 'error');
      return;
    }

    // 1. 先提取选区内的文字内容
    var content = extractTextInRect(
      rectLeft + window.scrollX,
      rectTop + window.scrollY,
      rectW,
      rectH
    );

    // 2. 保存裁剪参数（异步回调中使用）
    var cropLeft = rectLeft;
    var cropTop = rectTop;
    var cropW = rectW;
    var cropH = rectH;

    // 3. 逐个隐藏截图工具元素，确保截图干净
    //    先隐藏选区框（绿色边框）、resize handles（小方块）、工具栏
    if (clipState.rect) clipState.rect.style.display = 'none';
    if (clipState.toolbar) clipState.toolbar.style.display = 'none';
    // 隐藏所有 handles（以防有残留的）
    if (clipState.overlay) {
      var handles = clipState.overlay.querySelectorAll('.ka-clip-handle');
      handles.forEach(function (h) { h.style.display = 'none'; });
      // 隐藏提示文字
      var tip = clipState.overlay.querySelector('.ka-clip-tip');
      if (tip) tip.style.display = 'none';
    }
    // 最后隐藏整个 overlay（包括暗幕遮罩）
    if (clipState.overlay) clipState.overlay.style.display = 'none';

    // 4. 双重 requestAnimationFrame 确保浏览器完成渲染后再截图
    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        safeSendMessage({ type: 'CAPTURE_SCREENSHOT' }, function (resp) {
          // 立即清除遮罩
          cancelClip();

          if (!resp || !resp.success || !resp.dataUrl) {
            // 截图失败，回退到纯文字（弹出编辑器让用户确认）
            openClipEditor(content, null);
            return;
          }

          // 5. 用 canvas 裁剪选区部分
          var img = new Image();
          img.onload = function () {
            var dpr = window.devicePixelRatio || 1;
            var canvas = document.createElement('canvas');
            canvas.width = cropW * dpr;
            canvas.height = cropH * dpr;
            var ctx = canvas.getContext('2d');
            ctx.drawImage(img,
              cropLeft * dpr, cropTop * dpr, cropW * dpr, cropH * dpr,
              0, 0, cropW * dpr, cropH * dpr
            );
            var croppedDataUrl = canvas.toDataURL('image/jpeg', 0.8);
            openClipEditor(content, croppedDataUrl);
          };
          img.onerror = function () {
            openClipEditor(content, null);
          };
          img.src = resp.dataUrl;
        });
      });
    });
  }

  // 保存选择剪藏数据（独立函数，不依赖 clipState）
  function doSaveSelectClip(textContent, screenshotDataUrl) {
    // 只要有截图或有文字，就保存
    var hasText = textContent && textContent.trim().length > 0;
    var hasScreenshot = !!screenshotDataUrl;

    if (!hasText && !hasScreenshot) {
      showNotification('选区内没有可保存的内容', 'error');
      return;
    }

    var title = document.title || location.hostname;
    var clipData = {
      type: 'select-clip',
      content: hasText ? textContent.trim() : '（截图内容）',
      title: title,
      meta: { url: location.href, title: title, timestamp: Date.now() }
    };

    if (hasScreenshot) {
      clipData.screenshot = screenshotDataUrl;
    }

    safeSendMessage({
      type: 'SAVE_CLIP',
      payload: clipData
    }, function (resp) {
      if (resp && resp.success) {
        var msg = '内容已截取保存';
        if (resp.syncedToKb && resp.kbName) {
          msg = '内容已截取保存，并同步到「' + resp.kbName + '」';
        } else if (resp.syncError) {
          msg = '保存失败，请稍后重试';
          showNotification(msg, 'error');
          safeSendMessage({
            type: 'CLIP_RESULT',
            payload: { title: clipData.title, content: clipData.content }
          });
          return;
        }
        showNotification(msg, 'success');
        safeSendMessage({
          type: 'CLIP_RESULT',
          payload: { title: clipData.title, content: clipData.content }
        });
      } else {
        showNotification('保存失败: ' + ((resp && resp.error) || '未知错误'), 'error');
      }
    });
  }

  // === 快速笔记浮动面板（Control+Option 快捷键触发） ===
  var quickNoteOverlay = null;

  function openQuickNotePanel() {
    if (quickNoteOverlay) return; // 已打开

    var overlay = document.createElement('div');
    overlay.className = 'ka-editor-overlay';
    overlay.style.cssText = 'position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,0.35);z-index:2147483646;display:flex;align-items:flex-start;justify-content:center;padding-top:80px;backdrop-filter:blur(2px);-webkit-backdrop-filter:blur(2px);animation:ka-qn-fadein 0.2s ease;';

    overlay.innerHTML =
      '<div style="background:#fff;border-radius:16px;box-shadow:0 12px 48px rgba(0,0,0,0.18),0 0 1px rgba(0,0,0,0.08);width:480px;max-width:calc(100vw - 32px);display:flex;flex-direction:column;max-height:calc(100vh - 120px);overflow:hidden;animation:ka-qn-slidein 0.25s cubic-bezier(0.4,0,0.2,1);" class="ka-qn-panel">' +
        /* 头部 */
        '<div style="display:flex;align-items:center;justify-content:space-between;padding:14px 18px 10px;border-bottom:1px solid #f0f0f0;">' +
          '<div style="display:flex;align-items:center;gap:8px;">' +
            '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="#07C160" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="16" y1="13" x2="8" y2="13"/><line x1="16" y1="17" x2="8" y2="17"/></svg>' +
            '<span style="font-size:14px;font-weight:600;color:#333;">快速笔记</span>' +
            '<span style="font-size:10px;color:#bbb;background:#f5f5f5;padding:2px 6px;border-radius:4px;">Markdown</span>' +
          '</div>' +
          '<button class="ka-qn-close" style="width:28px;height:28px;border:none;background:#f5f5f5;border-radius:50%;cursor:pointer;display:flex;align-items:center;justify-content:center;color:#999;transition:all 0.15s;" title="关闭 (Esc)">' +
            '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>' +
          '</button>' +
        '</div>' +
        /* 编辑区 */
        '<div style="flex:1;display:flex;flex-direction:column;min-height:0;">' +
          '<textarea class="ka-qn-input" style="flex:1;border:none;padding:14px 18px;font-size:14px;font-family:\'SF Mono\',\'Fira Code\',\'Menlo\',\'Consolas\',monospace;line-height:1.7;resize:none;outline:none;color:#333;min-height:180px;background:transparent;" placeholder="# 标题\n\n在这里快速记录…"></textarea>' +
          '<div class="ka-qn-preview" style="display:none;flex:1;padding:14px 18px;overflow-y:auto;font-size:14px;line-height:1.7;color:#333;"></div>' +
        '</div>' +
        /* 底部工具栏 */
        '<div style="display:flex;align-items:center;justify-content:space-between;padding:8px 14px;border-top:1px solid #f0f0f0;background:#fafafa;border-radius:0 0 16px 16px;">' +
          '<div style="display:flex;align-items:center;gap:2px;">' +
            '<button class="ka-qn-tool" data-md="bold" title="加粗" style="width:30px;height:30px;border:none;background:transparent;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center;color:#777;transition:all 0.15s;">' +
              '<svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor"><path d="M15.6 10.79c.97-.67 1.65-1.77 1.65-2.79 0-2.26-1.75-4-4-4H7v14h7.04c2.09 0 3.71-1.7 3.71-3.79 0-1.52-.86-2.82-2.15-3.42zM10 6.5h3c.83 0 1.5.67 1.5 1.5s-.67 1.5-1.5 1.5h-3v-3zm3.5 9H10v-3h3.5c.83 0 1.5.67 1.5 1.5s-.67 1.5-1.5 1.5z"/></svg>' +
            '</button>' +
            '<button class="ka-qn-tool" data-md="heading" title="标题" style="width:30px;height:30px;border:none;background:transparent;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center;color:#777;transition:all 0.15s;">' +
              '<svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor"><path d="M5 4v3h5.5v12h3V7H19V4H5z"/></svg>' +
            '</button>' +
            '<button class="ka-qn-tool" data-md="ul" title="列表" style="width:30px;height:30px;border:none;background:transparent;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center;color:#777;transition:all 0.15s;">' +
              '<svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor"><path d="M4 10.5c-.83 0-1.5.67-1.5 1.5s.67 1.5 1.5 1.5 1.5-.67 1.5-1.5-.67-1.5-1.5-1.5zm0-6c-.83 0-1.5.67-1.5 1.5S3.17 7.5 4 7.5 5.5 6.83 5.5 6 4.83 4.5 4 4.5zm0 12c-.83 0-1.5.68-1.5 1.5s.68 1.5 1.5 1.5 1.5-.68 1.5-1.5-.67-1.5-1.5-1.5zM7 19h14v-2H7v2zm0-6h14v-2H7v2zm0-8v2h14V5H7z"/></svg>' +
            '</button>' +
            '<button class="ka-qn-tool" data-md="link" title="链接" style="width:30px;height:30px;border:none;background:transparent;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center;color:#777;transition:all 0.15s;">' +
              '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>' +
            '</button>' +
            '<div style="width:1px;height:14px;background:#e0e0e0;margin:0 4px;"></div>' +
            '<button class="ka-qn-tool ka-qn-preview-btn" data-md="preview" title="预览" style="width:30px;height:30px;border:none;background:transparent;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center;color:#777;transition:all 0.15s;">' +
              '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>' +
            '</button>' +
          '</div>' +
          '<div style="display:flex;align-items:center;gap:8px;">' +
            '<span class="ka-qn-hint" style="font-size:11px;color:#bbb;">⌃⌥N</span>' +
            '<button class="ka-qn-save" style="height:32px;padding:0 18px;border:none;border-radius:16px;background:#07C160;color:#fff;font-size:13px;font-weight:500;cursor:pointer;transition:all 0.2s;display:flex;align-items:center;gap:5px;">' +
              '<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M12 4l-1.41 1.41L16.17 11H4v2h12.17l-5.58 5.59L12 20l8-8-8-8z"/></svg>' +
              '保存' +
            '</button>' +
          '</div>' +
        '</div>' +
      '</div>';

    // 注入动画样式
    var styleId = 'ka-qn-anim-style';
    if (!document.getElementById(styleId)) {
      var style = document.createElement('style');
      style.id = styleId;
      style.textContent =
        '@keyframes ka-qn-fadein { from { opacity: 0; } to { opacity: 1; } }' +
        '@keyframes ka-qn-slidein { from { opacity: 0; transform: translateY(-20px) scale(0.97); } to { opacity: 1; transform: translateY(0) scale(1); } }' +
        '.ka-qn-close:hover { background: #eee !important; color: #333 !important; }' +
        '.ka-qn-tool:hover { background: rgba(7,193,96,0.08) !important; color: #07C160 !important; }' +
        '.ka-qn-save:hover { background: #06ad54 !important; box-shadow: 0 2px 8px rgba(7,193,96,0.3); }';
      document.head.appendChild(style);
    }

    document.body.appendChild(overlay);
    quickNoteOverlay = overlay;

    var textarea = overlay.querySelector('.ka-qn-input');
    var previewDiv = overlay.querySelector('.ka-qn-preview');
    var isPreview = false;

    // 聚焦输入框
    setTimeout(function () { textarea.focus(); }, 100);

    // 工具栏按钮
    overlay.querySelectorAll('.ka-qn-tool[data-md]').forEach(function (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        var action = btn.getAttribute('data-md');
        switch (action) {
          case 'bold': editorInsertMd(textarea, '**', '**', '粗体文本'); break;
          case 'heading': editorInsertLinePrefix(textarea, '## '); break;
          case 'ul': editorInsertLinePrefix(textarea, '- '); break;
          case 'link': editorInsertMd(textarea, '[', '](url)', '链接文本'); break;
          case 'preview':
            isPreview = !isPreview;
            if (isPreview) {
              previewDiv.innerHTML = renderMarkdown(textarea.value);
              previewDiv.style.display = 'block';
              textarea.style.display = 'none';
              btn.style.color = '#07C160';
              btn.style.background = 'rgba(7,193,96,0.1)';
            } else {
              previewDiv.style.display = 'none';
              textarea.style.display = 'block';
              textarea.focus();
              btn.style.color = '#777';
              btn.style.background = 'transparent';
            }
            break;
        }
      });
    });

    // 保存按钮
    overlay.querySelector('.ka-qn-save').addEventListener('click', function () {
      var text = textarea.value.trim();
      if (!text) { showNotification('请先输入内容', 'error'); return; }
      var firstLine = text.split('\n')[0].replace(/^#+\s*/, '').trim() || '速记';
      safeSendMessage({
        type: 'SAVE_CLIP',
        payload: { type: 'markdown', content: text, title: firstLine }
      }, function (resp) {
        if (resp && resp.success) {
          var msg = '笔记已保存';
          if (resp.syncedToKb && resp.kbName) {
            msg = '笔记已保存，已同步到「' + resp.kbName + '」';
          } else if (resp.syncError) {
            showNotification('保存失败，请稍后重试', 'error');
            closeQuickNotePanel();
            return;
          }
          showNotification(msg, 'success');
          closeQuickNotePanel();
        } else {
          showNotification('保存失败: ' + ((resp && resp.error) || '未知错误'), 'error');
        }
      });
    });

    // 关闭按钮
    overlay.querySelector('.ka-qn-close').addEventListener('click', closeQuickNotePanel);

    // 点击遮罩关闭
    overlay.addEventListener('click', function (e) { if (e.target === overlay) closeQuickNotePanel(); });

    // Esc 关闭
    function onEsc(e) {
      if (e.key === 'Escape') { closeQuickNotePanel(); document.removeEventListener('keydown', onEsc); }
    }
    document.addEventListener('keydown', onEsc);

    // Cmd+Enter / Ctrl+Enter 快速保存
    textarea.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        overlay.querySelector('.ka-qn-save').click();
      }
    });
  }

  function closeQuickNotePanel() {
    if (quickNoteOverlay) {
      quickNoteOverlay.remove();
      quickNoteOverlay = null;
    }
  }

  // === 截图/剪藏编辑弹窗 ===
  var clipEditorOverlay = null;

  function openClipEditor(textContent, screenshotDataUrl, onSaveCallback, editorOptions) {
    closeClipEditor();

    var opts = editorOptions || {};
    var pageTitle = document.title || location.hostname;
    var pageUrl = location.href;
    var now = new Date();
    var timeStr = now.getFullYear() + '-' +
      String(now.getMonth() + 1).padStart(2, '0') + '-' +
      String(now.getDate()).padStart(2, '0') + ' ' +
      String(now.getHours()).padStart(2, '0') + ':' +
      String(now.getMinutes()).padStart(2, '0') + ':' +
      String(now.getSeconds()).padStart(2, '0');

    var hasScreenshot = !!screenshotDataUrl;
    var hasCallback = typeof onSaveCallback === 'function';
    var defaultTitle = opts.defaultTitle || (pageTitle.replace(/\.+$/, '') + '.md');
    var headerTitle = opts.headerTitle || (hasCallback ? '保存笔记' : '保存剪藏');

    var overlay = document.createElement('div');
    overlay.className = 'ka-editor-overlay';

    // 截图缩略图区域
    var screenshotHtml = '';
    if (hasScreenshot) {
      screenshotHtml =
        '<div class="ka-clip-ed-screenshot">' +
          '<img src="' + screenshotDataUrl + '" alt="截图预览">' +
        '</div>';
    }

    overlay.innerHTML =
      '<div class="ka-clip-ed-panel">' +
        // 头部
        '<div class="ka-clip-ed-header">' +
          '<div class="ka-clip-ed-header-left">' +
            '<svg class="ka-clip-ed-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>' +
            '<span class="ka-clip-ed-header-title">' + headerTitle + '</span>' +
          '</div>' +
          '<button class="ka-editor-close" title="关闭">' +
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>' +
          '</button>' +
        '</div>' +

        // 左右分栏主体
        '<div class="ka-clip-ed-body">' +

          // 左侧：标题、元信息、截图
          '<div class="ka-clip-ed-left">' +
            // 文档标题区
            '<div class="ka-clip-ed-section">' +
              '<label class="ka-clip-ed-label">文档标题</label>' +
              '<div class="ka-clip-ed-title-box">' +
                '<input type="text" class="ka-clip-ed-title-input" value="">' +
              '</div>' +
            '</div>' +

            // 元信息区
            '<div class="ka-clip-ed-meta-section">' +
              '<div class="ka-clip-ed-meta-row">' +
                '<span class="ka-clip-ed-meta-label">创建时间</span>' +
                '<span class="ka-clip-ed-meta-value">' + timeStr + '</span>' +
              '</div>' +
              '<div class="ka-clip-ed-meta-row">' +
                '<span class="ka-clip-ed-meta-label">来源</span>' +
                '<a class="ka-clip-ed-meta-link" href="' + pageUrl + '" target="_blank" title="' + pageUrl + '">' + pageUrl + '</a>' +
              '</div>' +
            '</div>' +

            // 截图缩略图
            screenshotHtml +
          '</div>' +

          // 右侧：Markdown 编辑器
          '<div class="ka-clip-ed-right">' +
            '<div class="ka-clip-ed-content-header">' +
              '<label class="ka-clip-ed-label">文档内容</label>' +
            '</div>' +
            '<div class="ka-clip-ed-content-box">' +
              '<textarea class="ka-clip-ed-textarea" style="display:none;" placeholder="输入 Markdown 内容…"></textarea>' +
              '<div class="ka-clip-ed-preview"></div>' +
              '<div class="ka-clip-ed-toolbar">' +
                '<button class="ka-editor-tool-btn" data-md="bold" title="粗体">' +
                  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M6 4h8a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6z"/><path d="M6 12h9a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6z"/></svg>' +
                '</button>' +
                '<div class="ka-md-sep"></div>' +
                '<button class="ka-editor-tool-btn" data-md="heading" title="标题">' +
                  '<svg viewBox="0 0 24 24" fill="currentColor"><text x="3" y="18" font-size="16" font-weight="bold" font-family="Arial">T</text></svg>' +
                '</button>' +
                '<div class="ka-md-sep"></div>' +
                '<button class="ka-editor-tool-btn" data-md="ul" title="无序列表">' +
                  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><circle cx="4" cy="6" r="1" fill="currentColor"/><circle cx="4" cy="12" r="1" fill="currentColor"/><circle cx="4" cy="18" r="1" fill="currentColor"/></svg>' +
                '</button>' +
                '<button class="ka-editor-tool-btn" data-md="ol" title="有序列表">' +
                  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="10" y1="6" x2="21" y2="6"/><line x1="10" y1="12" x2="21" y2="12"/><line x1="10" y1="18" x2="21" y2="18"/><text x="2" y="8" font-size="8" fill="currentColor" stroke="none" font-family="Arial">1</text><text x="2" y="14" font-size="8" fill="currentColor" stroke="none" font-family="Arial">2</text><text x="2" y="20" font-size="8" fill="currentColor" stroke="none" font-family="Arial">3</text></svg>' +
                '</button>' +
                '<div class="ka-md-sep"></div>' +
                '<button class="ka-editor-tool-btn" data-md="link" title="链接">' +
                  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>' +
                '</button>' +
                '<button class="ka-editor-tool-btn" data-md="image" title="图片">' +
                  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="18" height="18" rx="2" ry="2"/><circle cx="8.5" cy="8.5" r="1.5"/><polyline points="21 15 16 10 5 21"/></svg>' +
                '</button>' +
                '<button class="ka-editor-tool-btn ka-preview-active" data-md="preview" title="切换源码/预览">' +
                  '<svg class="ka-icon-preview" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>' +
                  '<svg class="ka-icon-source" style="display:none;" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/></svg>' +
                '</button>' +
              '</div>' +
            '</div>' +
          '</div>' +

        '</div>' +

        // 底部操作栏
        '<div class="ka-clip-ed-footer">' +
          '<div class="ka-clip-ed-footer-actions">' +
            '<button class="ka-editor-btn-cancel">取消</button>' +
            '<button class="ka-editor-btn-save">确认保存</button>' +
          '</div>' +
        '</div>' +
      '</div>';

    document.body.appendChild(overlay);
    clipEditorOverlay = overlay;

    // 设置标题和内容
    var titleInput = overlay.querySelector('.ka-clip-ed-title-input');
    titleInput.value = defaultTitle;

    var textarea = overlay.querySelector('.ka-clip-ed-textarea');
    textarea.value = textContent || '';

    // 默认显示 Markdown 预览模式（可编辑）
    var previewDiv = overlay.querySelector('.ka-clip-ed-preview');
    previewDiv.setAttribute('contenteditable', 'true');
    previewDiv.setAttribute('spellcheck', 'true');
    previewDiv.innerHTML = renderMarkdown(textarea.value);
    var previewBtn = overlay.querySelector('.ka-editor-tool-btn[data-md="preview"]');
    if (previewBtn) {
      previewBtn.style.color = '#07C160';
      previewBtn.querySelector('.ka-icon-preview').style.display = 'none';
      previewBtn.querySelector('.ka-icon-source').style.display = '';
    }

    function syncPreviewToTextarea() {
      textarea.value = htmlToMarkdown(previewDiv);
    }

    function syncTextareaToPreview() {
      previewDiv.innerHTML = renderMarkdown(textarea.value);
    }

    // 自动调整高度（由 CSS flex 控制实际可用空间）
    // 不自动 focus 标题，让用户自行选择编辑区域

    textarea.addEventListener('input', function () {
      if (previewDiv.style.display !== 'none') {
        syncTextareaToPreview();
      }
    });
    previewDiv.addEventListener('input', syncPreviewToTextarea);
    previewDiv.addEventListener('blur', syncPreviewToTextarea);

    // 关闭
    overlay.querySelector('.ka-editor-close').addEventListener('click', function () { closeClipEditor(); });
    overlay.querySelector('.ka-editor-btn-cancel').addEventListener('click', function () { closeClipEditor(); });
    overlay.addEventListener('click', function (e) { if (e.target === overlay) closeClipEditor(); });

    function onEsc(e) {
      if (e.key === 'Escape') { closeClipEditor(); document.removeEventListener('keydown', onEsc); }
    }
    document.addEventListener('keydown', onEsc);

    // Markdown 工具栏
    overlay.querySelectorAll('.ka-editor-tool-btn[data-md]').forEach(function (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        var action = btn.getAttribute('data-md');
        var pDiv = overlay.querySelector('.ka-clip-ed-preview');
        var inPreview = pDiv && pDiv.style.display !== 'none';

        if (action === 'preview') {
            var iconPreview = btn.querySelector('.ka-icon-preview');
            var iconSource = btn.querySelector('.ka-icon-source');
            if (pDiv.style.display !== 'none') {
              // 从预览模式切换到源码模式
              syncPreviewToTextarea();
              pDiv.style.display = 'none';
              textarea.style.display = 'block';
              textarea.focus();
              btn.style.color = '#666';
              btn.classList.remove('ka-preview-active');
              if (iconPreview) iconPreview.style.display = '';
              if (iconSource) iconSource.style.display = 'none';
            } else {
              // 从源码模式切换到预览模式：渲染 textarea 内容为 HTML
              syncTextareaToPreview();
              pDiv.style.display = 'block';
              textarea.style.display = 'none';
              btn.style.color = '#07C160';
              btn.classList.add('ka-preview-active');
              if (iconPreview) iconPreview.style.display = 'none';
              if (iconSource) iconSource.style.display = '';
            }
          return;
        }

        // 格式化操作：区分预览模式和源码模式
        if (inPreview) {
          // 预览模式：用 execCommand 操作富文本
          pDiv.focus();
          switch (action) {
            case 'bold':    document.execCommand('bold'); break;
            case 'heading':
              // 在光标处插入一个 h2
              document.execCommand('formatBlock', false, 'h2');
              break;
            case 'ul':      document.execCommand('insertUnorderedList'); break;
            case 'ol':      document.execCommand('insertOrderedList'); break;
            case 'link':
              var url = prompt('请输入链接地址:', 'https://');
              if (url) document.execCommand('createLink', false, url);
              break;
            case 'image':
              var imgUrl = prompt('请输入图片地址:', 'https://');
              if (imgUrl) document.execCommand('insertImage', false, imgUrl);
              break;
          }
          syncPreviewToTextarea();
        } else {
          // 源码模式：插入 Markdown 语法
          switch (action) {
            case 'bold':    editorInsertMd(textarea, '**', '**', '粗体文本'); break;
            case 'heading': editorInsertLinePrefix(textarea, '## '); break;
            case 'ul':      editorInsertLinePrefix(textarea, '- '); break;
            case 'ol':      editorInsertLinePrefix(textarea, '1. '); break;
            case 'link':    editorInsertMd(textarea, '[', '](url)', '链接文本'); break;
            case 'image':   editorInsertMd(textarea, '![', '](url)', '图片描述'); break;
          }
        }
      });
    });

    // 确认保存
    overlay.querySelector('.ka-editor-btn-save').addEventListener('click', function () {
      if (previewDiv.style.display !== 'none') {
        syncPreviewToTextarea();
      }
      var finalTitle = titleInput.value.trim() || defaultTitle;
      var finalContent = textarea.value.trim();

      if (!finalContent && !hasScreenshot) {
        showNotification('请输入内容', 'error');
        return;
      }

      closeClipEditor();

      if (hasCallback) {
        onSaveCallback(finalContent, finalTitle);
        return;
      }

      var clipData = {
        type: 'select-clip',
        content: finalContent || '（截图内容）',
        title: finalTitle,
        meta: { url: pageUrl, title: pageTitle, timestamp: Date.now() }
      };

      if (hasScreenshot) {
        clipData.screenshot = screenshotDataUrl;
      }

      safeSendMessage({
        type: 'SAVE_CLIP',
        payload: clipData
      }, function (resp) {
        if (resp && resp.success) {
          var msg = '内容已保存';
          if (resp.syncedToKb && resp.kbName) {
            msg = '已保存并同步到「' + resp.kbName + '」';
          } else if (resp.syncError) {
            msg = '保存失败，请稍后重试';
            showNotification(msg, 'error');
            safeSendMessage({
              type: 'CLIP_RESULT',
              payload: { title: clipData.title, content: clipData.content }
            });
            return;
          }
          showNotification(msg, 'success');
          safeSendMessage({
            type: 'CLIP_RESULT',
            payload: { title: clipData.title, content: clipData.content }
          });
        } else {
          showNotification('保存失败: ' + ((resp && resp.error) || '未知错误'), 'error');
        }
      });
    });
  }

  function closeClipEditor() {
    if (clipEditorOverlay) {
      clipEditorOverlay.remove();
      clipEditorOverlay = null;
    }
  }

  function extractTextInRect(absX, absY, w, h) {
    // 隐藏遮罩以免干扰元素查找
    if (clipState.overlay) clipState.overlay.style.display = 'none';

    var texts = [];
    var walker = document.createTreeWalker(
      document.body,
      NodeFilter.SHOW_TEXT,
      null,
      false
    );

    var node;
    while (node = walker.nextNode()) {
      var text = node.textContent.trim();
      if (!text) continue;

      var range = document.createRange();
      range.selectNodeContents(node);
      var rects = range.getClientRects();

      for (var i = 0; i < rects.length; i++) {
        var r = rects[i];
        var nodeAbsX = r.left + window.scrollX;
        var nodeAbsY = r.top + window.scrollY;

        // 检查是否在选区内（有交集）
        if (nodeAbsX + r.width > absX &&
            nodeAbsX < absX + w &&
            nodeAbsY + r.height > absY &&
            nodeAbsY < absY + h) {
          texts.push(text);
          break;
        }
      }
    }

    if (clipState.overlay) clipState.overlay.style.display = '';

    // 去重并保持顺序
    var seen = {};
    var unique = [];
    texts.forEach(function (t) {
      if (!seen[t]) {
        seen[t] = true;
        unique.push(t);
      }
    });

    return unique.join('\n');
  }

  function cancelClip() {
    clipState.active = false;
    document.removeEventListener('keydown', onClipKeyDown);
    document.removeEventListener('mousemove', onClipMouseMove);
    document.removeEventListener('mouseup', onClipMouseUp);
    if (clipState.overlay) {
      clipState.overlay.remove();
      clipState.overlay = null;
    }
    clipState.rect = null;
    clipState.toolbar = null;
    clipState.dimTop = null;
    clipState.dimBottom = null;
    clipState.dimLeft = null;
    clipState.dimRight = null;
  }

  // === 页面内通知 ===
  function showNotification(msg, type) {
    var n = document.createElement('div');
    n.className = 'ka-notification ka-notification-' + (type || 'info');
    n.textContent = msg;
    document.body.appendChild(n);

    requestAnimationFrame(function () {
      n.classList.add('ka-notification-show');
    });

    setTimeout(function () {
      n.classList.remove('ka-notification-show');
      setTimeout(function () { n.remove(); }, 300);
    }, 2500);
  }

  // === 消息监听 ===
  function ensureMessageListener() {
    // 如果上下文已失效，不再尝试注册
    if (!isRuntimeValid()) return;

    // 移除旧版监听器（如果有）
    if (window.__kaMessageListener) {
      try { chrome.runtime.onMessage.removeListener(window.__kaMessageListener); } catch (e) {}
    }

    window.__kaMessageListener = function (msg, sender, sendResponse) {
      // 登录/知识库状态变更通知 — 刷新本地缓存
      if (msg.type === 'AUTH_STATE_CHANGED') {
        refreshAuthState();
        sendResponse({ success: true });
        return true;
      }
      if (msg.type === 'SMART_CLIP') {
        if (!isFunctionReady()) { showAuthGuardHint(); sendResponse({ success: false }); return true; }
        smartClip();
        sendResponse({ success: true });
      }
      if (msg.type === 'SELECT_CLIP') {
        if (!isFunctionReady()) { showAuthGuardHint(); sendResponse({ success: false }); return true; }
        startSelectClip();
        sendResponse({ success: true });
      }
      if (msg.type === 'SHOW_NOTIFICATION' && msg.payload) {
        showNotification(msg.payload.msg, msg.payload.status);
        sendResponse({ success: true });
      }
      if (msg.type === 'QUICK_NOTE') {
        if (!kaUserAuth) { showNotification('请先在扩展中登录账号', 'error'); sendResponse({ success: false }); return true; }
        openQuickNotePanel();
        sendResponse({ success: true });
      }
      if (msg.type === 'OPEN_EDITOR_FOR_SELECTION' && msg.payload) {
        if (!isFunctionReady()) { showAuthGuardHint(); sendResponse({ success: false }); return true; }
        openClipEditor(msg.payload.text, null, function (editedContent) {
          doSaveSelection(editedContent);
        });
        sendResponse({ success: true });
      }
      if (msg.type === 'EDIT_KB_KNOWLEDGE' && msg.payload) {
        var kbId = msg.payload.kbId;
        var knowledgeId = msg.payload.knowledgeId;
        var itemTitle = msg.payload.title || '未命名';
        if (!kbId || !knowledgeId) { sendResponse({ success: false, error: '缺少参数' }); return true; }
        chrome.runtime.sendMessage({ type: 'GET_KB_KNOWLEDGE', payload: { kbId: kbId, knowledgeId: knowledgeId } }, function (resp) {
          if (chrome.runtime.lastError) { showNotification('加载失败', 'error'); return; }
          var rawContent = '';
          var rawTitle = itemTitle;
          if (resp && resp.success !== false && resp.data) {
            var d = resp.data;
            rawContent = d.metadata && d.metadata.content ? d.metadata.content : d.content || '';
            rawTitle = d.title || d.name || rawTitle;
          } else if (resp && resp.data) {
            rawContent = resp.data.content || '';
            rawTitle = resp.data.title || resp.data.name || rawTitle;
          }
          var editContent = rawContent
            .replace(/^<!--\s*weknora-clip-type:\S+\s*-->\n?/, '')
            .replace(/^>\s*来源:\s*https?:\/\/\S+\n\n?/, '');
          openClipEditor(editContent, null, function (finalContent, finalTitle) {
            chrome.runtime.sendMessage({
              type: 'UPDATE_KB_KNOWLEDGE',
              payload: { kbId: kbId, knowledgeId: knowledgeId, title: finalTitle, content: finalContent }
            }, function (updateResp) {
              if (chrome.runtime.lastError) { showNotification('保存失败', 'error'); return; }
              if (updateResp && updateResp.success !== false) {
                showNotification('已更新', 'success');
              } else {
                showNotification('更新失败: ' + ((updateResp && updateResp.error) || '未知错误'), 'error');
              }
            });
          }, { defaultTitle: rawTitle, headerTitle: '编辑知识' });
        });
        sendResponse({ success: true });
      }
      if (msg.type === 'TOKEN_EXPIRED') {
        kaUserAuth = null;
        kaClipKbId = '';
        kaClipKbName = '';
        showNotification('登录已过期，请点击扩展图标重新扫码登录', 'error');
        sendResponse({ success: true });
      }
      return true;
    };

    chrome.runtime.onMessage.addListener(window.__kaMessageListener);
  }

  // 统一的保存函数，带重试和唤醒机制
  function doSaveSelection(editedContent, retryCount) {
    if (typeof retryCount === 'undefined') retryCount = 0;

    // 前置检查：上下文是否有效
    if (!isRuntimeValid()) {
      showRefreshHint();
      return;
    }

    var clipTitle = '选中文本 - ' + (document.title || location.hostname);
    var clipPayload = {
      type: 'select-clip',
      content: editedContent,
      title: clipTitle,
      meta: { url: location.href, title: document.title || '' }
    };

    // 先发一个轻量消息唤醒 service worker
    safeSendMessage({ type: 'GET_AUTH' }, function () {
      // service worker 已激活，现在执行真正的保存
      safeSendMessage({
        type: 'SAVE_SELECTION',
        payload: clipPayload
      }, function (saveResp) {
        if (saveResp === null) {
          // safeSendMessage 检测到上下文失效，已经弹出刷新提示，不需要重试
          return;
        }
        if (saveResp && saveResp.success) {
          var msg = '保存成功';
          if (saveResp.syncedToKb && saveResp.kbName) {
            msg = '保存成功，已同步到「' + saveResp.kbName + '」';
          } else if (saveResp.syncError) {
            showNotification('保存失败，请稍后重试', 'error');
            return;
          }
          showNotification(msg, 'success');
        } else {
          // 其他错误可以重试
          if (retryCount < 3) {
            setTimeout(function () { doSaveSelection(editedContent, retryCount + 1); }, 800);
          } else {
            showNotification('保存失败: ' + ((saveResp && saveResp.error) || '请重试'), 'error');
          }
        }
      });
    });
  }

  // Markdown 工具辅助函数
  function editorInsertMd(textarea, before, after, placeholder) {
    var start = textarea.selectionStart;
    var end = textarea.selectionEnd;
    var val = textarea.value;
    var selected = val.substring(start, end) || placeholder;
    var newVal = val.substring(0, start) + before + selected + after + val.substring(end);
    textarea.value = newVal;
    textarea.focus();
    // 选中插入的文本
    var newStart = start + before.length;
    var newEnd = newStart + selected.length;
    textarea.setSelectionRange(newStart, newEnd);
  }

  function editorInsertLinePrefix(textarea, prefix) {
    var start = textarea.selectionStart;
    var val = textarea.value;
    // 找到当前行的开头
    var lineStart = val.lastIndexOf('\n', start - 1) + 1;
    var newVal = val.substring(0, lineStart) + prefix + val.substring(lineStart);
    textarea.value = newVal;
    textarea.focus();
    textarea.setSelectionRange(start + prefix.length, start + prefix.length);
  }

  // 轻量级 Markdown → HTML 渲染
  function renderMarkdown(md) {
    // 先对特殊 HTML 字符做转义
    function esc(s) {
      return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    }

    // 第一步：提取原始 HTML 块（table 等），用占位符替代，避免被转义
    var htmlBlocks = [];
    var processed = md.replace(/<table[\s\S]*?<\/table>/gi, function (match) {
      var idx = htmlBlocks.length;
      // 给 table 加样式后直接保留
      var styled = match
        .replace(/<table(?![^>]*style)/gi, '<table style="border-collapse:collapse;width:100%;margin:10px 0;font-size:14px;"')
        .replace(/<th(?![^>]*style)/gi, '<th style="border:1px solid #ddd;padding:8px 12px;background:#f6f8fa;font-weight:600;text-align:left;"')
        .replace(/<td(?![^>]*style)/gi, '<td style="border:1px solid #ddd;padding:8px 12px;"');
      htmlBlocks.push(styled);
      return '\x00HTMLBLOCK' + idx + '\x00';
    });

    var lines = processed.split('\n');
    var html = [];
    var inCodeBlock = false;
    var inList = false;
    var listType = '';
    // Markdown 表格状态
    var inMdTable = false;
    var mdTableRows = [];

    function flushMdTable() {
      if (mdTableRows.length === 0) return;
      var tableHtml = '<table style="border-collapse:collapse;width:100%;margin:10px 0;font-size:14px;">';
      for (var t = 0; t < mdTableRows.length; t++) {
        var cells = mdTableRows[t];
        var cellTag = t === 0 ? 'th' : 'td';
        var cellStyle = t === 0
          ? 'border:1px solid #ddd;padding:8px 12px;background:#f6f8fa;font-weight:600;text-align:left;'
          : 'border:1px solid #ddd;padding:8px 12px;';
        tableHtml += '<tr>';
        for (var c = 0; c < cells.length; c++) {
          tableHtml += '<' + cellTag + ' style="' + cellStyle + '">' + inlineRender(cells[c]) + '</' + cellTag + '>';
        }
        tableHtml += '</tr>';
      }
      tableHtml += '</table>';
      html.push(tableHtml);
      mdTableRows = [];
      inMdTable = false;
    }

    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];

      // 代码块 ```
      if (/^```/.test(line)) {
        if (inMdTable) flushMdTable();
        if (inCodeBlock) {
          html.push('</code></pre>');
          inCodeBlock = false;
        } else {
          if (inList) { html.push(listType === 'ul' ? '</ul>' : '</ol>'); inList = false; }
          html.push('<pre style="background:#f6f8fa;padding:12px 16px;border-radius:6px;overflow-x:auto;font-size:13px;line-height:1.6;"><code>');
          inCodeBlock = true;
        }
        continue;
      }
      if (inCodeBlock) {
        html.push(esc(line) + '\n');
        continue;
      }

      // HTML 块占位符 — 直接还原
      var blockMatch = line.match(/^\x00HTMLBLOCK(\d+)\x00$/);
      if (blockMatch) {
        if (inMdTable) flushMdTable();
        if (inList) { html.push(listType === 'ul' ? '</ul>' : '</ol>'); inList = false; }
        html.push(htmlBlocks[parseInt(blockMatch[1])]);
        continue;
      }

      // Markdown 表格：| col1 | col2 | 格式
      var pipeMatch = line.match(/^\|(.+)\|$/);
      if (pipeMatch) {
        var raw = pipeMatch[1];
        // 分隔行 |---|---| 跳过
        if (/^[\s|:-]+$/.test(raw)) {
          continue;
        }
        var cells = raw.split('|').map(function (c) { return c.trim(); });
        if (!inMdTable) inMdTable = true;
        mdTableRows.push(cells);
        continue;
      } else if (inMdTable) {
        flushMdTable();
      }

      // 空行
      if (line.trim() === '') {
        if (inList) { html.push(listType === 'ul' ? '</ul>' : '</ol>'); inList = false; }
        html.push('<div class="ka-md-blank" data-blank="1"></div>');
        continue;
      }

      // 标题 h1-h6
      var hMatch = line.match(/^(#{1,6})\s+(.*)$/);
      if (hMatch) {
        if (inList) { html.push(listType === 'ul' ? '</ul>' : '</ol>'); inList = false; }
        var level = hMatch[1].length;
        var sizes = { 1: '24px', 2: '20px', 3: '17px', 4: '15px', 5: '14px', 6: '13px' };
        html.push('<h' + level + ' style="margin:12px 0 6px;font-size:' + sizes[level] + ';font-weight:600;color:#1a1a1a;">' + inlineRender(hMatch[2]) + '</h' + level + '>');
        continue;
      }

      // 无序列表
      var ulMatch = line.match(/^[\s]*[-*+]\s+(.*)$/);
      if (ulMatch) {
        if (!inList || listType !== 'ul') {
          if (inList) html.push(listType === 'ul' ? '</ul>' : '</ol>');
          html.push('<ul style="margin:6px 0;padding-left:24px;">');
          inList = true;
          listType = 'ul';
        }
        html.push('<li style="margin:3px 0;line-height:1.7;">' + inlineRender(ulMatch[1]) + '</li>');
        continue;
      }

      // 有序列表
      var olMatch = line.match(/^[\s]*\d+\.\s+(.*)$/);
      if (olMatch) {
        if (!inList || listType !== 'ol') {
          if (inList) html.push(listType === 'ul' ? '</ul>' : '</ol>');
          html.push('<ol style="margin:6px 0;padding-left:24px;">');
          inList = true;
          listType = 'ol';
        }
        html.push('<li style="margin:3px 0;line-height:1.7;">' + inlineRender(olMatch[1]) + '</li>');
        continue;
      }

      // 分割线
      if (/^[-*_]{3,}\s*$/.test(line.trim())) {
        if (inList) { html.push(listType === 'ul' ? '</ul>' : '</ol>'); inList = false; }
        html.push('<hr style="border:none;border-top:1px solid #e5e5e5;margin:12px 0;">');
        continue;
      }

      // 引用块
      var bqMatch = line.match(/^>\s?(.*)$/);
      if (bqMatch) {
        if (inList) { html.push(listType === 'ul' ? '</ul>' : '</ol>'); inList = false; }
        html.push('<blockquote style="margin:8px 0;padding:4px 16px;border-left:3px solid #07C160;color:#666;background:#f9f9f9;border-radius:0 4px 4px 0;">' + inlineRender(bqMatch[1]) + '</blockquote>');
        continue;
      }

      // 普通段落
      if (inList) { html.push(listType === 'ul' ? '</ul>' : '</ol>'); inList = false; }
      html.push('<p style="margin:6px 0;line-height:1.8;color:#333;">' + inlineRender(line) + '</p>');
    }

    if (inMdTable) flushMdTable();
    if (inCodeBlock) html.push('</code></pre>');
    if (inList) html.push(listType === 'ul' ? '</ul>' : '</ol>');

    // 用 join('') 不插入额外换行，避免 round-trip 时白文本节点叠加
    return html.join('');
  }

  // 将 contentEditable 预览区的 HTML 转回 Markdown
  function htmlToMarkdown(el) {
    var result = [];

    function walkChildren(parent) {
      for (var i = 0; i < parent.childNodes.length; i++) {
        walk(parent.childNodes[i]);
      }
    }

    function walk(node) {
      // 跳过纯空白文本节点（元素之间的缩进/换行）
      if (node.nodeType === 3) {
        var txt = node.textContent;
        // 如果文本节点只包含空白且在块级元素之间，跳过
        if (/^\s*$/.test(txt) && node.parentElement) {
          var prev = node.previousSibling;
          var next = node.nextSibling;
          var prevIsBlock = prev && prev.nodeType === 1 && isBlockTag(prev.tagName);
          var nextIsBlock = next && next.nodeType === 1 && isBlockTag(next.tagName);
          if (prevIsBlock || nextIsBlock || (!prev && !next)) return;
        }
        result.push(txt);
        return;
      }
      if (node.nodeType !== 1) return;

      var tag = node.tagName.toLowerCase();

      // 空行占位符
      if (node.getAttribute('data-blank') === '1' || (tag === 'div' && node.className === 'ka-md-blank')) {
        result.push('\n');
        return;
      }

      if (tag === 'br') { result.push('\n'); return; }
      if (tag === 'hr') { ensureNewline(); result.push('---\n'); return; }
      if (tag === 'img') { result.push('![' + (node.alt || '') + '](' + (node.src || '') + ')'); return; }

      // 块级元素
      if (/^h[1-6]$/.test(tag)) {
        var lvl = parseInt(tag[1]);
        ensureNewline();
        result.push('#'.repeat(lvl) + ' ');
        walkChildren(node);
        result.push('\n');
        return;
      }
      if (tag === 'p') {
        ensureNewline();
        walkChildren(node);
        result.push('\n');
        return;
      }
      if (tag === 'blockquote') {
        ensureNewline();
        result.push('> ');
        walkChildren(node);
        result.push('\n');
        return;
      }
      if (tag === 'ul' || tag === 'ol') {
        ensureNewline();
        var items = node.querySelectorAll(':scope > li');
        for (var j = 0; j < items.length; j++) {
          var bullet = tag === 'ol' ? (j + 1) + '. ' : '- ';
          result.push(bullet);
          walkChildren(items[j]);
          result.push('\n');
        }
        return;
      }
      if (tag === 'li') {
        // 直接由 ul/ol 处理，跳过
        return;
      }
      if (tag === 'pre') {
        ensureNewline();
        var code = node.querySelector('code');
        result.push('```\n' + (code ? code.textContent : node.textContent) + '\n```\n');
        return;
      }

      // 表格 → Markdown 表格
      if (tag === 'table') {
        ensureNewline();
        var rows = node.querySelectorAll('tr');
        if (rows.length === 0) { walkChildren(node); return; }
        for (var r = 0; r < rows.length; r++) {
          var cells = rows[r].querySelectorAll('th, td');
          var cellTexts = [];
          for (var c = 0; c < cells.length; c++) {
            cellTexts.push(cells[c].textContent.trim().replace(/\|/g, '\\|').replace(/\n/g, ' '));
          }
          result.push('| ' + cellTexts.join(' | ') + ' |\n');
          // 在第一行后插入分隔行
          if (r === 0) {
            var sep = [];
            for (var c = 0; c < cellTexts.length; c++) sep.push('---');
            result.push('| ' + sep.join(' | ') + ' |\n');
          }
        }
        result.push('\n');
        return;
      }
      // 跳过表格内部元素（由 table 统一处理）
      if (tag === 'thead' || tag === 'tbody' || tag === 'tfoot' || tag === 'tr' || tag === 'th' || tag === 'td') {
        return;
      }

      // 行内元素
      if (tag === 'strong' || tag === 'b') {
        result.push('**');
        walkChildren(node);
        result.push('**');
        return;
      }
      if (tag === 'em' || tag === 'i') {
        result.push('*');
        walkChildren(node);
        result.push('*');
        return;
      }
      if (tag === 'del' || tag === 's') {
        result.push('~~');
        walkChildren(node);
        result.push('~~');
        return;
      }
      if (tag === 'code') {
        result.push('`');
        walkChildren(node);
        result.push('`');
        return;
      }
      if (tag === 'a') {
        result.push('[');
        walkChildren(node);
        result.push('](' + (node.href || '') + ')');
        return;
      }

      // 其他 div/span 等：直接遍历子节点
      walkChildren(node);
    }

    function isBlockTag(tagName) {
      return /^(P|H[1-6]|DIV|UL|OL|LI|BLOCKQUOTE|PRE|HR|TABLE|SECTION|ARTICLE|HEADER|FOOTER|NAV|MAIN)$/i.test(tagName);
    }

    function ensureNewline() {
      var last = result.length > 0 ? result[result.length - 1] : '\n';
      if (last.length > 0 && last[last.length - 1] !== '\n') {
        result.push('\n');
      }
    }

    walkChildren(el);

    // 归一化：连续 3 个以上换行 → 2 个换行（一个空行）
    return result.join('').replace(/\n{3,}/g, '\n\n').trim();
  }

  // 行内 Markdown 渲染（粗体、斜体、行内代码、链接、图片）
  function inlineRender(text) {
    // 先转义 HTML
    text = text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    // 图片 ![alt](url)
    text = text.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, '<img src="$2" alt="$1" style="max-width:100%;border-radius:4px;margin:4px 0;">');
    // 链接 [text](url)
    text = text.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" style="color:#07C160;text-decoration:underline;">$1</a>');
    // 行内代码 `code`
    text = text.replace(/`([^`]+)`/g, '<code style="background:#f0f0f0;padding:2px 6px;border-radius:3px;font-size:13px;color:#d63384;">$1</code>');
    // 粗斜体 ***text***
    text = text.replace(/\*\*\*([^*]+)\*\*\*/g, '<strong><em>$1</em></strong>');
    // 粗体 **text**
    text = text.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    // 斜体 *text*
    text = text.replace(/\*([^*]+)\*/g, '<em>$1</em>');
    // 删除线 ~~text~~
    text = text.replace(/~~([^~]+)~~/g, '<del>$1</del>');
    // 处理 Markdown 反斜杠转义（Turndown 生成的 \_ \* \# \[ 等）
    text = text.replace(/\\([_*#\[\]()~`>|\\!{}.+-])/g, '$1');
    return text;
  }

  // === 选中文字气泡 ===
  var selBubble = null;
  var selBubbleHideTimer = null;

  function removeSelBubble() {
    if (selBubble) {
      selBubble.remove();
      selBubble = null;
    }
    if (selBubbleHideTimer) {
      clearTimeout(selBubbleHideTimer);
      selBubbleHideTimer = null;
    }
  }

  function createSelBubble(text, rect) {
    removeSelBubble();

    var bubble = document.createElement('div');
    bubble.className = 'ka-sel-bubble';

    // 保存到笔记按钮
    var saveBtn = document.createElement('button');
    saveBtn.className = 'ka-sel-btn ka-sel-btn-save';
    saveBtn.setAttribute('data-tip', '保存到笔记');
    saveBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/></svg>保存';

    // 分隔线
    var divider = document.createElement('div');
    divider.className = 'ka-sel-divider';

    // 问知识管理助手按钮
    var askBtn = document.createElement('button');
    askBtn.className = 'ka-sel-btn ka-sel-btn-ask';
    askBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>问知识管理助手';

    bubble.appendChild(saveBtn);
    bubble.appendChild(divider);
    bubble.appendChild(askBtn);

    // 第二条分隔线
    var divider2 = document.createElement('div');
    divider2.className = 'ka-sel-divider';
    bubble.appendChild(divider2);

    // X 关闭按钮（带禁用下拉菜单）
    var closeWrap = document.createElement('div');
    closeWrap.className = 'ka-sel-close-wrap';

    var closeBtn = document.createElement('button');
    closeBtn.className = 'ka-sel-close';
    closeBtn.title = '关闭';
    closeBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>';
    closeWrap.appendChild(closeBtn);

    // 禁用下拉菜单（默认隐藏）
    var disableMenu = document.createElement('div');
    disableMenu.className = 'ka-sel-disable-menu';
    disableMenu.style.display = 'none';

    var disablePageItem = document.createElement('button');
    disablePageItem.className = 'ka-sel-disable-item';
    disablePageItem.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18.36 6.64A9 9 0 0 1 20.5 12c0 4.97-4.03 9-9 9a8.96 8.96 0 0 1-6.36-2.64"/><path d="M1 1l22 22"/><circle cx="12" cy="12" r="9" opacity="0.3"/></svg>当前页面禁用';

    var disableAllItem = document.createElement('button');
    disableAllItem.className = 'ka-sel-disable-item';
    disableAllItem.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></svg>所有页面禁用';

    disableMenu.appendChild(disablePageItem);
    disableMenu.appendChild(disableAllItem);
    closeWrap.appendChild(disableMenu);

    bubble.appendChild(closeWrap);

    // X 按钮点击 → 显示/隐藏禁用菜单
    closeBtn.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      var isShown = disableMenu.style.display !== 'none';
      disableMenu.style.display = isShown ? 'none' : 'block';
    });

    // "当前页面禁用"
    disablePageItem.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      var currentUrl = window.location.href;
      kaDisabledPages[currentUrl] = true;
      // 存储到 session storage（关闭浏览器后重置）
      try {
        chrome.storage.session.set({ ka_disabled_pages: kaDisabledPages });
      } catch (e) {}
      showNotification('已禁用当前页面的选中气泡', 'info');
      removeSelBubble();
    });

    // "所有页面禁用"
    disableAllItem.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      kaAllPagesDisabled = true;
      // 存储到 session storage（关闭浏览器后重置）
      try {
        chrome.storage.session.set({ ka_all_pages_disabled: true });
      } catch (e) {}
      showNotification('已禁用所有页面的选中气泡（浏览器重启后恢复）', 'info');
      removeSelBubble();
    });

    // 小三角
    var arrow = document.createElement('div');
    arrow.className = 'ka-sel-arrow';
    bubble.appendChild(arrow);

    document.body.appendChild(bubble);
    selBubble = bubble;

    // 定位：在选区上方居中
    var bubbleW = bubble.offsetWidth;
    var bubbleH = bubble.offsetHeight;
    var left = rect.left + rect.width / 2 - bubbleW / 2 + window.scrollX;
    var top = rect.top - bubbleH - 10 + window.scrollY;

    // 防止溢出左右
    if (left < 8) left = 8;
    if (left + bubbleW > document.documentElement.scrollWidth - 8) {
      left = document.documentElement.scrollWidth - bubbleW - 8;
    }
    // 如果上方空间不够，放到下方
    if (rect.top - bubbleH - 10 < 0) {
      top = rect.bottom + 10 + window.scrollY;
      arrow.className = 'ka-sel-arrow ka-sel-arrow-top';
    }

    bubble.style.left = left + 'px';
    bubble.style.top = top + 'px';

    // "问 WeKnora" — 发送到 sidepanel
    askBtn.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      safeSendMessage({
        type: 'ASK_WEKNORA',
        payload: { text: text }
      });
      removeSelBubble();
      window.getSelection().removeAllRanges();
    });

    // "保存到笔记" — 打开 Markdown 编辑弹窗
    saveBtn.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      removeSelBubble();
      window.getSelection().removeAllRanges();

      openClipEditor(text, null, function (editedContent) {
        doSaveSelection(editedContent);
      });
    });

    // 点击气泡外关闭（同时关闭禁用菜单）
    setTimeout(function () {
      document.addEventListener('mousedown', onBubbleOutsideClick);
    }, 50);
  }

  function onBubbleOutsideClick(e) {
    if (selBubble && !selBubble.contains(e.target)) {
      removeSelBubble();
      document.removeEventListener('mousedown', onBubbleOutsideClick);
    }
  }

  // 监听鼠标抬起 → 检测选中文字
  document.addEventListener('mouseup', function (e) {
    // 如果正在框选剪藏或点击在气泡内，不触发
    if (clipState.active) return;
    if (selBubble && selBubble.contains(e.target)) return;
    if (imgBubble && imgBubble.contains(e.target)) return;

    // 未登录或未选知识库时，不显示气泡
    if (!isFunctionReady()) return;

    // 用户关闭了选中气泡
    if (!kaSelBubbleEnabled) return;

    // 检查页面禁用状态
    if (kaAllPagesDisabled) return;
    if (kaDisabledPages[window.location.href]) return;

    // 延迟一点让 selection 更新
    setTimeout(function () {
      var sel = window.getSelection();
      var text = sel ? sel.toString().trim() : '';

      if (text.length < 2) {
        // 没有选中有效文字，移除气泡
        removeSelBubble();
        document.removeEventListener('mousedown', onBubbleOutsideClick);
        return;
      }

      // 获取选区矩形
      if (sel.rangeCount > 0) {
        var range = sel.getRangeAt(0);
        var rects = range.getClientRects();
        if (rects.length > 0) {
          // 使用第一个 rect 的顶部和整体范围来定位
          var boundRect = range.getBoundingClientRect();
          createSelBubble(text, boundRect);
        }
      }
    }, 10);
  });

  // === 右键图片气泡 ===
  var imgBubble = null;

  function removeImgBubble() {
    if (imgBubble) {
      imgBubble.remove();
      imgBubble = null;
    }
  }

  function createImgBubble(imgSrc, imgAlt, rect) {
    removeImgBubble();
    removeSelBubble(); // 同时移除文字气泡

    var bubble = document.createElement('div');
    bubble.className = 'ka-sel-bubble ka-img-bubble';

    // 保存图片按钮
    var saveBtn = document.createElement('button');
    saveBtn.className = 'ka-sel-btn ka-sel-btn-save';
    saveBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/></svg>保存图片到笔记';

    // 分隔线
    var divider = document.createElement('div');
    divider.className = 'ka-sel-divider';

    // 问知识管理助手按钮
    var askBtn = document.createElement('button');
    askBtn.className = 'ka-sel-btn ka-sel-btn-ask';
    askBtn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>问知识管理助手';

    bubble.appendChild(saveBtn);
    bubble.appendChild(divider);
    bubble.appendChild(askBtn);

    // 小三角
    var arrow = document.createElement('div');
    arrow.className = 'ka-sel-arrow';
    bubble.appendChild(arrow);

    document.body.appendChild(bubble);
    imgBubble = bubble;

    // 定位：在图片上方居中
    var bubbleW = bubble.offsetWidth;
    var bubbleH = bubble.offsetHeight;
    var left = rect.left + rect.width / 2 - bubbleW / 2 + window.scrollX;
    var top = rect.top - bubbleH - 10 + window.scrollY;

    // 防止溢出左右
    if (left < 8) left = 8;
    if (left + bubbleW > document.documentElement.scrollWidth - 8) {
      left = document.documentElement.scrollWidth - bubbleW - 8;
    }
    // 如果上方空间不够，放到下方
    if (rect.top - bubbleH - 10 < 0) {
      top = rect.bottom + 10 + window.scrollY;
      arrow.className = 'ka-sel-arrow ka-sel-arrow-top';
    }

    bubble.style.left = left + 'px';
    bubble.style.top = top + 'px';

    // "保存图片到笔记"
    saveBtn.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      var clipTitle = '图片收藏 - ' + (document.title || location.hostname);
      safeSendMessage({
        type: 'SAVE_IMAGE',
        payload: {
          type: 'image-clip',
          content: '![' + (imgAlt || '图片') + '](' + imgSrc + ')',
          title: clipTitle,
          meta: { url: location.href, title: document.title || '', imageUrl: imgSrc }
        }
      }, function (saveResp) {
        if (saveResp && saveResp.success) {
          var msg = '图片已保存到笔记';
          if (saveResp.syncedToKb && saveResp.kbName) {
            msg = '图片已保存，并同步到「' + saveResp.kbName + '」';
          } else if (saveResp.syncError) {
            showNotification('保存失败，请稍后重试', 'error');
            removeImgBubble();
            return;
          }
          showNotification(msg, 'success');
        } else if (saveResp !== null) {
          showNotification('保存失败', 'error');
        }
      });
      removeImgBubble();
    });

    // "问 WeKnora" — 发送图片描述到 sidepanel
    askBtn.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      var question = '请描述这张图片的内容: ' + imgSrc;
      safeSendMessage({
        type: 'ASK_WEKNORA',
        payload: { text: question }
      });
      removeImgBubble();
    });

    // 点击气泡外关闭
    setTimeout(function () {
      document.addEventListener('mousedown', onImgBubbleOutsideClick);
    }, 50);
  }

  function onImgBubbleOutsideClick(e) {
    if (imgBubble && !imgBubble.contains(e.target)) {
      removeImgBubble();
      document.removeEventListener('mousedown', onImgBubbleOutsideClick);
    }
  }

  // 监听右键点击图片 → 弹出气泡
  document.addEventListener('contextmenu', function (e) {
    // 如果正在框选剪藏，不触发
    if (clipState.active) return;

    // 未登录或未选知识库时，不显示图片气泡
    if (!isFunctionReady()) return;

    var target = e.target;
    // 检查是否右键点击了图片
    if (target.tagName === 'IMG' && target.src) {
      // 延迟一点，让浏览器原生右键菜单先出现
      setTimeout(function () {
        var imgRect = target.getBoundingClientRect();
        createImgBubble(target.src, target.alt || '', imgRect);
      }, 100);
    } else {
      // 不是图片，移除图片气泡
      removeImgBubble();
    }
  });

  // 监听 storage 变化，实时更新气泡开关和禁用状态
  try {
    chrome.storage.onChanged.addListener(function (changes, area) {
      if (area === 'local' && changes.ka_sel_bubble_enabled) {
        kaSelBubbleEnabled = changes.ka_sel_bubble_enabled.newValue !== false;
        if (!kaSelBubbleEnabled) removeSelBubble();
      }
      if (area === 'session') {
        if (changes.ka_disabled_pages) {
          kaDisabledPages = changes.ka_disabled_pages.newValue || {};
        }
        if (changes.ka_all_pages_disabled) {
          kaAllPagesDisabled = !!changes.ka_all_pages_disabled.newValue;
          if (kaAllPagesDisabled) removeSelBubble();
        }
      }
    });
  } catch (e) {}

  // 暴露到 window 上以便防重复注入时调用
  window.ensureMessageListener = ensureMessageListener;

  ensureMessageListener();
})();

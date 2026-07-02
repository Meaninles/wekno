// auth-callback.js — OAuth 回调处理
// chatbot.weixin.qq.com 登录成功后 redirect 到此页面，URL 参数中携带 token
// 支持两种场景：
//   1. 在 popup 的 iframe 中 — 通过 postMessage 通知 popup
//   2. 在独立 tab 中打开 — 存储后关闭 tab
(function () {
  var msgEl = document.getElementById('msg');
  var subEl = document.getElementById('sub');
  var iconEl = document.getElementById('icon');
  var inIframe = (window !== window.top);

  function showSuccess(text) {
    if (iconEl) {
      iconEl.className = 'icon icon-success';
      iconEl.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="#07C160" stroke-width="2.5"><polyline points="20 6 9 17 4 12"/></svg>';
    }
    if (msgEl) msgEl.textContent = text;
    if (subEl) subEl.textContent = inIframe ? '' : '页面即将自动关闭';
  }

  function showError(text) {
    if (iconEl) {
      iconEl.className = 'icon icon-error';
      iconEl.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="#e53935" stroke-width="2.5"><circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/></svg>';
    }
    if (msgEl) msgEl.textContent = text;
    if (subEl) subEl.textContent = '请关闭此页面后重试';
  }

  // 通知 popup（如果在 iframe 中）
  function notifyParent(success, data) {
    if (inIframe && window.parent) {
      window.parent.postMessage({
        type: 'WEKNORA_AUTH_CALLBACK',
        payload: Object.assign({ success: success }, data)
      }, '*');
    }
  }

  // 从 URL 参数提取登录信息
  var params = new URLSearchParams(location.search);

  // chatbot.weixin.qq.com 回调可能携带的参数（按优先级尝试多种参数名）
  var token = params.get('token') || params.get('access_token') || params.get('code') || '';
  var name = params.get('name') || params.get('nickname') || params.get('username') || '';
  var avatar = params.get('avatar') || params.get('headimgurl') || '';
  var error = params.get('error') || params.get('errmsg') || '';

  // 如果 URL hash 中也有参数（某些 OAuth 实现用 fragment）
  if (!token && location.hash) {
    var hashParams = new URLSearchParams(location.hash.substring(1));
    token = hashParams.get('token') || hashParams.get('access_token') || '';
    if (!name) name = hashParams.get('name') || hashParams.get('nickname') || '';
    if (!avatar) avatar = hashParams.get('avatar') || hashParams.get('headimgurl') || '';
  }

  if (error) {
    showError('登录失败: ' + error);
    notifyParent(false, { error: error });
    return;
  }

  if (!token) {
    showError('登录参数缺失，未获取到 token');
    notifyParent(false, { error: '未获取到 token' });
    return;
  }

  // 存储登录态
  var authData = {
    type: 'ka',
    login_type: 'scan',
    name: name || '微信用户',
    avatar: avatar || ''
  };

  chrome.storage.local.set({
    ka_chatbot_token: token,
    ka_auth: authData
  }, function () {
    if (chrome.runtime.lastError) {
      var errMsg = '存储登录信息失败: ' + chrome.runtime.lastError.message;
      showError(errMsg);
      notifyParent(false, { error: errMsg });
      return;
    }

    // 通知 background service worker 登录状态变更
    chrome.runtime.sendMessage({ type: 'AUTH_STATE_CHANGED', payload: authData }, function () {
      // 忽略可能的 "receiving end does not exist" 错误
    });

    showSuccess('登录成功！');

    // 通知 popup iframe 父窗口
    notifyParent(true, { name: authData.name, avatar: authData.avatar });

    // 如果不在 iframe 中（独立 tab 打开），延迟关闭
    if (!inIframe) {
      setTimeout(function () {
        window.close();
        setTimeout(function () {
          if (subEl) subEl.textContent = '请手动关闭此标签页';
        }, 500);
      }, 1200);
    }
  });
})();

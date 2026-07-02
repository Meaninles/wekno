// background.js — Service Worker
// 存储管理 + 消息路由 + 右键菜单 + API 通信

// === WeKnora API Helper ===
// 构建请求头：API Key 使用 X-API-Key header，Bearer token 使用 Authorization header
async function buildHeaders(config) {
  var headers = {
    'Content-Type': 'application/json',
    'X-Request-ID': Date.now().toString(36) + Math.random().toString(36).slice(2, 8)
  };
  if (config.apiKey) {
    // API Key (sk- 开头) 使用 X-API-Key header 进行租户级认证
    headers['X-API-Key'] = config.apiKey;
  }
  if (config.bearerToken) {
    // Bearer token (通过用户名密码登录获取) 使用 Authorization header
    headers['Authorization'] = 'Bearer ' + config.bearerToken;
  }
  return headers;
}

async function apiRequest(method, path, body, options) {
  var config = await getConfigData();
  if (!config || !config.baseUrl) {
    return { success: false, error: '未配置服务地址，请先在设置中配置' };
  }
  var baseUrl = config.baseUrl.replace(/\/+$/, '');
  var url = baseUrl + path;
  var headers = await buildHeaders(config);
  try {
    var fetchOpts = { method: method, headers: headers };
    if (body && method !== 'GET') {
      fetchOpts.body = JSON.stringify(body);
    }
    if (options && options.signal) {
      fetchOpts.signal = options.signal;
    }
    var resp = await fetch(url, fetchOpts);
    if (!resp.ok) {
      var errText = '';
      try { var errJson = await resp.json(); errText = errJson.error?.message || errJson.message || resp.statusText; } catch (e) { errText = resp.statusText; }
      return { success: false, error: errText, status: resp.status };
    }
    var data = await resp.json();
    return data;
  } catch (err) {
    return { success: false, error: err.message || '网络请求失败' };
  }
}

// SSE streaming chat request — returns ReadableStream
async function apiChatStream(path, body) {
  var config = await getConfigData();
  if (!config || !config.baseUrl) {
    return { success: false, error: '未配置服务地址' };
  }
  var baseUrl = config.baseUrl.replace(/\/+$/, '');
  var url = baseUrl + path;
  var headers = await buildHeaders(config);
  headers['Accept'] = 'text/event-stream';
  try {
    var resp = await fetch(url, {
      method: 'POST',
      headers: headers,
      body: JSON.stringify(body),
      cache: 'no-store'  // 避免浏览器缓存导致 SSE 流被缓冲
    });
    if (!resp.ok) {
      var errText = '';
      try { var errJson = await resp.json(); errText = errJson.error?.message || errJson.message || resp.statusText; } catch (e) { errText = resp.statusText; }
      return { success: false, error: errText };
    }
    return { success: true, response: resp };
  } catch (err) {
    return { success: false, error: err.message || '网络请求失败' };
  }
}

// 流式推送消息到 sidepanel / popup 等前端页面
function notifyStream(msg) {
  chrome.runtime.sendMessage(msg).catch(function () {});
}

// Helper to get raw config data
async function getConfigData() {
  var data = await chrome.storage.local.get('ka_config');
  return data.ka_config || null;
}

async function apiRequestWithConfig(config, method, path, body, options) {
  if (!config || !config.baseUrl) {
    return { success: false, error: '未配置服务地址' };
  }
  var baseUrl = String(config.baseUrl).replace(/\/+$/, '');
  var url = baseUrl + path;
  var headers = await buildHeaders(config);
  try {
    var fetchOpts = { method: method, headers: headers };
    if (body && method !== 'GET') {
      fetchOpts.body = JSON.stringify(body);
    }
    if (options && options.signal) {
      fetchOpts.signal = options.signal;
    }
    var resp = await fetch(url, fetchOpts);
    if (!resp.ok) {
      var errText = '';
      try {
        var errJson = await resp.json();
        errText = errJson.error?.message || errJson.message || resp.statusText;
      } catch (e) {
        errText = resp.statusText;
      }
      return { success: false, error: errText, status: resp.status };
    }
    return await resp.json();
  } catch (err) {
    return { success: false, error: err.message || '网络请求失败' };
  }
}

// === chatbot.weixin.qq.com 扫码登录 API Helper ===
// 独立于 WeKnora 后端的第二条认证链路
// 扫码登录成功后，使用固定 API 地址 + Bearer token 访问接口
var SCAN_LOGIN_API_BASE = 'https://weknora.weixin.qq.com/api/v1';

// 扫码登录链路的 API 请求（固定地址 + Bearer token）
async function scanLoginApiRequest(method, path, body, options) {
  var data = await chrome.storage.local.get('ka_chatbot_token');
  var token = data.ka_chatbot_token;
  if (!token) {
    return { success: false, error: '未登录知识管理助手，请先扫码登录' };
  }
  var url = SCAN_LOGIN_API_BASE + path;
  var headers = {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer ' + token
  };
  try {
    var fetchOpts = { method: method, headers: headers };
    if (body && method !== 'GET') {
      fetchOpts.body = JSON.stringify(body);
    }
    if (options && options.signal) {
      fetchOpts.signal = options.signal;
    }
    var resp = await fetch(url, fetchOpts);
    if (!resp.ok) {
      var errText = '';
      try { var errJson = await resp.json(); errText = errJson.error?.message || errJson.message || resp.statusText; } catch (e) { errText = resp.statusText; }
      if (resp.status === 401) {
        stopTokenKeepalive();
        broadcastTokenExpired();
        return { success: false, error: '登录已过期，请重新扫码登录', expired: true };
      }
      return { success: false, error: errText, status: resp.status };
    }
    var respData = await resp.json();
    return respData;
  } catch (err) {
    return { success: false, error: err.message || '网络请求失败' };
  }
}

// 根据当前登录方式自动选择 API 链路
// 扫码登录 → scanLoginApiRequest（固定地址 + Bearer token）
// API Key 登录 → apiRequest（配置的服务地址 + X-API-Key）
async function autoApiRequest(method, path, body, options) {
  var authData = await chrome.storage.local.get(['ka_auth', 'ka_chatbot_token']);
  var auth = authData.ka_auth;
  var hasToken = !!authData.ka_chatbot_token;
  // 扫码登录：login_type 标记为 scan，或者有 chatbot token 存在
  if ((auth && auth.login_type === 'scan') || hasToken) {
    return scanLoginApiRequest(method, path, body, options);
  }
  return apiRequest(method, path, body, options);
}

// 扫码登录链路的 SSE 流式请求
async function scanLoginApiChatStream(path, body) {
  var data = await chrome.storage.local.get('ka_chatbot_token');
  var token = data.ka_chatbot_token;
  if (!token) {
    return { success: false, error: '未登录知识管理助手，请先扫码登录' };
  }
  var url = SCAN_LOGIN_API_BASE + path;
  var headers = {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer ' + token,
    'Accept': 'text/event-stream'
  };
  try {
    var resp = await fetch(url, {
      method: 'POST',
      headers: headers,
      body: JSON.stringify(body),
      cache: 'no-store'
    });
    if (!resp.ok) {
      var errText = '';
      try { var errJson = await resp.json(); errText = errJson.error?.message || errJson.message || resp.statusText; } catch (e) { errText = resp.statusText; }
      return { success: false, error: errText };
    }
    return { success: true, response: resp };
  } catch (err) {
    return { success: false, error: err.message || '网络请求失败' };
  }
}

// 自动选择 SSE 流式链路
async function autoApiChatStream(path, body) {
  var authData = await chrome.storage.local.get(['ka_auth', 'ka_chatbot_token']);
  var auth = authData.ka_auth;
  var hasToken = !!authData.ka_chatbot_token;
  if ((auth && auth.login_type === 'scan') || hasToken) {
    return scanLoginApiChatStream(path, body);
  }
  return apiChatStream(path, body);
}

// === Token 保活 & 过期检测 ===
var TOKEN_KEEPALIVE_ALARM = 'ka-token-keepalive';
var TOKEN_KEEPALIVE_INTERVAL_MIN = 10; // 每 10 分钟 ping 一次

// 启动 token 保活定时器
async function startTokenKeepalive() {
  var authData = await chrome.storage.local.get(['ka_auth', 'ka_chatbot_token']);
  if (authData.ka_auth && authData.ka_auth.login_type === 'scan' && authData.ka_chatbot_token) {
    chrome.alarms.create(TOKEN_KEEPALIVE_ALARM, {
      delayInMinutes: 1,
      periodInMinutes: TOKEN_KEEPALIVE_INTERVAL_MIN
    });
    // 启动时立即检查一次
    checkTokenHealth();
  }
}

function stopTokenKeepalive() {
  chrome.alarms.clear(TOKEN_KEEPALIVE_ALARM);
}

// 检查 token 是否仍然有效，失败则广播过期事件
async function checkTokenHealth() {
  var authData = await chrome.storage.local.get(['ka_auth', 'ka_chatbot_token']);
  if (!authData.ka_chatbot_token || !authData.ka_auth || authData.ka_auth.login_type !== 'scan') {
    stopTokenKeepalive();
    return;
  }
  var result = await scanLoginApiRequest('GET', '/auth/me');
  if (result && result.expired) {
    stopTokenKeepalive();
    broadcastTokenExpired();
  }
}

// 广播 token 过期事件到所有页面（sidepanel + popup + content scripts）
function broadcastTokenExpired() {
  chrome.runtime.sendMessage({ type: 'TOKEN_EXPIRED' }).catch(function () {});
  chrome.tabs.query({}, function (tabs) {
    tabs.forEach(function (tab) {
      if (tab.id) {
        chrome.tabs.sendMessage(tab.id, { type: 'TOKEN_EXPIRED' }).catch(function () {});
      }
    });
  });
}

chrome.alarms.onAlarm.addListener(function (alarm) {
  if (alarm.name === TOKEN_KEEPALIVE_ALARM) {
    checkTokenHealth();
  }
});

// SW 启动时检查是否需要保活
startTokenKeepalive();

// === 右键菜单 ===
// 防止并发注册
var _menuSetupInProgress = false;

function setupContextMenus() {
  if (_menuSetupInProgress) return;
  _menuSetupInProgress = true;

  chrome.contextMenus.removeAll(function () {
    void chrome.runtime.lastError; // 清除可能的 lastError

    // 保存选中文字
    chrome.contextMenus.create({
      id: 'ka-save-selection',
      title: '保存到知识管理助手',
      contexts: ['selection']
    }, function () { void chrome.runtime.lastError; });

    // 用选中文字提问
    chrome.contextMenus.create({
      id: 'ka-ask-selection',
      title: '使用知识管理助手提问',
      contexts: ['selection']
    }, function () { void chrome.runtime.lastError; });

    // 保存图片到知识管理助手
    chrome.contextMenus.create({
      id: 'ka-save-image',
      title: '保存图片到知识管理助手',
      contexts: ['image']
    }, function () { void chrome.runtime.lastError; });

    _menuSetupInProgress = false;

    // 根据登录状态更新菜单文案
    updateContextMenuTitle();
  });
}

// 插件安装/更新时注册
chrome.runtime.onInstalled.addListener(function () {
  setupContextMenus();
  // 允许 content script 访问 session storage（用于气泡禁用状态等）
  try {
    chrome.storage.session.setAccessLevel({ accessLevel: 'TRUSTED_AND_UNTRUSTED_CONTEXTS' });
  } catch (e) {}
});

// Service Worker 每次启动时也注册（确保重载后菜单存在）
setupContextMenus();

// 每次 SW 启动时也确保 session storage 访问级别正确
try {
  chrome.storage.session.setAccessLevel({ accessLevel: 'TRUSTED_AND_UNTRUSTED_CONTEXTS' });
} catch (e) {}

// 根据登录类型动态更新右键菜单中"提问"的文案
async function updateContextMenuTitle() {
  var data = await chrome.storage.local.get('ka_auth');
  var auth = data.ka_auth;
  var askTitle = '使用知识管理助手提问';
  if (auth && auth.type === 'wk') {
    askTitle = '使用 WeKnora 提问';
  } else if (auth && auth.type === 'ka') {
    askTitle = '使用知识管理助手提问';
  }
  chrome.contextMenus.update('ka-ask-selection', { title: askTitle }, function () {
    void chrome.runtime.lastError; // 菜单还未创建时静默忽略
  });
}

chrome.contextMenus.onClicked.addListener(function (info, tab) {
  // 处理图片保存（不需要 selectionText）
  if (info.menuItemId === 'ka-save-image') {
    var imgUrl = info.srcUrl;
    if (!imgUrl) return;
    var title = '图片收藏 - ' + (tab.title || '未知页面');
    var clip = {
      type: 'image-clip',
      content: '![图片](' + imgUrl + ')',
      title: title,
      meta: { url: tab.url || '', title: tab.title || '', imageUrl: imgUrl }
    };
    saveClip(clip).then(function (result) {
      if (tab && tab.id) {
        var notifMsg = '图片已保存到知识管理助手';
        if (result && result.syncedToKb && result.kbName) {
          notifMsg = '图片已保存，并同步到「' + result.kbName + '」';
        }
        chrome.tabs.sendMessage(tab.id, {
          type: 'SHOW_NOTIFICATION',
          payload: { msg: notifMsg, status: 'success' }
        }).catch(function () {});
      }
    });
    return;
  }

  if (!info.selectionText) return;

  if (info.menuItemId === 'ka-save-selection') {
    // 发送到 content.js，打开 Markdown 编辑弹窗
    if (tab && tab.id) {
      chrome.tabs.sendMessage(tab.id, {
        type: 'OPEN_EDITOR_FOR_SELECTION',
        payload: { text: info.selectionText }
      }).catch(function () {});
    }
  }

  if (info.menuItemId === 'ka-ask-selection') {
    // 打开侧边栏并将选中文字作为问题
    if (tab && tab.id) {
      chrome.storage.local.set({
        ka_pending_query: { query: info.selectionText, ts: Date.now() }
      });
      chrome.sidePanel.open({ tabId: tab.id }).catch(function () {});
    }
  }
});

chrome.runtime.onMessage.addListener(function (msg, sender, sendResponse) {
  handleMessage(msg, sender).then(function (result) {
    sendResponse(result);
  }).catch(function (err) {
    sendResponse({ success: false, error: err.message || '未知错误' });
  });
  return true;
});

chrome.runtime.onMessageExternal.addListener(function (msg, sender, sendResponse) {
  handleExternalMessage(msg, sender).then(function (result) {
    sendResponse(result);
  }).catch(function (err) {
    sendResponse({ success: false, error: err.message || '未知错误' });
  });
  return true;
});

async function handleExternalMessage(msg, sender) {
  if (!msg || typeof msg !== 'object') {
    return { success: false, error: '无效消息' };
  }

  switch (msg.type) {
    case 'WEKNORA_EXTENSION_PING':
      return getExternalInstallState(sender);
    case 'WEKNORA_EXTENSION_CONFIGURE':
      return configureFromWeKnora(msg.payload || {}, sender);
    default:
      return { success: false, error: '不支持的消息类型' };
  }
}

async function getExternalInstallState(sender) {
  var data = await chrome.storage.local.get(['ka_config', 'ka_auth']);
  var config = data.ka_config || {};
  var configured = !!(config.baseUrl && (config.apiKey || config.bearerToken));
  return {
    success: true,
    installed: true,
    configured: configured,
    tenantId: config.tenantId || '',
    tenantName: config.tenantName || '',
    baseUrl: config.baseUrl || '',
    authType: config.apiKey ? 'api_key' : (config.bearerToken ? 'bearer' : ''),
    loginType: data.ka_auth?.login_type || '',
    source: sender?.origin || sender?.url || ''
  };
}

async function configureFromWeKnora(payload, sender) {
  var baseUrl = String(payload.baseUrl || '').trim().replace(/\/+$/, '');
  var apiKey = String(payload.apiKey || '').trim();
  var tenantId = String(payload.tenantId || '').trim();
  var tenantName = String(payload.tenantName || '').trim();
  if (!baseUrl) return { success: false, error: '缺少服务地址' };
  if (!apiKey) return { success: false, error: '缺少 API Key' };

  var config = {
    baseUrl: baseUrl,
    apiKey: apiKey,
    tenantId: tenantId,
    tenantName: tenantName,
    configuredAt: new Date().toISOString(),
    configuredFrom: sender?.origin || sender?.url || 'weknora-web'
  };

  var validation = await apiRequestWithConfig(config, 'GET', '/knowledge-bases');
  if (!validation || validation.success === false) {
    return {
      success: false,
      error: validation?.error || validation?.message || '配置验证失败'
    };
  }

  await chrome.storage.local.set({
    ka_config: config,
    ka_auth: {
      type: 'wk',
      name: tenantName || 'WeKnora 用户',
      login_type: 'api_key',
      tenant_id: tenantId || ''
    }
  });
  updateContextMenuTitle();
  broadcastAuthStateChanged();
  return {
    success: true,
    configured: true,
    tenantId: tenantId,
    tenantName: tenantName,
    baseUrl: baseUrl
  };
}

async function handleMessage(msg, sender) {
  switch (msg.type) {
    case 'GET_AUTH':
      return getAuth();
    case 'SET_AUTH':
      return setAuth(msg.payload);
    case 'CLEAR_AUTH':
      return clearAuth();
    case 'GET_CONFIG':
      return getConfig();
    case 'SET_CONFIG':
      return setConfig(msg.payload);
    case 'SAVE_NOTE':
      return saveNote(msg.payload);
    case 'GET_NOTES':
      return getNotes();
    case 'SAVE_CLIP':
      return saveClip(msg.payload);
    case 'GET_CLIPS':
      return getClips();
    case 'DELETE_CLIP':
      return deleteClip(msg.payload);
    case 'DELETE_NOTE':
      return deleteNote(msg.payload);
    case 'UPDATE_CLIP':
      return updateClip(msg.payload);
    case 'UPDATE_NOTE':
      return updateNote(msg.payload);
    case 'INJECT_SCRIPT':
      return injectScript(msg.payload.tabId);
    case 'ASK_WEKNORA':
      // 打开侧边栏并传递选中的文字作为问题
      if (sender && sender.tab && sender.tab.id) {
        await chrome.sidePanel.open({ tabId: sender.tab.id });
        // 存储待处理的问题，sidepanel 加载后会读取
        await chrome.storage.local.set({
          ka_pending_query: { query: msg.payload.text, ts: Date.now() }
        });
      }
      return { success: true };
    case 'SAVE_SELECTION':
      // 从气泡/编辑弹窗保存选中文字
      return saveClip(msg.payload);
    case 'OPEN_EDITOR_FOR_SELECTION':
      // 该消息通过 chrome.tabs.sendMessage 发给 content script，
      // 不应到达此处，但为安全起见兼容处理
      return { success: true };
    case 'SAVE_IMAGE':
      // 从气泡保存图片
      return saveClip(msg.payload);
    case 'CAPTURE_SCREENSHOT':
      // 截取当前标签页可见区域
      try {
        var tabId = sender && sender.tab && sender.tab.id;
        if (!tabId) return { success: false, error: '无法获取标签页' };
        var dataUrl = await chrome.tabs.captureVisibleTab(null, { format: 'jpeg', quality: 90 });
        return { success: true, dataUrl: dataUrl };
      } catch (err) {
        return { success: false, error: err.message || '截图失败' };
      }

    // === WeKnora API 相关 ===
    case 'VALIDATE_CONFIG':
      // 通过调用知识库列表接口来验证连通性和认证
      return autoApiRequest('GET', '/knowledge-bases');

    case 'LIST_KNOWLEDGE_BASES':
      // agent_id 参数仅用于共享智能体（跨租户），本地/内置智能体不传
      var agentFilter = (msg.payload && msg.payload.sharedAgentId) ? '?agent_id=' + msg.payload.sharedAgentId : '';
      return autoApiRequest('GET', '/knowledge-bases' + agentFilter);

    case 'LIST_KB_ITEMS': {
      // 拉取指定知识库的条目列表
      var kbPayload = msg.payload || {};
      var kbId = kbPayload.kbId;
      if (!kbId) return { success: false, error: '缺少知识库 ID' };
      var page = kbPayload.page || 1;
      var pageSize = kbPayload.pageSize || 5;
      return autoApiRequest('GET', '/knowledge-bases/' + kbId + '/knowledge?page=' + page + '&page_size=' + pageSize);
    }

    case 'LIST_AGENTS':
      return autoApiRequest('GET', '/agents');

    case 'GET_SUGGESTED_QUESTIONS': {
      var sqPayload = msg.payload || {};
      var sqAgentId = sqPayload.agentId;
      if (!sqAgentId) return { success: false, error: '缺少 agentId' };
      var sqQuery = 'limit=' + (sqPayload.limit || 6);
      if (sqPayload.knowledgeBaseIds && sqPayload.knowledgeBaseIds.length > 0) {
        sqQuery += '&knowledge_base_ids=' + sqPayload.knowledgeBaseIds.join(',');
      }
      return autoApiRequest('GET', '/agents/' + sqAgentId + '/suggested-questions?' + sqQuery);
    }

    case 'CREATE_SESSION':
      return autoApiRequest('POST', '/sessions', msg.payload || {});

    case 'LIST_SESSIONS':
      var p = msg.payload || {};
      return autoApiRequest('GET', '/sessions?page=' + (p.page || 1) + '&page_size=' + (p.page_size || 20));

    case 'CLEAR_SESSION_MESSAGES': {
      var sid = (msg.payload || {}).sessionId;
      if (!sid) return { success: false, error: '缺少 sessionId' };
      return autoApiRequest('DELETE', '/sessions/' + sid + '/messages');
    }

    case 'CHAT_QUERY': {
      // 真正的知识库问答 — 使用 SSE 流式输出
      var payload = msg.payload || {};
      var query = payload.query;
      if (!query) return { success: false, error: '请输入问题' };

      // 获取或创建会话
      var sessionId = payload.sessionId;
      if (!sessionId) {
        var sessionResp = await autoApiRequest('POST', '/sessions', {});
        if (sessionResp && sessionResp.success && sessionResp.data) {
          sessionId = sessionResp.data.id;
        } else if (sessionResp && sessionResp.id) {
          sessionId = sessionResp.id;
        }
        if (!sessionId) {
          return { success: false, error: '创建会话失败: ' + (sessionResp.error || '未知错误') };
        }
        await chrome.storage.local.set({ ka_current_session: sessionId });
      }

      // 确定使用知识库问答还是智能体问答
      var kbIds = payload.knowledgeBaseIds || [];
      var agentId = payload.agentId || '';
      var useAgent = payload.agentEnabled || false;
      var chatPath = useAgent
        ? '/agent-chat/' + sessionId
        : '/knowledge-chat/' + sessionId;

      // 构建完整请求体，参考 CreateKnowledgeQARequest
      var chatBody = { query: query, channel: 'browser_extension' };
      if (kbIds.length > 0) {
        chatBody.knowledge_base_ids = kbIds;
      }
      if (agentId) {
        chatBody.agent_id = agentId;
      }
      if (useAgent) {
        chatBody.agent_enabled = true;
      }
      if (payload.webSearchEnabled) {
        chatBody.web_search_enabled = true;
      }
      if (payload.mentionedItems) {
        chatBody.mentioned_items = payload.mentionedItems;
      }
      if (payload.images && payload.images.length > 0) {
        chatBody.images = payload.images;
      }

      // 使用请求 ID 区分不同来源的流式推送
      var chatRequestId = payload._requestId || (Date.now().toString(36) + Math.random().toString(36).slice(2, 6));

      // SSE 流式请求
      var streamResult = await autoApiChatStream(chatPath, chatBody);
      if (!streamResult.success) {
        return { success: false, error: streamResult.error };
      }

      // 读取 SSE 流，逐块推送到前端
      try {
        var reader = streamResult.response.body.getReader();
        var decoder = new TextDecoder();
        var fullText = '';
        var buffer = '';

        while (true) {
          var readResult = await reader.read();
          if (readResult.done) break;
          buffer += decoder.decode(readResult.value, { stream: true });

          // SSE 格式: "event: message\ndata: {json}\n\n"
          // 按双换行分割完整事件块
          var eventBlocks = buffer.split('\n\n');
          buffer = eventBlocks.pop() || '';

          for (var bi = 0; bi < eventBlocks.length; bi++) {
            var block = eventBlocks[bi].trim();
            if (!block) continue;

            // 从事件块中提取 data 行
            var dataLine = '';
            var blockLines = block.split('\n');
            for (var li = 0; li < blockLines.length; li++) {
              var bline = blockLines[li];
              if (bline.startsWith('data:')) {
                dataLine = bline.substring(5).trim();
              }
            }
            if (!dataLine || dataLine === '[DONE]') continue;

            try {
              var evt = JSON.parse(dataLine);
              var responseType = evt.response_type || '';

              // 根据 response_type 处理不同事件
              if (responseType === 'answer') {
                var chunk = evt.content || '';
                if (chunk) {
                  fullText += chunk;
                  notifyStream({
                    type: 'CHAT_STREAM_CHUNK',
                    payload: { requestId: chatRequestId, sessionId: sessionId, responseType: 'answer', content: chunk, done: !!evt.done }
                  });
                }
              } else if (responseType === 'thinking') {
                notifyStream({
                  type: 'CHAT_STREAM_CHUNK',
                  payload: {
                    requestId: chatRequestId,
                    sessionId: sessionId,
                    responseType: 'thinking',
                    content: evt.content || '',
                    eventId: evt.data && evt.data.event_id,
                    toolData: evt.data || null,
                    timestamp: evt.timestamp || Date.now()
                  }
                });
              } else if (responseType === 'tool_call') {
                notifyStream({
                  type: 'CHAT_STREAM_CHUNK',
                  payload: {
                    requestId: chatRequestId,
                    sessionId: sessionId,
                    responseType: 'tool_call',
                    content: evt.content || '',
                    toolName: evt.data && evt.data.tool_name,
                    eventId: evt.data && (evt.data.event_id || evt.data.tool_call_id),
                    toolCallId: evt.data && evt.data.tool_call_id,
                    arguments: evt.data && evt.data.arguments,
                    toolData: evt.data || null,
                    displayType: evt.display_type || (evt.data && evt.data.display_type) || '',
                    timestamp: evt.timestamp || Date.now()
                  }
                });
              } else if (responseType === 'tool_result') {
                notifyStream({
                  type: 'CHAT_STREAM_CHUNK',
                  payload: {
                    requestId: chatRequestId,
                    sessionId: sessionId,
                    responseType: 'tool_result',
                    content: evt.content || '',
                    toolName: evt.data && evt.data.tool_name,
                    eventId: evt.data && (evt.data.event_id || evt.data.tool_call_id),
                    toolCallId: evt.data && evt.data.tool_call_id,
                    success: evt.data && evt.data.success,
                    arguments: evt.data && evt.data.arguments,
                    toolData: evt.data || null,
                    displayType: evt.display_type || (evt.data && evt.data.display_type) || '',
                    timestamp: evt.timestamp || Date.now()
                  }
                });
              } else if (responseType === 'references') {
                var kRefs = evt.knowledge_references;
                if (Array.isArray(kRefs) && kRefs.length > 0) {
                  notifyStream({
                    type: 'CHAT_STREAM_CHUNK',
                    payload: { requestId: chatRequestId, sessionId: sessionId, responseType: 'references', references: kRefs }
                  });
                }
              } else if (responseType === 'error') {
                notifyStream({
                  type: 'CHAT_STREAM_CHUNK',
                  payload: {
                    requestId: chatRequestId, sessionId: sessionId, responseType: 'error',
                    content: evt.content || '请求出错', done: !!evt.done,
                    toolName: evt.data && evt.data.tool_name,
                    toolCallId: evt.data && evt.data.tool_call_id
                  }
                });
              } else if (responseType === 'complete') {
                notifyStream({
                  type: 'CHAT_STREAM_CHUNK',
                  payload: { requestId: chatRequestId, sessionId: sessionId, responseType: 'complete', done: true }
                });
              }
              if (responseType === 'session_title') {
                notifyStream({
                  type: 'CHAT_STREAM_CHUNK',
                  payload: { requestId: chatRequestId, sessionId: sessionId, responseType: 'session_title', content: evt.content || '' }
                });
              }
              // agent_query 等事件静默忽略
            } catch (e) {
              // 非 JSON data 行，忽略
            }
          }
        }

        return { success: true, data: fullText || '未获取到回复内容', sessionId: sessionId, requestId: chatRequestId };
      } catch (streamErr) {
        return { success: false, error: '读取回复流失败: ' + streamErr.message };
      }
    }

    case 'SAVE_CLIP_TO_KB': {
      // 保存剪藏内容到知识库（作为手动知识条目）
      var pl = msg.payload || {};
      if (!pl.kbId || !pl.content) return { success: false, error: '缺少知识库 ID 或内容' };
      var contentWithMeta = pl.content;
      if (pl.url) {
        contentWithMeta = '> 来源: ' + pl.url + '\n\n' + pl.content;
      }
      return autoApiRequest('POST', '/knowledge-bases/' + pl.kbId + '/knowledge/manual', {
        title: pl.title || '知识管理助手剪藏',
        content: contentWithMeta,
        status: 'publish',
        channel: 'browser_extension'
      });
    }

    case 'GET_KB_KNOWLEDGE': {
      var pl = msg.payload || {};
      if (!pl.kbId || !pl.knowledgeId) return { success: false, error: '缺少知识库 ID 或知识 ID' };
      return autoApiRequest('GET', '/knowledge/' + pl.knowledgeId);
    }

    case 'UPDATE_KB_KNOWLEDGE': {
      var pl = msg.payload || {};
      if (!pl.kbId || !pl.knowledgeId) return { success: false, error: '缺少知识库 ID 或知识 ID' };
      var body = {
        channel: 'browser_extension',
        status: 'publish'
      };
      if (pl.title !== undefined) body.title = pl.title;
      if (pl.content !== undefined) body.content = pl.content;

      return autoApiRequest('PUT', '/knowledge/manual/' + pl.knowledgeId, body);
    }

    case 'FETCH_FILE': {
      // 通过 background 代理带认证头请求文件（图片等），返回 data URL
      var filePath = (msg.payload || {}).filePath;
      if (!filePath) return { success: false, error: '缺少 filePath' };

      var fileUrl, fileHeaders;
      var authCheck = await chrome.storage.local.get('ka_auth');
      if (authCheck.ka_auth && authCheck.ka_auth.login_type === 'scan') {
        // 扫码登录：用固定地址 + Bearer token
        var tokenCheck = await chrome.storage.local.get('ka_chatbot_token');
        var scanToken = tokenCheck.ka_chatbot_token;
        if (!scanToken) return { success: false, error: '未登录' };
        var scanBaseUrl = SCAN_LOGIN_API_BASE.replace(/\/api\/v\d+$/, '');
        fileUrl = scanBaseUrl + '/files?file_path=' + encodeURIComponent(filePath);
        fileHeaders = { 'Authorization': 'Bearer ' + scanToken };
      } else {
        // API Key 登录：用配置的服务地址
        var cfg = await getConfigData();
        if (!cfg || !cfg.baseUrl) return { success: false, error: '未配置服务地址' };
        var fileBaseUrl = cfg.baseUrl.replace(/\/+$/, '').replace(/\/api\/v\d+$/, '');
        fileUrl = fileBaseUrl + '/files?file_path=' + encodeURIComponent(filePath);
        fileHeaders = await buildHeaders(cfg);
      }
      try {
        var fileResp = await fetch(fileUrl, { method: 'GET', headers: fileHeaders });
        if (!fileResp.ok) return { success: false, error: 'HTTP ' + fileResp.status };
        var blob = await fileResp.blob();
        var reader2 = new FileReader();
        var dataUrl = await new Promise(function (resolve, reject) {
          reader2.onload = function () { resolve(reader2.result); };
          reader2.onerror = function () { reject(new Error('FileReader error')); };
          reader2.readAsDataURL(blob);
        });
        return { success: true, dataUrl: dataUrl };
      } catch (err) {
        return { success: false, error: err.message || '文件请求失败' };
      }
    }

    case 'GET_USER_INFO':
      return autoApiRequest('GET', '/auth/me');

    // === chatbot.weixin.qq.com API（扫码登录链路） ===
    case 'AUTH_STATE_CHANGED':
      updateContextMenuTitle();
      broadcastAuthStateChanged();
      return { success: true };

    default:
      return { success: false, error: '未知消息类型' };
  }
}

// === Auth ===
async function getAuth() {
  var data = await chrome.storage.local.get('ka_auth');
  return { success: true, data: data.ka_auth || null };
}

// 广播状态变更到所有 tab 的 content script
function broadcastAuthStateChanged() {
  chrome.tabs.query({}, function (tabs) {
    tabs.forEach(function (tab) {
      if (tab.id) {
        chrome.tabs.sendMessage(tab.id, { type: 'AUTH_STATE_CHANGED' }).catch(function () {});
      }
    });
  });
}

async function setAuth(auth) {
  await chrome.storage.local.set({ ka_auth: auth });
  updateContextMenuTitle();
  broadcastAuthStateChanged();
  // 扫码登录成功后启动 token 保活
  if (auth && auth.login_type === 'scan') {
    startTokenKeepalive();
  }
  return { success: true };
}

async function clearAuth() {
  stopTokenKeepalive();
  await chrome.storage.local.remove([
    'ka_auth',
    'ka_chatbot_token',
    'ka_clips',
    'ka_notes',
    'ka_selected_agent',
    'ka_pending_query',
    'ka_open_note',
    'clipKbId',
    'clipKbName',
    'ka_sel_bubble_enabled'
  ]);
  updateContextMenuTitle();
  broadcastAuthStateChanged();
  return { success: true };
}

// === Config (WeKnora) ===
async function getConfig() {
  var data = await chrome.storage.local.get('ka_config');
  return { success: true, data: data.ka_config || { baseUrl: '', apiKey: '' } };
}

async function setConfig(config) {
  await chrome.storage.local.set({ ka_config: config });
  return { success: true };
}

// === Notes (Markdown) ===
async function saveNote(note) {
  var data = await chrome.storage.local.get('ka_notes');
  var notes = data.ka_notes || [];
  note.id = Date.now().toString();
  note.createdAt = new Date().toISOString();
  notes.unshift(note);
  if (notes.length > 100) notes = notes.slice(0, 100);
  await chrome.storage.local.set({ ka_notes: notes });
  return { success: true, data: note };
}

async function getNotes() {
  var data = await chrome.storage.local.get('ka_notes');
  return { success: true, data: data.ka_notes || [] };
}

// === Clips (网页截取收藏) ===
async function saveClip(clip) {
  try {
    var data = await chrome.storage.local.get('ka_clips');
    var clips = data.ka_clips || [];

    // 如果传入了已有 id，说明是编辑已有笔记，执行更新而非新增
    if (clip.id) {
      var found = false;
      for (var i = 0; i < clips.length; i++) {
        if (clips[i].id === clip.id) {
          // 保留原始创建时间和其他元数据，只更新内容相关字段
          clips[i].content = clip.content;
          if (clip.title) clips[i].title = clip.title;
          if (clip.type) clips[i].type = clip.type;
          clips[i].updatedAt = new Date().toISOString();
          clip = clips[i]; // 返回完整的更新后记录
          found = true;
          break;
        }
      }
      // 如果在 ka_clips 中没找到，再到 ka_notes 中查找并更新
      if (!found) {
        var notesData = await chrome.storage.local.get('ka_notes');
        var notes = notesData.ka_notes || [];
        for (var j = 0; j < notes.length; j++) {
          if (notes[j].id === clip.id) {
            notes[j].content = clip.content;
            if (clip.title) notes[j].title = clip.title;
            if (clip.type) notes[j].type = clip.type;
            notes[j].updatedAt = new Date().toISOString();
            clip = notes[j];
            found = true;
            await chrome.storage.local.set({ ka_notes: notes });
            break;
          }
        }
      }
      if (found) {
        await chrome.storage.local.set({ ka_clips: clips });
        // 编辑已有记录也同步到知识库
        var editSyncResult = await syncClipToKb(clip);
        return { success: true, data: clip, syncedToKb: editSyncResult.synced, kbName: editSyncResult.kbName, syncError: editSyncResult.error || '' };
      }
      // 没找到原记录，当作新建处理（fallthrough）
    }

    // 新建记录
    clip.id = Date.now().toString();
    clip.createdAt = new Date().toISOString();
    clips.unshift(clip);
    if (clips.length > 200) clips = clips.slice(0, 200);
    await chrome.storage.local.set({ ka_clips: clips });

    // 自动同步到用户选中的知识库
    var syncResult = await syncClipToKb(clip);

    return { success: true, data: clip, syncedToKb: syncResult.synced, kbName: syncResult.kbName, syncError: syncResult.error || '' };
  } catch (err) {
    // 如果保存失败（可能是截图太大），尝试去掉截图再保存
    if (clip.screenshot) {
      try {
        delete clip.screenshot;
        var data2 = await chrome.storage.local.get('ka_clips');
        var clips2 = data2.ka_clips || [];
        clips2.unshift(clip);
        if (clips2.length > 200) clips2 = clips2.slice(0, 200);
        await chrome.storage.local.set({ ka_clips: clips2 });
        // 去掉截图后也尝试同步到知识库
        var syncResult2 = await syncClipToKb(clip);
        return { success: true, data: clip, warning: '截图过大已省略，仅保存文字', syncedToKb: syncResult2.synced, kbName: syncResult2.kbName, syncError: syncResult2.error || '' };
      } catch (err2) {
        return { success: false, error: '保存失败: ' + (err2.message || '存储空间不足') };
      }
    }
    return { success: false, error: '保存失败: ' + (err.message || '未知错误') };
  }
}

// 自动同步剪藏内容到用户选中的知识库
async function syncClipToKb(clip) {
  try {
    var kbData = await chrome.storage.local.get(['clipKbId', 'clipKbName']);
    var kbId = kbData.clipKbId;
    var kbName = kbData.clipKbName || '';

    if (!kbId) {
      return { synced: false, kbName: '' };
    }

    // 构建要保存到知识库的内容（去掉截图数据，只同步文本）
    var contentForKb = clip.content || '';
    var clipTypeMarker = '';
    if (clip && clip.type) {
      clipTypeMarker = '<!-- weknora-clip-type:' + clip.type + ' -->\n';
    }
    if (clip.meta && clip.meta.url) {
      contentForKb = '> 来源: ' + clip.meta.url + '\n\n' + contentForKb;
    }
    if (clipTypeMarker) {
      contentForKb = clipTypeMarker + contentForKb;
    }

    var kbResp = await autoApiRequest('POST', '/knowledge-bases/' + kbId + '/knowledge/manual', {
      title: clip.title || '知识管理助手剪藏',
      content: contentForKb,
      status: 'publish',
      channel: 'browser_extension'
    });

    if (kbResp && kbResp.success !== false && !kbResp.error) {
      var knowledgeId = (kbResp.data && kbResp.data.id) || '';
      // 将知识 ID 和知识库 ID 写回 clip 记录
      if (knowledgeId && clip.id) {
        try {
          var stored = await chrome.storage.local.get('ka_clips');
          var allClips = stored.ka_clips || [];
          for (var ci = 0; ci < allClips.length; ci++) {
            if (allClips[ci].id === clip.id) {
              allClips[ci].knowledgeId = knowledgeId;
              allClips[ci].knowledgeBaseId = kbId;
              break;
            }
          }
          await chrome.storage.local.set({ ka_clips: allClips });
        } catch (e) {}
      }
      return { synced: true, kbName: kbName, knowledgeId: knowledgeId };
    } else {
      return { synced: false, kbName: kbName, error: (kbResp && kbResp.error) || '同步失败' };
    }
  } catch (e) {
    return { synced: false, kbName: '', error: e.message };
  }
}

async function getClips() {
  var data = await chrome.storage.local.get('ka_clips');
  return { success: true, data: data.ka_clips || [] };
}

async function deleteClip(payload) {
  var data = await chrome.storage.local.get('ka_clips');
  var clips = data.ka_clips || [];
  clips = clips.filter(function (c) { return c.id !== payload.id; });
  await chrome.storage.local.set({ ka_clips: clips });
  return { success: true };
}

async function deleteNote(payload) {
  var data = await chrome.storage.local.get('ka_notes');
  var notes = data.ka_notes || [];
  notes = notes.filter(function (n) { return n.id !== payload.id; });
  await chrome.storage.local.set({ ka_notes: notes });
  return { success: true };
}

async function updateClip(payload) {
  var data = await chrome.storage.local.get('ka_clips');
  var clips = data.ka_clips || [];
  var found = false;
  for (var i = 0; i < clips.length; i++) {
    if (clips[i].id === payload.id) {
      clips[i].content = payload.content;
      if (payload.title) clips[i].title = payload.title;
      clips[i].updatedAt = new Date().toISOString();
      found = true;
      break;
    }
  }
  if (!found) return { success: false, error: '未找到对应记录' };
  await chrome.storage.local.set({ ka_clips: clips });
  return { success: true };
}

async function updateNote(payload) {
  var data = await chrome.storage.local.get('ka_notes');
  var notes = data.ka_notes || [];
  var found = false;
  for (var i = 0; i < notes.length; i++) {
    if (notes[i].id === payload.id) {
      notes[i].content = payload.content;
      if (payload.title) notes[i].title = payload.title;
      notes[i].updatedAt = new Date().toISOString();
      found = true;
      break;
    }
  }
  if (!found) return { success: false, error: '未找到对应记录' };
  await chrome.storage.local.set({ ka_notes: notes });
  return { success: true };
}

// === Inject content script ===
async function injectScript(tabId) {
  try {
    await chrome.scripting.executeScript({ target: { tabId: tabId }, files: ['defuddle.js', 'content.js'] });
    await chrome.scripting.insertCSS({ target: { tabId: tabId }, files: ['content.css'] });
    return { success: true };
  } catch (e) {
    return { success: false, error: e.message };
  }
}

// === Commands ===
chrome.commands.onCommand.addListener(async function (cmd, tab) {
  if (!tab || !tab.id) return;
  if (cmd === 'open-sidepanel') {
    await chrome.sidePanel.open({ tabId: tab.id });
  }
  if (cmd === 'quick-ask') {
    await chrome.sidePanel.open({ tabId: tab.id });
  }
  if (cmd === 'select-clip') {
    // 快捷键触发选择剪藏：检查 content script 是否已注入
    try {
      await chrome.tabs.sendMessage(tab.id, { type: 'SELECT_CLIP' });
    } catch (e) {
      // content script 未注入（初次安装后未刷新页面），提示用户刷新
      chrome.tabs.sendMessage(tab.id, {
        type: 'SHOW_NOTIFICATION',
        payload: { msg: '请先刷新当前页面，再使用剪藏功能', status: 'error' }
      }).catch(function () {
        // 连通知都发不出去（页面完全没有 content script），尝试注入一个最小提示
        try {
          chrome.scripting.executeScript({
            target: { tabId: tab.id },
            func: function () { alert('插件提示：请刷新当前页面后再使用剪藏功能'); }
          });
        } catch (ignore) {}
      });
    }
  }
  if (cmd === 'quick-note') {
    // 快捷键触发快速笔记
    try {
      await chrome.tabs.sendMessage(tab.id, { type: 'QUICK_NOTE' });
    } catch (e) {
      chrome.tabs.sendMessage(tab.id, {
        type: 'SHOW_NOTIFICATION',
        payload: { msg: '请先刷新当前页面，再使用此功能', status: 'error' }
      }).catch(function () {
        try {
          chrome.scripting.executeScript({
            target: { tabId: tab.id },
            func: function () { alert('插件提示：请刷新当前页面后再使用此功能'); }
          });
        } catch (ignore) {}
      });
    }
  }
  if (cmd === 'smart-clip') {
    // 快捷键触发智能剪藏
    try {
      await chrome.tabs.sendMessage(tab.id, { type: 'SMART_CLIP' });
    } catch (e) {
      chrome.tabs.sendMessage(tab.id, {
        type: 'SHOW_NOTIFICATION',
        payload: { msg: '请先刷新当前页面，再使用剪藏功能', status: 'error' }
      }).catch(function () {
        try {
          chrome.scripting.executeScript({
            target: { tabId: tab.id },
            func: function () { alert('插件提示：请刷新当前页面后再使用剪藏功能'); }
          });
        } catch (ignore) {}
      });
    }
  }
});

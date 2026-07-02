(function () {
  'use strict';

  // === State ===
  var currentUser = null; // { type: 'ka'|'wk', name, avatar, badge }
  var clipKbId = '';      // 剪藏目标知识库 ID
  var clipKbName = '';    // 剪藏目标知识库名称

  // === DOM Helpers ===
  function $(id) { return document.getElementById(id); }

  function goPage(id) {
    document.querySelectorAll('.page').forEach(function (p) { p.classList.remove('active'); });
    $(id).classList.add('active');
  }

  function toast(msg, type) {
    var el = $('toast');
    el.textContent = msg;
    el.className = 'toast show' + (type ? ' ' + type : '');
    setTimeout(function () { el.classList.remove('show'); }, 2000);
  }

  // === Chrome Storage Helpers ===
  function sendMsg(data) {
    return new Promise(function (resolve) {
      if (chrome && chrome.runtime && chrome.runtime.sendMessage) {
        try {
          chrome.runtime.sendMessage(data, function (resp) {
            // 静默处理 lastError，防止 Chrome 报错
            void chrome.runtime.lastError;
            resolve(resp || { success: false });
          });
        } catch (e) {
          resolve({ success: false, error: e.message });
        }
      } else {
        resolve({ success: false, error: 'no chrome api' });
      }
    });
  }

  // 卡通头像
  var kaAvatarUrl = "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 120 120'%3E%3Cdefs%3E%3ClinearGradient id='bg' x1='0' y1='0' x2='1' y2='1'%3E%3Cstop offset='0%25' stop-color='%2307C160'/%3E%3Cstop offset='100%25' stop-color='%2306ad54'/%3E%3C/linearGradient%3E%3C/defs%3E%3Crect fill='url(%23bg)' width='120' height='120' rx='60'/%3E%3Ccircle cx='60' cy='48' r='22' fill='%23fff'/%3E%3Ccircle cx='52' cy='44' r='3' fill='%2307C160'/%3E%3Ccircle cx='68' cy='44' r='3' fill='%2307C160'/%3E%3Cpath d='M54 54 Q60 60 66 54' stroke='%2307C160' stroke-width='2.5' fill='none' stroke-linecap='round'/%3E%3Cellipse cx='60' cy='90' rx='28' ry='20' fill='%23fff'/%3E%3C/svg%3E";

  // === 密码可见性切换 ===
  function initEyeToggle(btnId, inputId) {
    var btn = $(btnId);
    var input = $(inputId);
    if (!btn || !input) return;
    btn.addEventListener('click', function () {
      var isVisible = btn.classList.toggle('visible');
      input.type = isVisible ? 'text' : 'password';
    });
  }
  initEyeToggle('btn-eye-wk', 'wk-key');

  // === Page Navigation ===

  // 官网链接 — 用 chrome.tabs.create 打开
  $('link-ka-site').addEventListener('click', function (e) {
    e.preventDefault();
    chrome.tabs.create({ url: this.href });
  });
  $('link-wk-site').addEventListener('click', function (e) {
    e.preventDefault();
    chrome.tabs.create({ url: this.href });
  });

  // 知识管理助手 → 进入扫码登录页
  $('btn-ka').addEventListener('click', function () {
    goPage('pg-ka-login');
    initQrCode();
  });

  // 返回首页
  $('btn-ka-back').addEventListener('click', function () {
    goPage('pg-login');
  });

  // === 微信扫码登录 ===
  var CHATBOT_BASE = 'https://chatbot.weixin.qq.com';
  var qrPollTimer = null; // 轮询定时器
  var qrExpireTimer = null; // 过期定时器

  // 生成 12 位随机 hex 字符串（与 chatbot 平台一致）
  function randomLoginCode(len) {
    var chars = 'abcdef0123456789';
    var s = '';
    for (var i = 0; i < (len || 12); i++) {
      s += chars.charAt(Math.floor(Math.random() * chars.length));
    }
    return s;
  }

  function initQrCode() {
    var canvasEl = $('qr-canvas');
    var loading = $('qr-loading');
    var statusEl = $('qr-status');
    var statusText = $('qr-status-text');
    var refreshBtn = $('btn-qr-refresh');
    var qrBox = $('qr-box');

    // 清理上一轮的定时器
    if (qrPollTimer) { clearTimeout(qrPollTimer); qrPollTimer = null; }
    if (qrExpireTimer) { clearTimeout(qrExpireTimer); qrExpireTimer = null; }

    // 重置状态
    statusEl.className = 'qr-status';
    statusText.textContent = '等待扫码…';
    refreshBtn.style.display = 'none';
    canvasEl.innerHTML = '';
    loading.style.display = 'flex';

    // 移除过期遮罩
    var oldMask = qrBox.querySelector('.qr-expired-mask');
    if (oldMask) oldMask.remove();

    // 生成 logincode
    var loginCode = randomLoginCode(12);
    var qrUrl = CHATBOT_BASE + '/weixin/login?logincode=' + loginCode;

    // 生成二维码
    loading.style.display = 'none';
    try {
      new QRCode(canvasEl, {
        text: qrUrl,
        width: 180,
        height: 180,
        colorDark: '#000000',
        colorLight: '#ffffff',
        correctLevel: QRCode.CorrectLevel.M
      });
    } catch (e) {
      statusText.textContent = '二维码生成失败';
      return;
    }

    // 180 秒后过期
    qrExpireTimer = setTimeout(function () {
      showExpired();
    }, 180000);

    // 开始轮询登录状态
    pollLoginStatus(loginCode);

    function pollLoginStatus(code) {
      qrPollTimer = setTimeout(function () {
        fetch(CHATBOT_BASE + '/get/login/status?logincode=' + code + '&source=ext')
          .then(function (resp) { return resp.json(); })
          .then(function (data) {
            // data.scan === 1 → 已扫码；data.logged === 1 → 登录成功（带 token）
            if (data.logged === 1) {
              onLoginSuccess(data);
              return;
            }
            if (data.scan && data.scan === 1) {
              statusEl.className = 'qr-status scanned';
              statusText.textContent = '扫码成功，请在手机上确认…';
            }
            // 继续轮询
            pollLoginStatus(code);
          })
          .catch(function () {
            // 网络错误，继续重试
            pollLoginStatus(code);
          });
      }, 1200);
    }

    function onLoginSuccess(data) {
      if (qrExpireTimer) { clearTimeout(qrExpireTimer); qrExpireTimer = null; }
      statusEl.className = 'qr-status scanned';
      statusText.textContent = '登录成功，正在获取用户信息…';

      var token = data.token || '';
      if (!token) {
        statusText.textContent = '登录异常：未获取到 token';
        statusEl.className = 'qr-status';
        toast('登录失败：服务端未返回 token', 'error');
        refreshBtn.style.display = '';
        return;
      }

      // 先存 token 和基本 auth（login_type: 'scan'），后续请求才能带上 Bearer token
      var authData = {
        type: 'ka',
        login_type: 'scan',
        name: '微信用户',
        avatar: ''
      };

      chrome.storage.local.set({
        ka_chatbot_token: token,
        ka_auth: authData
      }, function () {
        sendMsg({ type: 'SET_AUTH', payload: authData });
        sendMsg({ type: 'AUTH_STATE_CHANGED', payload: authData });

        // 用 token 调 /auth/me 获取真实用户信息
        sendMsg({ type: 'GET_USER_INFO' }).then(function (resp) {
          var name = '微信用户';
          var avatar = '';
          if (resp && resp.success && resp.data) {
            var user = resp.data.user || resp.data;
            name = user.username || user.name || user.nickname || name;
            avatar = user.avatar || user.headimgurl || avatar;
          }
          // 更新 auth 存储中的用户信息
          authData.name = name;
          authData.avatar = avatar;
          chrome.storage.local.set({ ka_auth: authData });
          sendMsg({ type: 'SET_AUTH', payload: authData });

          statusText.textContent = '登录成功！';
          setTimeout(function () {
            enterMain('ka', name, avatar || kaAvatarUrl, '知识管理助手');
            toast('登录成功', 'success');
          }, 300);
        });
      });
    }

    function showExpired() {
      if (qrPollTimer) { clearTimeout(qrPollTimer); qrPollTimer = null; }

      var mask = document.createElement('div');
      mask.className = 'qr-expired-mask';
      mask.innerHTML = '<span>二维码已过期</span><button>点击刷新</button>';
      mask.querySelector('button').addEventListener('click', function () {
        initQrCode();
      });
      qrBox.appendChild(mask);
      refreshBtn.style.display = '';
    }
  }

  // popup 关闭时清理定时器
  window.addEventListener('unload', function () {
    if (qrPollTimer) clearTimeout(qrPollTimer);
    if (qrExpireTimer) clearTimeout(qrExpireTimer);
  });

  $('btn-qr-refresh').addEventListener('click', function () {
    initQrCode();
  });

  // WeKnora 默认服务地址
  var WK_DEFAULT_URL = 'https://your-weknora-domain/api/v1';

  // WeKnora 配置页
  $('btn-wk').addEventListener('click', function () {
    goPage('pg-weknora');
    // 读取已有配置，若无则自动填入默认服务地址
    sendMsg({ type: 'GET_CONFIG' }).then(function (resp) {
      if (resp && resp.success && resp.data) {
        $('wk-url').value = resp.data.baseUrl || WK_DEFAULT_URL;
        $('wk-key').value = resp.data.apiKey || '';
      } else {
        $('wk-url').value = WK_DEFAULT_URL;
      }
    });
  });

  // WeKnora 返回
  $('btn-wk-back').addEventListener('click', function () {
    goPage('pg-login');
  });

  // WeKnora 配置指南 — 打开独立指南页面
  $('wk-help-link').addEventListener('click', function (e) {
    e.preventDefault();
    chrome.tabs.create({ url: chrome.runtime.getURL('guide.html') });
  });

  // WeKnora 保存并登录
  $('btn-wk-save').addEventListener('click', function () {
    var url = $('wk-url').value.trim();
    var key = $('wk-key').value.trim();
    if (!url) {
      $('wk-test-msg').textContent = '请填写服务地址';
      $('wk-test-msg').className = 'wk-test-result err';
      return;
    }
    if (!key) {
      $('wk-test-msg').textContent = '请填写 API Key';
      $('wk-test-msg').className = 'wk-test-result err';
      return;
    }
    $('wk-test-msg').textContent = '正在验证…';
    $('wk-test-msg').className = 'wk-test-result';
    // 保存配置
    sendMsg({ type: 'SET_CONFIG', payload: { baseUrl: url, apiKey: key } }).then(function () {
      // 验证连接
      return sendMsg({ type: 'VALIDATE_CONFIG' });
    }).then(function (resp) {
      if (resp && resp.success) {
        // API Key 验证成功
        sendMsg({ type: 'SET_AUTH', payload: { type: 'wk', name: 'WeKnora 用户', avatar: '' } });
        $('wk-test-msg').textContent = '验证通过，配置已保存';
        $('wk-test-msg').className = 'wk-test-result ok';
        setTimeout(function () {
          enterMain('wk', 'WeKnora 用户', '', 'WeKnora');
        }, 500);
      } else {
        $('wk-test-msg').textContent = '验证未通过，请检查服务地址和 API Key';
        $('wk-test-msg').className = 'wk-test-result err';
      }
    });
  });

  // === 更新用户信息显示 ===
  function updateUserDisplay(type, name, avatarUrl, badge) {
    if (avatarUrl) {
      $('main-avatar-img').src = avatarUrl;
      $('main-avatar-img').style.display = 'block';
      $('main-letter').style.display = 'none';
    } else {
      $('main-avatar-img').style.display = 'none';
      $('main-letter').style.display = 'flex';
      $('main-letter').textContent = (name || 'U').charAt(0).toUpperCase();
      $('main-letter').style.background = '#07C160';
    }
    $('main-name').textContent = name;
  }

  function getClipKbSelectionStorageKey() {
    if (!currentUser || !currentUser.type) return '';
    var name = (currentUser.name || '').trim();
    return name ? (currentUser.type + '::' + name) : currentUser.type;
  }

  function loadClipKbSelectionForCurrentUser(callback) {
    chrome.storage.local.get(['clipKbId', 'clipKbName', 'clipKbSelections', 'ka_kb_items_cache'], function (data) {
      var selectionMap = data.clipKbSelections || {};
      var key = getClipKbSelectionStorageKey();
      var accountSelection = key ? selectionMap[key] : null;
      callback({
        kbId: (accountSelection && accountSelection.kbId) || data.clipKbId || '',
        kbName: (accountSelection && accountSelection.kbName) || data.clipKbName || '',
        cache: data.ka_kb_items_cache,
        selectionMap: selectionMap,
        storageKey: key
      });
    });
  }

  function persistClipKbSelection(kbId, kbName, callback) {
    chrome.storage.local.get(['clipKbSelections'], function (data) {
      var selectionMap = data.clipKbSelections || {};
      var key = getClipKbSelectionStorageKey();
      if (key) {
        if (kbId) selectionMap[key] = { kbId: kbId, kbName: kbName || '' };
        else delete selectionMap[key];
      }
      chrome.storage.local.set({
        clipKbId: kbId || '',
        clipKbName: kbName || '',
        clipKbSelections: selectionMap
      }, function () {
        if (callback) callback();
      });
    });
  }

  function syncActiveClipKbSelection(kbId, kbName, callback) {
    chrome.storage.local.set({
      clipKbId: kbId || '',
      clipKbName: kbName || ''
    }, function () {
      chrome.tabs.query({}, function (tabs) {
        tabs.forEach(function (t) {
          if (t.id) chrome.tabs.sendMessage(t.id, { type: 'AUTH_STATE_CHANGED' }).catch(function () {});
        });
        if (callback) callback();
      });
    });
  }

  // === 进入主界面 ===
  function enterMain(type, name, avatarUrl, badge) {
    currentUser = { type: type, name: name, avatar: avatarUrl, badge: badge };
    updateUserDisplay(type, name, avatarUrl, badge);

    // 防闪烁辅助：跟踪多个异步任务，全部完成后才 reveal
    function createReadyGate(total, onReady) {
      var count = 0;
      return function () {
        count++;
        if (count >= total) onReady();
      };
    }

    var mainRevealed = false;
    function revealMain() {
      if (mainRevealed) return;
      mainRevealed = true;
      var pgMain = $('pg-main');
      pgMain.classList.remove('loading');
      pgMain.classList.add('reveal');
    }

    function continueEnterMain() {
      loadClipKbSelectionForCurrentUser(function (data) {
        var hasKb = !!data.kbId;
        clipKbId = data.kbId;
        clipKbName = data.kbName || '';
        updateClipKbDisplay();
        syncActiveClipKbSelection(clipKbId, clipKbName, function () {
          if (hasKb) {
            // 需要等待 2 个异步任务: (1)缓存/列表渲染 (2)智能体加载
            var markReady = createReadyGate(2, revealMain);

            // 先让 pg-main 以 loading 态显示（子元素不可见，占位不闪）
            $('pg-main').classList.add('loading');
            goPage('pg-main');
            // 安全超时：800ms 后如果异步任务还没全部完成，强制 reveal 防止白屏
            setTimeout(revealMain, 800);

            // 任务1: 缓存渲染
            var cache = data.cache;
            if (cache && cache.kbId === clipKbId && cache.items && cache.items.length > 0) {
              renderKbItemsFromCache(cache.items, cache.total || 0, function () {
                loadKbItems();
                markReady();
              });
            } else {
              loadKbItems();
              markReady();
            }

            // 任务2: 智能体加载（含图片按钮显隐）
            loadAgentsForDropdown(markReady);
          } else {
            goPage('pg-select-kb');
            loadAgentsForDropdown();
          }
          loadShortcutTips();
        });
      });
    }

    sendMsg({ type: 'GET_USER_INFO' }).then(function (resp) {
      if (resp && resp.success && resp.data) {
        var user = resp.data.user || resp.data;
        var realName = user.username || user.name || name;
        var realAvatar = user.avatar || avatarUrl;
        currentUser.name = realName;
        currentUser.avatar = realAvatar;
        sendMsg({ type: 'SET_AUTH', payload: { type: type, name: realName, avatar: realAvatar } });
        updateUserDisplay(type, realName, realAvatar, badge);
      }
    }).catch(function () {
    }).then(function () {
      continueEnterMain();
    });
  }

  // === 读取快捷键并更新按钮 tooltip ===
  function loadShortcutTips() {
    if (!chrome.commands || !chrome.commands.getAll) return;
    chrome.commands.getAll(function (commands) {
      var map = { 'select-clip': 'btn-select-clip', 'smart-clip': 'btn-smart-clip', 'quick-note': 'btn-quick-note' };
      var labels = { 'select-clip': '选择剪藏', 'smart-clip': '智能剪藏', 'quick-note': '速记' };
      commands.forEach(function (cmd) {
        if (map[cmd.name] && cmd.shortcut) {
          var btn = $(map[cmd.name]);
          if (btn) {
            btn.setAttribute('data-tip', labels[cmd.name] + '  ' + cmd.shortcut);
          }
        }
      });
    });
  }

  // === 从 API 加载知识库列表到下拉菜单 ===
  function loadKnowledgeBasesForDropdown() {
    sendMsg({ type: 'LIST_KNOWLEDGE_BASES' }).then(function (resp) {
      if (resp && resp.success && resp.data) {
        var kbList = Array.isArray(resp.data) ? resp.data : (resp.data.items || []);
        popupAllKbs = kbList;
        filterAndRenderPopupKbs();
        // 检查剪藏知识库选择
        checkClipKbSelection(kbList);
      }
    });
  }

  var popupAllKbs = [];
  var popupAgents = [];

  // === 根据当前选中智能体配置过滤知识库 ===
  function filterAndRenderPopupKbs() {
    var filtered = popupAllKbs;
    var agent = null;
    for (var i = 0; i < popupAgents.length; i++) {
      if (popupAgents[i].id === popupAgentId) { agent = popupAgents[i]; break; }
    }
    if (agent && agent.config) {
      var mode = agent.config.kb_selection_mode || 'all';
      if (mode === 'none') {
        filtered = [];
      } else if (mode === 'selected' && agent.config.knowledge_bases) {
        var allowedIds = agent.config.knowledge_bases;
        filtered = popupAllKbs.filter(function (kb) {
          return allowedIds.indexOf(kb.id) !== -1;
        });
      }
    }
    renderKbDropdownItems(filtered);
  }

  // === 从 API 加载智能体列表到模式下拉 ===
  function loadAgentsForDropdown(onReady) {
    sendMsg({ type: 'LIST_AGENTS' }).then(function (resp) {
      if (resp && resp.success && resp.data) {
        popupAgents = Array.isArray(resp.data) ? resp.data : (resp.data.data || []);
        if (popupAgents.length > 0) {
          // 恢复持久化的模式选择
          chrome.storage.local.get('ka_selected_agent', function (stored) {
            if (!popupAgentId && stored && stored.ka_selected_agent && stored.ka_selected_agent.agentId) {
              var saved = stored.ka_selected_agent;
              for (var i = 0; i < popupAgents.length; i++) {
                if (popupAgents[i].id === saved.agentId) {
                  popupAgentId = saved.agentId;
                  popupAgentEnabled = !!saved.agentEnabled;
                  break;
                }
              }
            }
            if (!popupAgentId) {
              popupAgentId = popupAgents[0].id;
              var isQA = popupAgents[0].id === 'builtin-quick-answer' || (popupAgents[0].config && popupAgents[0].config.agent_mode === 'quick-answer');
              popupAgentEnabled = !isQA;
            }
            // 设置当前选中智能体的图片上传能力
            for (var j = 0; j < popupAgents.length; j++) {
              if (popupAgents[j].id === popupAgentId) {
                popupAgentImageUpload = !!(popupAgents[j].config && popupAgents[j].config.image_upload_enabled);
                break;
              }
            }
            popupUpdateImageUI();
            renderAgentModeItems(popupAgents);
            if (typeof onReady === 'function') onReady();
            // 加载全部知识库（前端根据智能体配置过滤）
            loadKnowledgeBasesForDropdown();
          });
        } else {
          if (typeof onReady === 'function') onReady();
          loadKnowledgeBasesForDropdown();
        }
      } else {
        if (typeof onReady === 'function') onReady();
        loadKnowledgeBasesForDropdown();
      }
    }).catch(function () {
      if (typeof onReady === 'function') onReady();
    });
  }

  function renderAgentModeItems(agentList) {
    var modeMenu = $('popup-mode-menu');
    if (!modeMenu) return;
    // 移除旧的模式选项
    var oldItems = modeMenu.querySelectorAll('.kb-mode-item');
    oldItems.forEach(function (item) { item.remove(); });

    agentList.forEach(function (agent, idx) {
      var item = document.createElement('div');
      var isQA = agent.id === 'builtin-quick-answer' || (agent.config && agent.config.agent_mode === 'quick-answer');
      var isSelected = popupAgentId ? (agent.id === popupAgentId) : (idx === 0);
      item.className = 'kb-mode-item' + (isSelected ? ' selected' : '');
      item.setAttribute('data-agent-id', agent.id);
      item.innerHTML = '<span class="kb-radio"></span> ' + (function (s) {
        var d = document.createElement('div'); d.textContent = s; return d.innerHTML;
      })(agent.name);
      if (isSelected) {
        $('mode-name').textContent = agent.name;
      }
      item.addEventListener('click', function (e) {
        e.stopPropagation();
        popupAgentId = agent.id;
        popupAgentEnabled = !isQA;
        modeMenu.querySelectorAll('.kb-mode-item').forEach(function (i) { i.classList.remove('selected'); });
        item.classList.add('selected');
        $('mode-name').textContent = agent.name;
        modeMenu.classList.remove('show');
        // 持久化模式选择，同步给 sidepanel / weknora
        chrome.storage.local.set({ ka_selected_agent: { agentId: agent.id, agentEnabled: !isQA } });
        // 更新图片上传按钮可见性
        popupAgentImageUpload = !!(agent.config && agent.config.image_upload_enabled);
        popupUpdateImageUI();
        if (!popupAgentImageUpload && popupPendingImages.length > 0) popupClearImages();
        // 切换智能体后根据配置过滤知识库
        filterAndRenderPopupKbs();
      });
      modeMenu.appendChild(item);
    });
  }

  function renderKbDropdownItems(kbList) {
    var kbMenu = $('kb-menu');
    if (!kbMenu) return;
    // 移除旧的知识库选项（保留分隔线、模式等）
    var oldItems = kbMenu.querySelectorAll('.kb-dropdown-item');
    oldItems.forEach(function (item) { item.remove(); });
    // 在第一个分隔线之前插入新选项
    var firstDivider = kbMenu.querySelector('.kb-dropdown-divider');
    // 插入 "全部" 选项
    var allItem = createKbDropdownItem('all', '全部知识库', true);
    kbMenu.insertBefore(allItem, firstDivider);
    // 插入真实知识库
    kbList.forEach(function (kb) {
      var item = createKbDropdownItem(kb.id, kb.name, false);
      kbMenu.insertBefore(item, firstDivider);
    });
  }

  function createKbDropdownItem(kbId, name, isSelected) {
    var div = document.createElement('div');
    div.className = 'kb-dropdown-item' + (isSelected ? ' selected' : '');
    div.setAttribute('data-kb', kbId);
    div.innerHTML = '<span class="kb-radio"></span> ' + (function (s) {
      var d = document.createElement('div');
      d.textContent = s;
      return d.innerHTML;
    })(name);
    div.addEventListener('click', function (e) {
      e.stopPropagation();
      selectedKb = kbId;
      $('kb-name').textContent = name.length > 4 ? name.substring(0, 4) : name;
      $('kb-menu').querySelectorAll('.kb-dropdown-item').forEach(function (i) { i.classList.remove('selected'); });
      div.classList.add('selected');
      $('kb-menu').classList.remove('show');
    });
    return div;
  }

  // === 加载并显示知识库条目列表 ===
  var latestClips = []; // 缓存最近笔记列表（兼容）
  function loadLatestClip() {
    // 数据来源改为 API 拉取当前知识库的条目
    loadKbItems();
  }

  // 根据笔记类型返回显示信息
  function getClipDisplayInfo(clip) {
    var title = clip.title || (clip.content || '').split('\n')[0].replace(/^#+\s*/, '').trim() || '未命名';
    var hasScreenshot = clip.type === 'select-clip' && clip.screenshot;

    if (clip.type === 'markdown') {
      return {
        title: title,
        tagText: 'Markdown',
        tagClass: 'clip-list-tag tag-markdown',
        iconClass: 'icon-markdown',
        typeLabel: 'Markdown 笔记',
        iconSvg: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="16" y1="13" x2="8" y2="13"/><line x1="16" y1="17" x2="8" y2="17"/></svg>'
      };
    } else if (clip.type === 'smart-clip') {
      return {
        title: title,
        tagText: '智能剪藏',
        tagClass: 'clip-list-tag tag-clip',
        iconClass: 'icon-clip',
        typeLabel: '网页剪藏',
        iconSvg: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2L3 14h7l-1 8 10-12h-7l1-8z"/></svg>'
      };
    } else if (clip.type === 'image-clip') {
      return {
        title: title,
        tagText: '图片',
        tagClass: 'clip-list-tag tag-image',
        iconClass: 'icon-image',
        typeLabel: '图片收藏',
        iconSvg: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8.5" cy="8.5" r="1.5"/><polyline points="21 15 16 10 5 21"/></svg>'
      };
    } else if (hasScreenshot) {
      return {
        title: title,
        tagText: '截图',
        tagClass: 'clip-list-tag tag-image',
        iconClass: 'icon-shot',
        typeLabel: '截图采集',
        iconSvg: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M9 3H5a2 2 0 0 0-2 2v4"/><path d="M15 3h4a2 2 0 0 1 2 2v4"/><path d="M21 15v4a2 2 0 0 1-2 2h-4"/><path d="M3 15v4a2 2 0 0 0 2 2h4"/><rect x="7" y="7" width="10" height="10" rx="2"/></svg>'
      };
    } else {
      return {
        title: title,
        tagText: '文本',
        tagClass: 'clip-list-tag tag-text',
        iconClass: '',
        typeLabel: '文本采集',
        iconSvg: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>'
      };
    }
  }

  // 根据 API 返回的文档类型推断显示信息（绿色 + 灰色两种风格）
  var DOC_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="16" y1="13" x2="8" y2="13"/><line x1="16" y1="17" x2="8" y2="17"/></svg>';
  var LINK_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>';
  var FILE_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>';
  var IMG_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8.5" cy="8.5" r="1.5"/><polyline points="21 15 16 10 5 21"/></svg>';
  var SMART_CLIP_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2L3 14h7l-1 8 10-12h-7l1-8z"/></svg>';
  var SCREENSHOT_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M9 3H5a2 2 0 0 0-2 2v4"/><path d="M15 3h4a2 2 0 0 1 2 2v4"/><path d="M21 15v4a2 2 0 0 1-2 2h-4"/><path d="M3 15v4a2 2 0 0 0 2 2h4"/><rect x="7" y="7" width="10" height="10" rx="2"/></svg>';

  function detectClipTypeHint(title, content, explicitType) {
    var type = (explicitType || '').toLowerCase();
    var normalizedTitle = (title || '').trim();
    var textContent = content || '';
    var markerMatch = textContent.match(/<!--\s*weknora-clip-type:([a-z-]+)\s*-->/i);
    if (!type && markerMatch && markerMatch[1]) type = markerMatch[1].toLowerCase();
    if (type) return type;
    if (/^(截图)\s*[—\-–:：]\s*/i.test(normalizedTitle) || /（截图内容）/.test(textContent)) {
      return 'select-clip';
    }
    if (/^(智能剪藏)\s*[—\-–:：]\s*/i.test(normalizedTitle) ||
        (/^>\s*来源:\s*\[[^\]]+\]\(https?:\/\/\S+\)/m.test(textContent) && /\n\n---\n来源:\s*https?:\/\/\S+/m.test(textContent))) {
      return 'smart-clip';
    }
    return '';
  }

  function getItemTypeInfo(docType, sourceUrl, title, content, clipTypeHint) {
    var dt = (docType || '').toLowerCase();
    var explicitType = detectClipTypeHint(title, content, clipTypeHint);

    if (explicitType === 'smart-clip') {
      return { tagText: '智能剪藏', tagClass: 'clip-list-tag tag-green', iconClass: 'icon-clip', iconSvg: SMART_CLIP_ICON };
    }
    if (explicitType === 'select-clip') {
      return { tagText: '截图', tagClass: 'clip-list-tag tag-gray', iconClass: 'icon-shot', iconSvg: SCREENSHOT_ICON };
    }
    if (explicitType === 'image-clip') {
      return { tagText: '图片', tagClass: 'clip-list-tag tag-gray', iconClass: '', iconSvg: IMG_ICON };
    }
    if (explicitType === 'markdown') {
      return { tagText: '', tagClass: '', iconClass: 'icon-green', iconSvg: DOC_ICON };
    }
    // 笔记 / Markdown
    if (dt === 'markdown' || dt === 'md' || dt === 'note' || dt === 'text' || dt === 'txt') {
      return { tagText: '', tagClass: '', iconClass: 'icon-green', iconSvg: DOC_ICON };
    }
    // 网页
    if (dt === 'webpage' || dt === 'web' || dt === 'url' || dt === 'html') {
      return { tagText: '网页收藏', tagClass: 'clip-list-tag tag-green', iconClass: 'icon-green', iconSvg: LINK_ICON };
    }
    // 文件
    if (dt === 'file' || dt === 'pdf' || dt === 'doc' || dt === 'docx' || dt === 'ppt' || dt === 'pptx' || dt === 'xls' || dt === 'xlsx' || dt === 'csv') {
      return { tagText: '文件', tagClass: 'clip-list-tag tag-gray', iconClass: '', iconSvg: FILE_ICON };
    }
    // 图片
    if (dt === 'image' || dt === 'img' || dt === 'png' || dt === 'jpg' || dt === 'jpeg' || dt === 'gif') {
      return { tagText: '图片', tagClass: 'clip-list-tag tag-gray', iconClass: '', iconSvg: IMG_ICON };
    }
    // 默认：有来源 → 网页收藏，无来源 → 快速笔记
    if (sourceUrl) {
      return { tagText: '网页收藏', tagClass: 'clip-list-tag tag-green', iconClass: 'icon-green', iconSvg: LINK_ICON };
    }
    return { tagText: '', tagClass: '', iconClass: 'icon-green', iconSvg: DOC_ICON };
  }

  function getKbItemTypeHintMap(kbId) {
    return new Promise(function (resolve) {
      chrome.storage.local.get(['ka_clips', 'ka_notes', 'ka_kb_item_type_hints'], function (data) {
        var map = {};
        var persisted = data && data.ka_kb_item_type_hints ? data.ka_kb_item_type_hints : {};
        Object.keys(persisted).forEach(function (id) { map[id] = persisted[id]; });
        ['ka_clips', 'ka_notes'].forEach(function (key) {
          var list = data && Array.isArray(data[key]) ? data[key] : [];
          list.forEach(function (entry) {
            if (!entry || !entry.knowledgeId || !entry.type) return;
            if (kbId && entry.kbId && entry.kbId !== kbId) return;
            map[entry.knowledgeId] = entry.type;
          });
        });
        resolve(map);
      });
    });
  }

  function persistTypeHints(hints) {
    if (!hints || Object.keys(hints).length === 0) return;
    chrome.storage.local.get(['ka_kb_item_type_hints'], function (d) {
      var existing = d && d.ka_kb_item_type_hints ? d.ka_kb_item_type_hints : {};
      Object.assign(existing, hints);
      chrome.storage.local.set({ ka_kb_item_type_hints: existing });
    });
  }

  function formatTimeShort(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    var now = new Date();
    var diff = now - d;
    if (diff < 60000) return '刚刚';
    if (diff < 3600000) return Math.floor(diff / 60000) + '分钟前';
    if (diff < 86400000) return Math.floor(diff / 3600000) + '小时前';
    if (diff < 604800000) return Math.floor(diff / 86400000) + '天前';
    return (d.getMonth() + 1) + '月' + d.getDate() + '日';
  }

  // === 对话功能 ===
  var chatInput = $('chat-input');
  var sendBtn = $('btn-chat-send');
  var selectedKb = 'all';
  var popupAgentId = '';
  var popupAgentEnabled = false;
  var popupAgentImageUpload = false;

  // 图片上传
  var popupPendingImages = [];
  var POPUP_MAX_IMAGES = 5;
  var POPUP_ALLOWED_TYPES = ['image/jpeg', 'image/png', 'image/gif', 'image/webp'];
  var POPUP_MAX_IMAGE_SIZE = 10 * 1024 * 1024;
  var popupImageInput = $('popup-image-input');
  var popupImageBtn = $('popup-image-btn');
  var popupImagePreviews = $('popup-image-previews');

  function popupAddImages(files) {
    if (!popupAgentImageUpload) return;
    for (var i = 0; i < files.length; i++) {
      if (popupPendingImages.length >= POPUP_MAX_IMAGES) { break; }
      var f = files[i];
      if (POPUP_ALLOWED_TYPES.indexOf(f.type) === -1 || f.size > POPUP_MAX_IMAGE_SIZE) continue;
      popupPendingImages.push({ file: f, preview: URL.createObjectURL(f) });
    }
    popupRenderPreviews();
  }

  function popupRemoveImage(idx) {
    if (idx >= 0 && idx < popupPendingImages.length) {
      URL.revokeObjectURL(popupPendingImages[idx].preview);
      popupPendingImages.splice(idx, 1);
    }
    popupRenderPreviews();
  }

  function popupClearImages() {
    popupPendingImages.forEach(function (img) { URL.revokeObjectURL(img.preview); });
    popupPendingImages = [];
    popupRenderPreviews();
  }

  function popupRenderPreviews() {
    if (!popupImagePreviews) return;
    if (popupPendingImages.length === 0) {
      popupImagePreviews.style.display = 'none';
      popupImagePreviews.innerHTML = '';
      return;
    }
    popupImagePreviews.style.display = 'flex';
    var html = '';
    for (var i = 0; i < popupPendingImages.length; i++) {
      html += '<div class="popup-img-item" data-idx="' + i + '">'
        + '<img class="popup-img-thumb" src="' + popupPendingImages[i].preview + '">'
        + '<span class="popup-img-remove">&times;</span></div>';
    }
    popupImagePreviews.innerHTML = html;
    popupImagePreviews.querySelectorAll('.popup-img-remove').forEach(function (btn) {
      btn.addEventListener('click', function () {
        popupRemoveImage(parseInt(btn.parentElement.getAttribute('data-idx')));
      });
    });
  }

  function popupUpdateImageUI() {
    if (popupImageBtn) popupImageBtn.style.display = popupAgentImageUpload ? '' : 'none';
    if (!popupAgentImageUpload && popupPendingImages.length > 0) popupClearImages();
  }

  function popupFileToBase64(file) {
    return new Promise(function (resolve, reject) {
      var reader = new FileReader();
      reader.onload = function () { resolve(reader.result); };
      reader.onerror = reject;
      reader.readAsDataURL(file);
    });
  }

  if (popupImageInput) {
    popupImageInput.addEventListener('change', function () {
      if (popupImageInput.files) popupAddImages(Array.from(popupImageInput.files));
      popupImageInput.value = '';
    });
  }
  if (popupImageBtn) {
    popupImageBtn.addEventListener('click', function () {
      if (popupImageInput) popupImageInput.click();
    });
  }

  chatInput.addEventListener('paste', function (e) {
    if (!popupAgentImageUpload) return;
    var items = e.clipboardData && e.clipboardData.items;
    if (!items) return;
    var imageFiles = [];
    for (var i = 0; i < items.length; i++) {
      if (items[i].type.indexOf('image/') === 0) {
        var f = items[i].getAsFile();
        if (f) imageFiles.push(f);
      }
    }
    if (imageFiles.length > 0) {
      e.preventDefault();
      popupAddImages(imageFiles);
    }
  });

  chatInput.addEventListener('input', function () {
    sendBtn.classList.toggle('active', chatInput.value.trim().length > 0);
    // 自动调整高度
    chatInput.style.height = 'auto';
    chatInput.style.height = Math.min(chatInput.scrollHeight, 100) + 'px';
  });

  chatInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      sendChat();
    }
    // Shift+Enter 默认行为即换行，无需额外处理
  });

  sendBtn.addEventListener('click', sendChat);

  function sendChat() {
    var text = chatInput.value.trim();
    if (!text) return;
    var queryData = { query: text, user: currentUser, kb: selectedKb, ts: Date.now(), agentId: popupAgentId, agentEnabled: popupAgentEnabled };
    // 如果选中了具体知识库，传递知识库 ID
    if (selectedKb && selectedKb !== 'all') {
      queryData.knowledgeBaseIds = [selectedKb];
    }

    function doPopupSend(data) {
      if (chrome && chrome.storage) {
        chrome.storage.local.set({ ka_pending_query: data });
      }
      // 通知已打开的 sidepanel（不发 CHAT_QUERY 避免 background 重复处理）
      if (chrome && chrome.runtime && chrome.runtime.sendMessage) {
        chrome.runtime.sendMessage({ type: 'SIDEPANEL_QUERY', payload: data }).catch(function () {});
      }
      if (chrome && chrome.tabs) {
        chrome.tabs.query({ active: true, currentWindow: true }, function (tabs) {
          if (tabs && tabs[0]) {
            chrome.sidePanel.open({ tabId: tabs[0].id }).catch(function () {});
          }
          window.close();
        });
      }
    }

    if (popupPendingImages.length > 0) {
      var promises = popupPendingImages.map(function (img) { return popupFileToBase64(img.file); });
      Promise.all(promises).then(function (dataURIs) {
        queryData.images = dataURIs.map(function (d) { return { data: d }; });
        popupClearImages();
        doPopupSend(queryData);
      });
    } else {
      doPopupSend(queryData);
    }
  }

  // === 知识库下拉 ===
  var kbMenu = $('kb-menu');
  var kbBtn = $('btn-kb-select');

  function positionKbMenu() {
    var rect = kbBtn.getBoundingClientRect();
    var menuH = kbMenu.offsetHeight;
    var top = rect.top - menuH - 6;
    if (top < 4) top = rect.bottom + 6;
    kbMenu.style.left = rect.left + 'px';
    kbMenu.style.top = top + 'px';
  }

  kbBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    popupModeMenu.classList.remove('show');
    var willShow = !kbMenu.classList.contains('show');
    kbMenu.classList.toggle('show');
    if (willShow) {
      requestAnimationFrame(positionKbMenu);
    }
  });
  // 知识库选择
  var kbShortNames = { 'all': '全部', 'quick-note': '笔记', 'web-collect': '收藏', 'product-doc': '文档', 'tech-spec': '技术' };
  kbMenu.querySelectorAll('.kb-dropdown-item').forEach(function (item) {
    item.addEventListener('click', function (e) {
      e.stopPropagation();
      selectedKb = item.getAttribute('data-kb');
      $('kb-name').textContent = kbShortNames[selectedKb] || item.textContent.trim();
      kbMenu.querySelectorAll('.kb-dropdown-item').forEach(function (i) { i.classList.remove('selected'); });
      item.classList.add('selected');
      kbMenu.classList.remove('show');
    });
  });

  // === 模式下拉（独立） ===
  var popupModeMenu = $('popup-mode-menu');
  var modeBtn = $('btn-mode-select');

  function positionModeMenu() {
    var rect = modeBtn.getBoundingClientRect();
    var menuH = popupModeMenu.offsetHeight;
    var top = rect.top - menuH - 6;
    if (top < 4) top = rect.bottom + 6;
    popupModeMenu.style.left = rect.left + 'px';
    popupModeMenu.style.top = top + 'px';
  }

  modeBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    kbMenu.classList.remove('show');
    var willShow = !popupModeMenu.classList.contains('show');
    popupModeMenu.classList.toggle('show');
    if (willShow) {
      requestAnimationFrame(positionModeMenu);
    }
  });

  popupModeMenu.addEventListener('click', function (e) {
    e.stopPropagation();
  });

  // 点击其他地方关闭下拉
  document.addEventListener('click', function () {
    kbMenu.classList.remove('show');
    popupModeMenu.classList.remove('show');
    if (moreMenu) moreMenu.classList.remove('show');
    closeClipKbDropdown();
  });

  // === 网页采集 ===
  var isFlipped = false;
  var collectWrap = $('collect-wrap');
  var cardFlipper = $('card-flipper');

  // 同步 flipper 容器高度为当前可见面的高度
  function syncFlipperHeight() {
    var front = $('latest-clip');
    var back = $('note-back');
    if (isFlipped) {
      var frontH = front.scrollHeight;
      var h = Math.max(frontH, 200);
      cardFlipper.style.height = h + 'px';
      back.style.height = h + 'px';
    } else {
      // 正面是 position:relative，自动撑开 flipper 高度
      cardFlipper.style.height = '';
    }
  }

  var editingClipId = null;
  var editingKbItem = null;

  function flipToNote(editClip) {
    if (isFlipped) return;
    isFlipped = true;
    var titleInput = $('note-title-input');
    var noteTitle = $('note-back').querySelector('.note-back-title');
    if (editClip && editClip.content) {
      $('note-input').value = editClip.content;
      editingClipId = editClip.id || null;
    } else {
      editingClipId = null;
    }
    if (editingKbItem) {
      titleInput.value = editingKbItem.title || '';
      titleInput.style.display = '';
      if (noteTitle) noteTitle.lastChild.textContent = ' 编辑知识';
    } else {
      titleInput.value = '';
      titleInput.style.display = 'none';
      if (noteTitle) noteTitle.lastChild.textContent = ' Markdown 速记';
    }
    var frontH = $('latest-clip').offsetHeight;
    $('note-back').style.minHeight = Math.max(frontH, 200) + 'px';
    collectWrap.classList.add('flipped');
    syncFlipperHeight();
    setTimeout(function () {
      var inp = editingKbItem ? titleInput : $('note-input');
      inp.focus();
      if (inp.setSelectionRange) inp.setSelectionRange(inp.value.length, inp.value.length);
      syncFlipperHeight();
    }, 650);
  }

  function flipToFront() {
    if (!isFlipped) return;
    isFlipped = false;
    editingClipId = null;
    editingKbItem = null;
    $('note-title-input').style.display = 'none';
    collectWrap.classList.remove('flipped');
    syncFlipperHeight();
  }

  // 页面加载后初始化高度
  setTimeout(syncFlipperHeight, 50);

  $('btn-quick-note').addEventListener('click', function () {
    flipToNote();
  });

  // 速记关闭按钮（X）— 不保存，直接翻转回正面
  $('btn-note-close').addEventListener('click', function () {
    noteInput.value = '';
    $('note-title-input').value = '';
    editingKbItem = null;
    if (isPreviewMode) {
      isPreviewMode = false;
      notePreview.style.display = 'none';
      noteInput.style.display = '';
      previewBtn.classList.remove('active');
    }
    flipToFront();
  });

  $('btn-select-clip').addEventListener('click', function () {
    chrome.tabs.query({ active: true, currentWindow: true }, function (tabs) {
      if (!tabs || !tabs[0]) return;
      var tab = tabs[0];
      var tabId = tab.id;

      // 检查是否为不支持注入的页面
      var url = tab.url || '';
      if (url.startsWith('chrome://') || url.startsWith('chrome-extension://') || url.startsWith('edge://') || url.startsWith('about:') || url === '' || url.startsWith('chrome:')) {
        toast('此页面不支持剪藏功能', 'error');
        return;
      }

      chrome.tabs.sendMessage(tabId, { type: 'SELECT_CLIP' }, function (resp) {
        if (chrome.runtime.lastError) {
          toast('请先刷新当前页面，再使用剪藏功能');
          return;
        }
        window.close();
      });
    });
  });

  // === 智能剪藏按钮 ===
  $('btn-smart-clip').addEventListener('click', function () {
    chrome.tabs.query({ active: true, currentWindow: true }, function (tabs) {
      if (!tabs || !tabs[0]) return;
      var tab = tabs[0];
      var tabId = tab.id;

      var url = tab.url || '';
      if (url.startsWith('chrome://') || url.startsWith('chrome-extension://') || url.startsWith('edge://') || url.startsWith('about:') || url === '' || url.startsWith('chrome:')) {
        toast('此页面不支持智能剪藏', 'error');
        return;
      }

      chrome.tabs.sendMessage(tabId, { type: 'SMART_CLIP' }, function (resp) {
        if (chrome.runtime.lastError) {
          toast('请先刷新当前页面，再使用剪藏功能');
          return;
        }
        window.close();
      });
    });
  });

  // === 轻量级 Markdown → HTML 渲染 ===
  function escapeHtml(s) {
    var d = document.createElement('div');
    d.textContent = s || '';
    return d.innerHTML;
  }

  function renderMarkdown(text) {
    if (!text) return '';

    // 1. 提取代码块，替换为占位符
    var codeBlocks = [];
    var processed = text.replace(/```(\w*)\n([\s\S]*?)```/g, function (_, lang, code) {
      var idx = codeBlocks.length;
      codeBlocks.push('<pre class="md-pre"><code>' + escapeHtml(code.replace(/\n$/, '')) + '</code></pre>');
      return '\x00CODEBLOCK' + idx + '\x00';
    });

    // 2. 提取行内代码
    var inlineCodes = [];
    processed = processed.replace(/`([^`]+)`/g, function (_, code) {
      var idx = inlineCodes.length;
      inlineCodes.push('<code class="md-code">' + escapeHtml(code) + '</code>');
      return '\x00INLINE' + idx + '\x00';
    });

    // 3. 表格处理 — 提取为占位符
    var tables = [];
    processed = processed.replace(/((?:\|.+\|\n)+)/g, function (tableBlock) {
      var rows = tableBlock.trim().split('\n');
      if (rows.length < 2) return tableBlock;
      var out = '<table class="md-table">';
      rows.forEach(function (row, i) {
        if (/^\|[\s\-:|]+\|$/.test(row)) return;
        var tag = i === 0 ? 'th' : 'td';
        var cells = row.split('|').filter(function (c, ci, arr) { return ci > 0 && ci < arr.length - 1; });
        out += '<tr>';
        cells.forEach(function (cell) {
          out += '<' + tag + '>' + cell.trim() + '</' + tag + '>';
        });
        out += '</tr>';
      });
      out += '</table>';
      var idx = tables.length;
      tables.push(out);
      return '\n\x00TABLE' + idx + '\x00\n';
    });

    // 4. 逐行解析为块元素
    var lines = processed.split('\n');
    var html = '';
    var i = 0;

    while (i < lines.length) {
      var line = lines[i];

      // 代码块占位符
      var cbMatch = line.match(/^\x00CODEBLOCK(\d+)\x00$/);
      if (cbMatch) {
        html += codeBlocks[parseInt(cbMatch[1])];
        i++; continue;
      }

      // 表格占位符
      var tbMatch = line.match(/^\x00TABLE(\d+)\x00$/);
      if (tbMatch) {
        html += tables[parseInt(tbMatch[1])];
        i++; continue;
      }

      // 标题
      if (/^(#{1,6})\s+(.+)/.test(line)) {
        var level = RegExp.$1.length;
        html += '<h' + level + ' class="md-h">' + inlineFormat(RegExp.$2) + '</h' + level + '>';
        i++; continue;
      }

      // 引用块
      if (/^>\s*(.*)/.test(line)) {
        var bqLines = [];
        while (i < lines.length && /^>\s*(.*)/.test(lines[i])) {
          bqLines.push(RegExp.$1);
          i++;
        }
        html += '<blockquote class="md-bq">' + inlineFormat(bqLines.join('<br>')) + '</blockquote>';
        continue;
      }

      // 无序列表
      if (/^(\s*)[*\-]\s+(.+)/.test(line)) {
        html += parseList(lines, i, 'ul');
        while (i < lines.length && /^(\s*)[*\-]\s+/.test(lines[i])) i++;
        continue;
      }

      // 有序列表
      if (/^(\s*)\d+\.\s+(.+)/.test(line)) {
        html += parseList(lines, i, 'ol');
        while (i < lines.length && /^(\s*)\d+\.\s+/.test(lines[i])) i++;
        continue;
      }

      // 水平线
      if (/^[-*_]{3,}\s*$/.test(line.trim())) {
        html += '<hr>';
        i++; continue;
      }

      // 空行
      if (line.trim() === '') {
        i++; continue;
      }

      // 普通段落
      var pLines = [];
      while (i < lines.length) {
        var pl = lines[i];
        if (pl.trim() === '' || /^#{1,6}\s/.test(pl) || /^>\s/.test(pl) ||
            /^(\s*)[*\-]\s+/.test(pl) || /^(\s*)\d+\.\s+/.test(pl) ||
            /^\x00CODEBLOCK/.test(pl) || /^\x00TABLE/.test(pl) ||
            /^[-*_]{3,}\s*$/.test(pl.trim())) break;
        pLines.push(pl);
        i++;
      }
      html += '<p class="md-p">' + inlineFormat(pLines.join('<br>')) + '</p>';
    }

    // 5. 恢复行内代码占位符
    html = html.replace(/\x00INLINE(\d+)\x00/g, function (_, idx) {
      return inlineCodes[parseInt(idx)];
    });

    return html;

    // --- 列表解析（支持嵌套）---
    function parseList(allLines, startIdx, defaultType) {
      var items = [];
      var j = startIdx;
      var baseIndent = -1;
      var listTag = defaultType;
      var re = listTag === 'ul' ? /^(\s*)[*\-]\s+(.+)/ : /^(\s*)\d+\.\s+(.+)/;

      while (j < allLines.length) {
        var m = allLines[j].match(re);
        if (!m) break;
        var indent = m[1].length;
        if (baseIndent < 0) baseIndent = indent;
        if (indent > baseIndent) {
          var subStart = j;
          while (j < allLines.length) {
            var sm = allLines[j].match(re);
            if (!sm || sm[1].length < indent) break;
            j++;
          }
          var subHtml = parseList(allLines, subStart, listTag);
          if (items.length > 0) {
            items[items.length - 1] += subHtml;
          }
          continue;
        }
        if (indent < baseIndent) break;
        items.push(inlineFormat(m[2]));
        j++;
      }

      var out = '<' + listTag + ' class="md-list">';
      for (var k = 0; k < items.length; k++) {
        out += '<li>' + items[k] + '</li>';
      }
      out += '</' + listTag + '>';
      return out;
    }

    // --- 行内格式化 ---
    function inlineFormat(s) {
      if (!s) return '';
      // 图片（必须在链接之前）
      s = s.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, '<img src="$2" alt="$1" style="max-width:100%;border-radius:6px;margin:4px 0;">');
      // 链接
      s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
      s = s.replace(/\*\*\*(.+?)\*\*\*/g, '<strong><em>$1</em></strong>');
      s = s.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
      s = s.replace(/(?<!\*)\*([^\s*][^*]*?)\*(?!\*)/g, '<em>$1</em>');
      s = s.replace(/~~(.+?)~~/g, '<del>$1</del>');
      s = s.replace(/\x00INLINE(\d+)\x00/g, function (_, idx) {
        return inlineCodes[parseInt(idx)];
      });
      // 处理 Markdown 反斜杠转义（Turndown 生成的 \_ \* \# \[ 等）
      s = s.replace(/\\([_*#\[\]()~`>|\\!{}.+-])/g, '$1');
      return s;
    }
  }

  // === Markdown 速记 ===
  var noteInput = $('note-input');
  var notePreview = $('note-preview');
  var noteSaveBtn = $('btn-note-save');
  var previewBtn = $('btn-note-preview');
  var isPreviewMode = false;

  // === Markdown 工具栏 ===
  // 在 textarea 中插入文本的辅助函数
  function insertMd(before, after, placeholder) {
    noteInput.focus();
    var start = noteInput.selectionStart;
    var end = noteInput.selectionEnd;
    var text = noteInput.value;
    var selected = text.substring(start, end);
    var insert = selected || placeholder || '';
    var newText = text.substring(0, start) + before + insert + (after || '') + text.substring(end);
    noteInput.value = newText;
    // 设置光标位置
    if (selected) {
      noteInput.selectionStart = start + before.length;
      noteInput.selectionEnd = start + before.length + insert.length;
    } else {
      noteInput.selectionStart = start + before.length;
      noteInput.selectionEnd = start + before.length + insert.length;
    }
    noteInput.dispatchEvent(new Event('input'));
  }

  // 在行首插入文本的辅助函数
  function insertLinePrefix(prefix) {
    noteInput.focus();
    var start = noteInput.selectionStart;
    var text = noteInput.value;
    // 找到当前行的开头
    var lineStart = text.lastIndexOf('\n', start - 1) + 1;
    var newText = text.substring(0, lineStart) + prefix + text.substring(lineStart);
    noteInput.value = newText;
    noteInput.selectionStart = noteInput.selectionEnd = start + prefix.length;
    noteInput.dispatchEvent(new Event('input'));
  }

  // 绑定所有工具栏按钮
  document.querySelectorAll('.note-tool-btn[data-md]').forEach(function (btn) {
    btn.addEventListener('click', function (e) {
      e.preventDefault();
      var action = btn.getAttribute('data-md');
      switch (action) {
        case 'bold':
          insertMd('**', '**', '粗体文本');
          break;
        case 'heading':
          insertLinePrefix('## ');
          break;
        case 'ul':
          insertLinePrefix('- ');
          break;
        case 'ol':
          insertLinePrefix('1. ');
          break;
        case 'link':
          insertMd('[', '](url)', '链接文本');
          break;
        case 'image':
          insertMd('![', '](url)', '图片描述');
          break;
        case 'preview':
          var content = noteInput.value.trim();
          if (!content && !isPreviewMode) { toast('请先输入内容'); return; }
          isPreviewMode = !isPreviewMode;
          if (isPreviewMode) {
            notePreview.innerHTML = renderMarkdown(content);
            noteInput.style.display = 'none';
            notePreview.style.display = 'block';
            previewBtn.classList.add('active');
          } else {
            noteInput.style.display = '';
            notePreview.style.display = 'none';
            previewBtn.classList.remove('active');
            noteInput.focus();
          }
          break;
      }
    });
  });

  noteSaveBtn.addEventListener('click', function () {
    var text = noteInput.value.trim();
    if (!text) {
      toast('请先输入内容');
      return;
    }

    noteSaveBtn.disabled = true;

    if (editingKbItem && editingKbItem.knowledgeId) {
      var titleVal = $('note-title-input').value.trim() || editingKbItem.title || '未命名';
      sendMsg({
        type: 'UPDATE_KB_KNOWLEDGE',
        payload: {
          kbId: editingKbItem.kbId,
          knowledgeId: editingKbItem.knowledgeId,
          title: titleVal,
          content: text
        }
      }).then(function (resp) {
        noteSaveBtn.disabled = false;
        if (resp && resp.success !== false) {
          toast('已更新', 'success');
          noteInput.value = '';
          $('note-title-input').value = '';
          editingKbItem = null;
          if (isPreviewMode) {
            isPreviewMode = false;
            noteInput.style.display = '';
            notePreview.style.display = 'none';
            notePreview.innerHTML = '';
            previewBtn.classList.remove('active');
          }
          setTimeout(function () { loadLatestClip(); }, 1000);
          setTimeout(function () { flipToFront(); }, 300);
        } else {
          toast('更新失败: ' + ((resp && resp.error) || '未知错误'), 'error');
        }
      });
      return;
    }

    var firstLine = text.split('\n')[0].replace(/^#+\s*/, '').trim() || '速记';
    var clip = {
      type: 'markdown',
      content: text,
      title: firstLine
    };
    if (editingClipId) {
      clip.id = editingClipId;
    }

    sendMsg({ type: 'SAVE_CLIP', payload: clip }).then(function (resp) {
      noteSaveBtn.disabled = false;
      if (resp && resp.success) {
        if (resp.syncError) {
          toast('保存失败，请稍后重试', 'error');
        } else {
          toast('笔记已保存', 'success');
        }
        noteInput.value = '';
        editingClipId = null;
        if (isPreviewMode) {
          isPreviewMode = false;
          noteInput.style.display = '';
          notePreview.style.display = 'none';
          notePreview.innerHTML = '';
          previewBtn.classList.remove('active');
        }
        setTimeout(function () { loadLatestClip(); }, 1000);
        setTimeout(function () { flipToFront(); }, 300);
      } else {
        toast('保存失败: ' + ((resp && resp.error) || '未知错误'), 'error');
      }
    });
  });

  // === 三点菜单 ===
  var moreMenu = $('more-menu');
  $('btn-more').addEventListener('click', function (e) {
    e.stopPropagation();
    moreMenu.classList.toggle('show');
  });

  // === 设置页面 ===
  $('btn-settings').addEventListener('click', function () {
    moreMenu.classList.remove('show');
    // 填充设置面板数据
    if (currentUser) {
      $('set-type').textContent = currentUser.badge || '-';
      $('set-name').textContent = currentUser.name || '-';
    }
    loadSelBubbleToggle();
    goPage('pg-settings');
    loadShortcuts();
  });

  // 返回按钮
  $('btn-settings-back').addEventListener('click', function () {
    goPage('pg-main');
  });

  // === 选中文字气泡开关 ===
  function loadSelBubbleToggle() {
    chrome.storage.local.get('ka_sel_bubble_enabled', function (data) {
      var enabled = data.ka_sel_bubble_enabled !== false;
      $('set-sel-bubble').checked = enabled;
    });
  }

  $('set-sel-bubble').addEventListener('change', function () {
    var enabled = $('set-sel-bubble').checked;
    chrome.storage.local.set({ ka_sel_bubble_enabled: enabled });
  });

  // === 快捷键展示 ===
  var shortcutCmdMap = {
    'select-clip': 'set-shortcut-select',
    'smart-clip': 'set-shortcut-smart',
    'quick-note': 'set-shortcut-note',
    'open-sidepanel': 'set-shortcut-sidepanel'
  };

  function loadShortcuts() {
    if (!chrome.commands || !chrome.commands.getAll) return;
    chrome.commands.getAll(function (commands) {
      commands.forEach(function (cmd) {
        var targetId = shortcutCmdMap[cmd.name];
        if (!targetId) return;
        var el = $(targetId);
        if (!el) return;
        var shortcut = cmd.shortcut || '';
        if (!shortcut) {
          el.innerHTML = '<kbd class="shortcut-unset">未设置</kbd>';
        } else {
          var keys = shortcut.split('+');
          el.innerHTML = keys.map(function (k) { return '<kbd>' + escapeHtml(k.trim()) + '</kbd>'; }).join('<span class="kbd-plus">+</span>');
        }
      });
    });
  }

  // 点击任意快捷键行 → 跳转 Chrome 快捷键设置
  document.querySelectorAll('.settings-shortcut-row').forEach(function (row) {
    row.addEventListener('click', function () {
      chrome.tabs.create({ url: 'chrome://extensions/shortcuts' });
      window.close();
    });
  });

  // === 剪藏知识库选择（下拉菜单） ===
  var clipKbPopupOpen = false;
  var clipKbDropdown = $('clip-kb-dropdown');
  var clipKbBarWrap = $('clip-kb-bar-wrap');

  // --- 知识库选择页面（登录后首次选择） ---
  var selectKbList = $('select-kb-list');

  function showSelectKbPage(kbList) {
    var list = kbList || popupAllKbs;
    if (!selectKbList) return;

    var kbIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg>';
    var html = '';

    if (!list || list.length === 0) {
      html = '<div class="select-kb-empty">'
        + '<svg class="select-kb-empty-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.2"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/><line x1="9" y1="8" x2="17" y2="8"/><line x1="9" y1="12" x2="15" y2="12"/></svg>'
        + '<div class="select-kb-empty-text">暂无知识库</div>'
        + '<div class="select-kb-empty-sub">请先前往创建一个知识库</div>'
        + '</div>';
    } else {
      list.forEach(function (kb) {
        var selected = kb.id === clipKbId ? ' selected' : '';
        html += '<div class="select-kb-item' + selected + '" data-kb-id="' + kb.id + '" data-kb-name="' + escapeHtml(kb.name) + '">'
          + kbIcon
          + '<span class="select-kb-item-name">' + escapeHtml(kb.name) + '</span>'
          + '<span class="select-kb-radio"></span>'
          + '</div>';
      });
    }
    selectKbList.innerHTML = html;

    // 绑定点击选择
    selectKbList.querySelectorAll('.select-kb-item').forEach(function (item) {
      item.addEventListener('click', function () {
        clipKbId = item.getAttribute('data-kb-id');
        clipKbName = item.getAttribute('data-kb-name');
        persistClipKbSelection(clipKbId, clipKbName, function () {
          chrome.tabs.query({}, function (tabs) {
            tabs.forEach(function (t) {
              if (t.id) chrome.tabs.sendMessage(t.id, { type: 'AUTH_STATE_CHANGED' }).catch(function () {});
            });
          });
          updateClipKbDisplay();
          loadKbItems();
        });
        updateClipKbDisplay();
        selectKbList.querySelectorAll('.select-kb-item').forEach(function (c) { c.classList.remove('selected'); });
        item.classList.add('selected');
        // 选中后跳回主界面
        setTimeout(function () {
          goPage('pg-main');
        }, 250);
        toast('已选择: ' + clipKbName, 'success');
      });
    });

    // 如果当前不在选择页，才跳转（enterMain 中可能已提前跳转）
    var selectKbPage = $('pg-select-kb');
    if (selectKbPage && !selectKbPage.classList.contains('active')) {
      goPage('pg-select-kb');
    }
  }

  // 返回按钮
  $('btn-select-kb-back').addEventListener('click', function () {
    goPage('pg-main');
  });

  // "前往创建"按钮
  $('select-kb-goto').addEventListener('click', function () {
    openNotesPage();
  });

  // --- 卡片内知识库下拉菜单 ---

  // 关闭知识库下拉菜单
  function closeClipKbDropdown() {
    if (!clipKbDropdown) return;
    clipKbDropdown.classList.remove('show');
    if (clipKbBarWrap) clipKbBarWrap.classList.remove('open');
  }

  // 打开知识库下拉菜单
  function openClipKbDropdown() {
    if (!clipKbDropdown) return;
    clipKbDropdown.classList.add('show');
    if (clipKbBarWrap) clipKbBarWrap.classList.add('open');
    // 计算 fixed 定位：基于知识库指示行
    positionClipKbDropdown();
  }

  function positionClipKbDropdown() {
    if (!clipKbDropdown || !clipKbBarWrap) return;
    var barRect = clipKbBarWrap.getBoundingClientRect();
    // 以卡片为基准，确保下拉菜单左右不超出卡片
    var cardEl = document.querySelector('.collect-card-front');
    var cardRect = cardEl ? cardEl.getBoundingClientRect() : barRect;
    var cardPadding = 10; // 卡片内侧留白
    var maxWidth = cardRect.width - cardPadding * 2;
    var menuWidth = Math.min(maxWidth, 300);
    // 水平居中于卡片内
    var left = cardRect.left + cardPadding;
    var top = barRect.bottom + 2;
    clipKbDropdown.style.left = left + 'px';
    clipKbDropdown.style.top = top + 'px';
    clipKbDropdown.style.width = menuWidth + 'px';
  }

  // 渲染并显示知识库下拉菜单（卡片内）
  function showClipKbSelector(kbList) {
    if (!clipKbDropdown) return;

    var list = kbList || popupAllKbs;
    var html = '';
    var kbIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg>';

    if (!list || list.length === 0) {
      html = '<div class="clip-kb-dd-empty">'
        + '<div>暂无知识库</div>'
        + '<div class="clip-kb-dd-empty-sub">请先前往创建一个知识库</div>'
        + '</div>'
        + '<div class="clip-kb-dropdown-footer">'
        + '<button class="clip-kb-dropdown-goto" id="clip-kb-dd-goto">'
        + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>'
        + '前往创建知识库</button></div>';
    } else {
      list.forEach(function (kb) {
        var selected = kb.id === clipKbId ? ' selected' : '';
        html += '<div class="clip-kb-dropdown-item' + selected + '" data-kb-id="' + kb.id + '" data-kb-name="' + escapeHtml(kb.name) + '">'
          + '<span class="clip-kb-dd-radio"></span>'
          + kbIcon
          + '<span class="clip-kb-dropdown-item-name">' + escapeHtml(kb.name) + '</span>'
          + '</div>';
      });
      html += '<div class="clip-kb-dropdown-footer">'
        + '<button class="clip-kb-dropdown-goto" id="clip-kb-dd-goto">'
        + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>'
        + '前往创建知识库</button></div>';
    }
    clipKbDropdown.innerHTML = html;

    // 绑定点击选择
    clipKbDropdown.querySelectorAll('.clip-kb-dropdown-item').forEach(function (item) {
      item.addEventListener('click', function (e) {
        e.stopPropagation();
        clipKbId = item.getAttribute('data-kb-id');
        clipKbName = item.getAttribute('data-kb-name');
        persistClipKbSelection(clipKbId, clipKbName, function () {
          chrome.tabs.query({}, function (tabs) {
            tabs.forEach(function (t) {
              if (t.id) chrome.tabs.sendMessage(t.id, { type: 'AUTH_STATE_CHANGED' }).catch(function () {});
            });
          });
          updateClipKbDisplay();
          loadKbItems();
        });
        updateClipKbDisplay();
        clipKbDropdown.querySelectorAll('.clip-kb-dropdown-item').forEach(function (c) { c.classList.remove('selected'); });
        item.classList.add('selected');
        // 选中后短暂延迟关闭
        setTimeout(function () {
          closeClipKbDropdown();
        }, 200);
        toast('已选择: ' + clipKbName, 'success');
      });
    });

    // 绑定"前往创建"按钮
    var gotoBtn = clipKbDropdown.querySelector('#clip-kb-dd-goto');
    if (gotoBtn) {
      gotoBtn.addEventListener('click', function (e) {
        e.stopPropagation();
        closeClipKbDropdown();
        openNotesPage();
      });
    }

    openClipKbDropdown();
  }

  function checkClipKbSelection(kbList) {
    loadClipKbSelectionForCurrentUser(function (data) {
      var prevKbId = clipKbId;
      clipKbId = data.kbId || '';
      clipKbName = data.kbName || '';

      if (clipKbId) {
        var found = kbList.some(function (kb) { return kb.id === clipKbId; });
        if (!found) {
          clipKbId = '';
          clipKbName = '';
          persistClipKbSelection('', '', function () {});
        }
      }

      updateClipKbDisplay();

      if (!clipKbId) {
        showSelectKbPage(kbList);
      } else if (clipKbId !== prevKbId) {
        loadKbItems();
      }
    });
  }

  function updateClipKbDisplay() {
    var el = $('set-clip-kb');
    if (el) {
      var arrow = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="width:12px;height:12px;"><polyline points="6 9 12 15 18 9"/></svg>';
      el.innerHTML = escapeHtml(clipKbName || '未选择') + ' ' + arrow;
    }
    // 同步更新卡片顶部知识库指示行
    var barName = $('clip-kb-bar-name');
    if (barName) {
      barName.textContent = clipKbName || '未选择知识库';
    }
  }

  // 设置页面：点击知识库行切换浮层（设置面板内的弹出列表）
  $('set-clip-kb-row').addEventListener('click', function (e) {
    e.stopPropagation();
    clipKbPopupOpen = !clipKbPopupOpen;
    var popup = $('settings-kb-popup');
    if (clipKbPopupOpen) {
      loadClipKbPopup();
      popup.classList.add('show');
    } else {
      popup.classList.remove('show');
    }
  });

  // 点击外部关闭浮层
  document.addEventListener('click', function () {
    if (clipKbPopupOpen) {
      clipKbPopupOpen = false;
      $('settings-kb-popup').classList.remove('show');
    }
  });

  function loadClipKbPopup() {
    var popup = $('settings-kb-popup');
    var list = popupAllKbs;

    if (!list || list.length === 0) {
      popup.innerHTML = '<div class="settings-kb-empty">'
        + '<div class="settings-kb-empty-text">暂无知识库，请先创建</div>'
        + '<button class="settings-kb-empty-btn" id="btn-kb-goto-create">前往创建</button>'
        + '</div>';
      var gotoBtn = popup.querySelector('#btn-kb-goto-create');
      if (gotoBtn) {
        gotoBtn.addEventListener('click', function () {
          clipKbPopupOpen = false;
          popup.classList.remove('show');
          openNotesPage();
        });
      }
      return;
    }

    var html = '';
    list.forEach(function (kb) {
      var selected = kb.id === clipKbId ? ' selected' : '';
      html += '<div class="settings-kb-option' + selected + '" data-kb-id="' + kb.id + '" data-kb-name="' + escapeHtml(kb.name) + '">'
        + '<span class="kb-dot"></span>'
        + escapeHtml(kb.name)
        + '</div>';
    });
    popup.innerHTML = html;

    popup.querySelectorAll('.settings-kb-option').forEach(function (opt) {
      opt.addEventListener('click', function (e) {
        e.stopPropagation();
        clipKbId = opt.getAttribute('data-kb-id');
        clipKbName = opt.getAttribute('data-kb-name');
        persistClipKbSelection(clipKbId, clipKbName, function () {
          chrome.tabs.query({}, function (tabs) {
            tabs.forEach(function (t) {
              if (t.id) chrome.tabs.sendMessage(t.id, { type: 'AUTH_STATE_CHANGED' }).catch(function () {});
            });
          });
          updateClipKbDisplay();
          // 切换知识库后拉取该知识库的条目
          loadKbItems();
        });
        updateClipKbDisplay();
        popup.querySelectorAll('.settings-kb-option').forEach(function (o) { o.classList.remove('selected'); });
        opt.classList.add('selected');
        clipKbPopupOpen = false;
        popup.classList.remove('show');
        toast('已选择: ' + clipKbName, 'success');
      });
    });
  }

  // === 渲染知识库条目列表（共享函数，缓存和 API 响应都调用） ===
  function renderKbItemsList(items, total, typeHintMap) {
    var emptyEl = $('latest-clip-empty');
    var contentEl = $('latest-clip-content');
    var listEl = $('clip-list');

    if (items.length === 0) {
      emptyEl.style.display = '';
      contentEl.style.display = 'none';
      setTimeout(syncFlipperHeight, 50);
      return;
    }

    emptyEl.style.display = 'none';
    contentEl.style.display = '';

    var html = '';
    items.slice(0, 3).forEach(function (item) {
      var title = item.title || item.name || '未命名';
      var docType = item.doc_type || item.type || '';
      var updatedAt = item.updated_at || item.updatedAt || item.created_at || item.createdAt || '';
      var sourceUrl = item.source_url || item.url || '';
      var content = item.content || '';

      if (!sourceUrl && content) {
        var srcMatch = content.match(/^>\s*来源:\s*(https?:\/\/\S+)/m);
        if (srcMatch) sourceUrl = srcMatch[1];
      }

      var typeHint = item._clipTypeHint || (typeHintMap && item.id ? typeHintMap[item.id] : '') || detectClipTypeHint(title, content, '');
      var typeInfo = getItemTypeInfo(docType, sourceUrl, title, content, typeHint);

      var metaHtml = updatedAt ? '<span>' + formatTimeShort(updatedAt) + '</span>' : '';

      html += '<li class="clip-list-item" data-item-id="' + (item.id || '') + '">' +
        '<div class="clip-list-icon ' + typeInfo.iconClass + '">' + typeInfo.iconSvg + '</div>' +
        '<div class="clip-list-info">' +
          '<div class="clip-list-title">' + escapeHtml(title) + '</div>' +
          '<div class="clip-list-meta">' + metaHtml + '</div>' +
        '</div>' +
      '</li>';
    });
    listEl.innerHTML = html;

    var displayedItems = items.slice(0, 3);
    listEl.querySelectorAll('.clip-list-item').forEach(function (el, idx) {
      el.addEventListener('click', function () {
        var itemData = displayedItems[idx];
        if (!itemData || !itemData.id) return;
        chrome.tabs.query({ active: true, currentWindow: true }, function (tabs) {
          if (!tabs || !tabs[0]) return;
          var tabId = tabs[0].id;
          var payload = { kbId: clipKbId, knowledgeId: itemData.id, title: itemData.title || itemData.name || '' };
          chrome.tabs.sendMessage(tabId, { type: 'EDIT_KB_KNOWLEDGE', payload: payload }, function (resp) {
            if (chrome.runtime.lastError) {
              sendMsg({ type: 'INJECT_SCRIPT', payload: { tabId: tabId } }).then(function (injectResp) {
                if (injectResp && injectResp.success) {
                  setTimeout(function () {
                    chrome.tabs.sendMessage(tabId, { type: 'EDIT_KB_KNOWLEDGE', payload: payload }, function () {
                      void chrome.runtime.lastError;
                    });
                  }, 600);
                }
              });
            }
          });
        });
      });
    });

    setTimeout(syncFlipperHeight, 50);
  }

  // 从缓存渲染（enterMain 中直接调用，无需等待 API）
  function renderKbItemsFromCache(items, total, callback) {
    getKbItemTypeHintMap(clipKbId).then(function (typeHintMap) {
      renderKbItemsList(items, total, typeHintMap || {});
      if (typeof callback === 'function') callback();
    });
  }

  // === 从 API 拉取当前知识库的条目列表 ===
  function loadKbItems() {
    var emptyEl = $('latest-clip-empty');
    var contentEl = $('latest-clip-content');
    var listEl = $('clip-list');

    if (!clipKbId) {
      emptyEl.style.display = '';
      contentEl.style.display = 'none';
      setTimeout(syncFlipperHeight, 50);
      return;
    }

    // 如果列表已有内容（来自缓存），不显示"加载中"，静默刷新
    var hasContent = listEl.children.length > 0 && !listEl.querySelector('.clip-list-loading');
    if (!hasContent) {
      emptyEl.style.display = 'none';
      contentEl.style.display = '';
      listEl.innerHTML = '<li class="clip-list-loading">加载中…</li>';
      setTimeout(syncFlipperHeight, 50);
    }

    sendMsg({ type: 'LIST_KB_ITEMS', payload: { kbId: clipKbId, page: 1, pageSize: 5 } }).then(function (resp) {
      var items = [];
      var total = 0;
      if (resp && resp.success && resp.data) {
        if (Array.isArray(resp.data)) {
          items = resp.data;
        } else if (resp.data.items && Array.isArray(resp.data.items)) {
          items = resp.data.items;
          total = resp.data.total || resp.data.total_count || 0;
        }
        if (!total && resp.data.total) total = resp.data.total;
        if (!total && resp.data.total_count) total = resp.data.total_count;
      } else if (resp && !resp.success && !resp.error) {
        if (Array.isArray(resp)) {
          items = resp;
        } else if (resp.items && Array.isArray(resp.items)) {
          items = resp.items;
          total = resp.total || resp.total_count || 0;
        } else if (resp.data && Array.isArray(resp.data)) {
          items = resp.data;
        } else if (resp.data && resp.data.items && Array.isArray(resp.data.items)) {
          items = resp.data.items;
          total = resp.data.total || resp.data.total_count || 0;
        }
      }
      if (!total) total = items.length;

      getKbItemTypeHintMap(clipKbId).then(function (typeHintMap) {
        typeHintMap = typeHintMap || {};
        renderKbItemsList(items, total, typeHintMap);
        var cachedItems = items.slice(0, 3).map(function (item) {
          var title = item.title || item.name || '';
          var content = item.content || '';
          var hint = (item.id ? typeHintMap[item.id] : '') || detectClipTypeHint(title, content, item._clipTypeHint || '');
          return Object.assign({}, item, hint ? { _clipTypeHint: hint } : {});
        });
        chrome.storage.local.set({
          ka_kb_items_cache: { kbId: clipKbId, items: cachedItems, total: total, ts: Date.now() }
        });
        var newHints = {};
        cachedItems.forEach(function (ci) {
          if (ci.id && ci._clipTypeHint) newHints[ci.id] = ci._clipTypeHint;
        });
        persistTypeHints(newHints);
      });
    }).catch(function () {
      // API 失败时，如果已有缓存内容则保持不动，否则才显示空
      if (!hasContent) {
        emptyEl.style.display = '';
        contentEl.style.display = 'none';
        setTimeout(syncFlipperHeight, 50);
      }
    });
  }

  // 卡片顶部知识库指示行 — 左侧点击 toggle 下拉菜单
  $('clip-kb-bar').addEventListener('click', function (e) {
    e.stopPropagation();
    if (clipKbDropdown && clipKbDropdown.classList.contains('show')) {
      closeClipKbDropdown();
    } else {
      showClipKbSelector();
    }
  });

  // 卡片顶部知识库指示行 — 右侧"更多..."跳转（与设置中笔记页一致）
  $('clip-kb-bar-more').addEventListener('click', function (e) {
    e.stopPropagation();
    openNotesPage();
  });

  // 点击下拉菜单内部不冒泡关闭
  if (clipKbDropdown) {
    clipKbDropdown.addEventListener('click', function (e) {
      e.stopPropagation();
    });
  }

  // "前往创建"按钮 — 打开 WeKnora 主站（备用，动态绑定在 showClipKbSelector 中）

  // === 打开对话面板（sidebar）===
  $('btn-sidebar').addEventListener('click', function () {
    moreMenu.classList.remove('show');
    if (chrome && chrome.tabs) {
      chrome.tabs.query({ active: true, currentWindow: true }, function (tabs) {
        if (tabs && tabs[0]) {
          chrome.sidePanel.open({ tabId: tabs[0].id }).catch(function () {});
        }
        window.close();
      });
    }
  });

  // === 打开笔记/知识库页面 ===
  function openNotesPage() {
    if (!currentUser || !chrome || !chrome.tabs) return;
    if (currentUser.type === 'wk') {
      sendMsg({ type: 'GET_CONFIG' }).then(function (resp) {
        if (resp && resp.success && resp.data && resp.data.baseUrl) {
          var url = resp.data.baseUrl.replace(/\/api\/v1\/?$/, '');
          chrome.tabs.create({ url: url });
        } else {
          toast('请先配置服务地址', 'error');
        }
      });
    } else {
      chrome.tabs.create({ url: 'https://weknora.weixin.qq.com/platform/knowledge' });
    }
  }

  // === 查看笔记按钮 ===
  $('btn-clips').addEventListener('click', function () {
    moreMenu.classList.remove('show');
    openNotesPage();
  });

  // === 退出登录（在设置面板内） ===
  $('btn-logout').addEventListener('click', function () {
    sendMsg({ type: 'CLEAR_AUTH' }).then(function () {
      currentUser = null;
      goPage('pg-login');
      toast('已退出登录');
    });
  });

  // 设置面板 — 开源项目链接
  $('set-github-row').addEventListener('click', function () {
    chrome.tabs.create({ url: 'https://github.com/Tencent/WeKnora' });
  });

  // === 监听 storage 变化，实时刷新最近剪藏列表 ===
  if (chrome && chrome.storage && chrome.storage.onChanged) {
    chrome.storage.onChanged.addListener(function (changes, area) {
      if (area === 'local' && (changes.ka_clips || changes.ka_notes)) {
        // 只在主界面可见时刷新（延迟等待后端同步）
        if (currentUser && $('pg-main').classList.contains('active') && !isFlipped) {
          setTimeout(function () { loadLatestClip(); }, 1000);
        }
      }
    });
  }

  // === 监听 TOKEN_EXPIRED 广播，实时响应 token 过期 ===
  if (chrome && chrome.runtime && chrome.runtime.onMessage) {
    chrome.runtime.onMessage.addListener(function (msg) {
      if (msg && msg.type === 'TOKEN_EXPIRED') {
        sendMsg({ type: 'CLEAR_AUTH' });
        currentUser = null;
        goPage('pg-login');
        toast('登录已过期，请重新扫码登录', 'error');
      }
    });
  }

  // === 初始化：检查已登录状态 ===
  function showReady() {
    document.body.classList.add('ready');
  }

  (function init() {
    sendMsg({ type: 'GET_AUTH' }).then(function (resp) {
      if (resp && resp.success && resp.data) {
        var auth = resp.data;

        function doEnterMain() {
          if (auth.type === 'ka') {
            enterMain('ka', auth.name || '知识管理助手用户', auth.avatar || '', '知识管理助手');
          } else if (auth.type === 'wk') {
            enterMain('wk', auth.name || 'Saras', auth.avatar || '', 'WeKnora');
          }
          showReady();
        }

        // 扫码登录用户：先验证 token 是否仍然有效，再决定是否进入主界面
        if (auth.login_type === 'scan') {
          sendMsg({ type: 'VALIDATE_CONFIG' }).then(function (vResp) {
            if (vResp && (vResp.expired || vResp.status === 401)) {
              sendMsg({ type: 'CLEAR_AUTH' });
              currentUser = null;
              goPage('pg-login');
              showReady();
              toast('登录已过期，请重新扫码登录', 'error');
            } else {
              doEnterMain();
            }
          });
        } else {
          doEnterMain();
        }
      } else {
        goPage('pg-login');
        showReady();
      }
    });
  })();
})();

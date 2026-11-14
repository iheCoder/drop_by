'use strict';

// 前端只呈现真实持久化记录。没有示例聊天、模拟输入状态或浏览器端后台定时生成。
const $ = id => document.getElementById(id);
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let state = { members: [], conversations: [], active_rooms: [] };
let selected = localStorage.getItem('dropby-room') || '';
let roomData = null, messages = [], replyTo = null, loadSequence = 0, hasEarlier = false;
let refreshTimer, toastTimer, budgetRefreshTimer, currentVersions = [];
let syncingMessages = false, refreshQueued = false, waitingForMessageCheck = false;
const messageCheckInterval = 15_000;
const messageRetryDelay = 2_000;
let currentArtifactIndex = 0, artifactMode = 'preview', panelRequest = 0;
let panelView = '', avatarData = '', avatarIcon = 'cat', avatarColor = 'jade';
let sideTab = 'chats', sending = false, renderedMessages = '', profileBaseline = null;
let domainEditorIndex = null, domainEditorFresh = false;
let avatarRequest = 0, avatarLoading = false;
const drafts = new Map();
let seen = {};
try { seen = JSON.parse(localStorage.getItem('dropby-seen') || '{}'); } catch { /* 损坏的本机已读标记不影响聊天数据。 */ }
const notificationCursor = {};
const avatarPresets = [['portrait-sky','晴空'],['portrait-cat','墨镜猫'],['portrait-traveler','旅人'],['cat','小猫'],['fox','狐狸'],['bear','小熊'],['rabbit','兔子'],['owl','猫头鹰'],['sprout','嫩芽'],['flower','花朵'],['sun','太阳'],['moon','月亮'],['cloud','云朵'],['ghost','小幽灵'],['orbit','星球']];
const colors = [['jade','青绿'],['apricot','杏橙'],['blue','雾蓝'],['rose','玫瑰'],['violet','浅紫'],['moss','苔绿']];
const icon = name => `<svg class="icon" aria-hidden="true"><use href="/icons.svg#${name}"/></svg>`;
let initialLoad = true;

async function api(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { 'Content-Type': 'application/json', ...options.headers } });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || '请求没有完成，请稍后再试。');
  return data;
}
const post = (path, data) => api(path, { method: 'POST', body: JSON.stringify(data) });
function toast(text) { $('toast').textContent = text; $('toast').hidden = false; clearTimeout(toastTimer); toastTimer = setTimeout(() => $('toast').hidden = true, 5000); }
function member(id) { return state.members.find(m => m.id === id); }
// 列表、选择器和浮卡共享当前人物说明，不再展示创建时留下的 direction。
// 摘录只截短原文，不根据一次聊天另生成或持久化一份人格。
function memberIntroduction(m) {
  const text=(m?.profile || '').trim();
  if(!text || text==='性格和兴趣尚未设定，可在实际交流中逐步形成；不虚构过往经历。') return '边聊，边认识';
  return text.length>90 ? `${text.slice(0,90)}…` : text;
}
function authorName(id) { return id === 'user' ? (state.profile?.name || '你') : member(id)?.name || '成员'; }
// 已有成员使用稳定的图形头像；视觉升级不重写成员身份或历史记录。
function presetFor(m) {
  const seed = Array.from(m?.id || m?.name || '友').reduce((total, c) => total + c.codePointAt(0), 0);
  return avatarPresets.some(([id]) => id === m?.avatar_icon) ? m.avatar_icon : avatarPresets[seed % avatarPresets.length][0];
}
function avatar(m, small = false) {
  const preset = presetFor(m);
  const portrait = preset.startsWith('portrait-') ? `<img src="/assets/${preset}.webp" alt="">` : m?.color === 'user' ? icon('user-round') : `<svg aria-hidden="true"><use href="/avatars.svg#${preset}"/></svg>`;
  return `<span class="avatar ${escapeHTML(m?.color || 'jade')} ${small ? 'small' : ''}" aria-hidden="true">${m?.avatar ? `<img src="${escapeHTML(m.avatar)}" alt="">` : portrait}</span>`;
}
function groupAvatar(c) {
  if (c.avatar) return avatar({avatar:c.avatar});
  const people = c.member_ids.map(member).filter(Boolean).slice(0, 2);
  return people.length > 1 ? `<span class="group-portrait" aria-hidden="true">${people.map(m => avatar(m, true)).join('')}</span>` : `<span class="group-icon" aria-hidden="true">${icon('users')}</span>`;
}
function timeLabel(date) { return new Date(date).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }); }
function dateLabel(date) { return new Date(date).toLocaleDateString('zh-CN', { month: 'long', day: 'numeric' }); }

// 周边界由服务端统一计算；界面只呈现它返回的重置时刻，不按浏览器时区另算一周。
function budgetResetLabel() {
  const reset = new Date(state.usage?.reset_at);
  if (!Number.isFinite(reset.getTime())) return '';
  const timeZone = state.usage.time_zone;
  const format = {month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'};
  try {
    const zoneLabel=timeZone === 'Asia/Shanghai' ? '北京时间' : timeZone ? new Intl.DateTimeFormat('zh-CN',{timeZone,timeZoneName:'short'}).formatToParts(reset).find(p=>p.type==='timeZoneName')?.value : '本地时间';
    return `${reset.toLocaleString('zh-CN', {...format,...(timeZone ? {timeZone} : {})})}（${zoneLabel || '本地时间'}）`;
  } catch {
    // 未配置时区的服务可能返回 Local；仍显示同一绝对时刻，并明确这是浏览器本地时间。
    return `${reset.toLocaleString('zh-CN', format)}（本地时间）`;
  }
}

// Markdown 先解析再用本地 DOMPurify 白名单清洗。聊天不是 HTML 执行环境；
// 图片、表单和内联脚本不会被带入宿主页，作品页面仍走独立 sandbox 预览。
function richText(text) {
  const fragment = DOMPurify.sanitize(marked.parse(String(text || ''), {gfm:true,breaks:true}), {
    RETURN_DOM_FRAGMENT:true,
    ALLOWED_TAGS:['p','br','strong','em','del','blockquote','ul','ol','li','pre','code','a','hr','h1','h2','h3','h4','h5','h6','table','thead','tbody','tr','th','td'],
    ALLOWED_ATTR:['href','title','start','align']
  });
  for (const link of fragment.querySelectorAll('a')) {
    if (!/^https?:\/\//i.test(link.getAttribute('href') || '')) link.removeAttribute('href');
    else { link.target='_blank'; link.rel='noopener noreferrer'; }
  }
  for (const table of [...fragment.querySelectorAll('table')]) {
    const wrap=document.createElement('div'); wrap.className='markdown-table'; wrap.tabIndex=0;
    wrap.setAttribute('role','region'); wrap.setAttribute('aria-label','可横向滚动的表格');
    table.replaceWith(wrap); wrap.append(table);
  }
  const container=document.createElement('div'); container.className='markdown'; container.append(fragment);
  return container.outerHTML;
}

async function refresh(signal) {
  try {
    state = await api('/api/state', {signal});
    // 新私聊也能提醒；通知游标和已读游标分开，围观其他群不会把它们误标为已读。
    for (const c of state.conversations) {
      const previous = notificationCursor[c.id];
      if (!initialLoad && document.hidden && c.last_seq > (previous || 0) && 'Notification' in window && Notification.permission === 'granted') {
        const data = await api(`/api/conversations/${c.id}`, {signal});
        for (const m of data.messages.filter(m => m.seq > (previous || 0) && m.notify && m.author_id !== 'user' && m.author_id !== 'system')) {
          const n = new Notification(`${authorName(m.author_id)} · ${c.name}`, { body: m.body.slice(0, 120), tag: m.id });
          n.onclick = () => { window.focus(); openRoom(c.id); n.close(); };
        }
      }
      notificationCursor[c.id] = c.last_seq;
    }
    initialLoad = false;
    renderSidebar();
    renderMyProfile();
    // 周额度恢复即刷新提示；这是账户状态刷新，不会触发模型或重复轮询。
    clearTimeout(budgetRefreshTimer);
    const untilReset=new Date(state.usage?.reset_at).getTime()-Date.now();
    if (untilReset>0) budgetRefreshTimer=setTimeout(scheduleRefresh,Math.min(untilReset+100,2147483647));
    const banner = !state.model_ready ? '聊天服务暂未连接，仍可以查看记录和编辑朋友资料。' : state.budget_blocked ? '可用额度暂时不足，进展已保留。恢复后会继续，也可以在个人中心查看。' : !state.search_ready ? '网页探索暂未接通，聊天和本地创作仍可使用。' : '';
    $('service-banner').textContent = banner; $('service-banner').hidden = !banner;
    if (workspaceView === 'account') updateAccountUsage();
    if (workspaceView === 'budget') updateBudgetPreview();
    if (workspaceView === 'usage') refreshUsageAnalysis();
    if (!selected || !state.conversations.some(c => c.id === selected)) selected = state.conversations[0]?.id || '';
    if (selected) await loadConversation(selected, true, signal); else renderWelcome();
  } catch (error) {
    if (signal) throw error;
    toast(error.message);
  }
}

function scheduleRefresh() {
  // 实时通知合并处理。连续失败后由定时检查接手，避免通知不断触发重试。
  if (waitingForMessageCheck) return;
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => {
    refreshTimer = undefined;
    syncMessages();
  }, 150);
}

async function syncMessages(checkOnly = false) {
  if (syncingMessages) {
    if (!checkOnly) refreshQueued = true;
    return;
  }
  if (checkOnly && (document.hidden || $('workspace-page').open || refreshTimer)) return;
  if (checkOnly && !selected && !waitingForMessageCheck) return;

  syncingMessages = true;
  const notice = $('message-sync-notice');
  // 如果上一次更新失败，直接补读，避免只查到相同编号就误判为恢复。
  let needsRefresh = !checkOnly || waitingForMessageCheck;
  try {
    const attempts = waitingForMessageCheck ? 1 : 2;
    for (let attempt = 0; attempt < attempts; attempt++) {
      const signal = AbortSignal.timeout(8_000);
      try {
        if (!needsRefresh) {
          const room = selected;
          const {last_seq} = await api(`/api/conversations/${room}/latest`, {signal});
          if (selected !== room) return;
          needsRefresh = roomData?.conversation.id !== room || last_seq > (messages.at(-1)?.seq || 0);
        }
        if (needsRefresh) await refresh(signal);
        waitingForMessageCheck = false;
        notice.hidden = true;
        return;
      } catch {
        if (attempt + 1 < attempts) await new Promise(resolve => setTimeout(resolve, messageRetryDelay));
      }
    }
    waitingForMessageCheck = true;
    notice.hidden = false;
  } finally {
    syncingMessages = false;
    if (refreshQueued) {
      refreshQueued = false;
      scheduleRefresh();
    }
  }
}

function renderSidebar() {
  const query = $('chat-search').value.trim().toLocaleLowerCase();
  const conversations = state.conversations.filter(c => `${c.name} ${c.last_message}`.toLocaleLowerCase().includes(query));
  const people = state.members.filter(m => `${m.name} ${m.profile}`.toLocaleLowerCase().includes(query));
  $('conversation-list').innerHTML = conversations.length ? conversations.map(c => {
    const m = c.kind === 'direct' ? member(c.member_ids[0]) : null;
    const unread = c.last_seq > (seen[c.id] || 0) && c.id !== selected;
    // 升级前的日预算提示仍是原始聊天记录，不能在列表里冒充今天的额度状态。
    const previewNode=document.createElement('div'); previewNode.innerHTML=richText(c.last_message);
    const preview=previewNode.textContent.replace(/\s+/g,' ').trim();
    return `<button class="room-button ${c.id === selected ? 'selected' : ''}" data-action="open-room" data-id="${c.id}" ${c.id === selected ? 'aria-current="page"' : ''}>${m ? avatar(m) : groupAvatar(c)}<span class="room-copy"><span class="room-heading"><span class="room-name">${escapeHTML(c.name)}</span><time class="room-time">${c.updated_at ? timeLabel(c.updated_at) : ''}</time></span><span class="room-preview">${escapeHTML(preview || '从一句随口的话开始')}</span></span>${unread ? '<span class="unread" aria-label="有新消息"></span>' : ''}</button>`;
  }).join('') : `<p class="side-empty">${query ? '没有找到，换个词试试。' : '还没有聊天。<br>先认识一位新朋友吧。'}</p>`;
  $('member-list').innerHTML = people.length ? people.map(m => `<button class="person-button" data-action="profile" data-id="${m.id}">${avatar(m)}<span class="person-copy"><span class="person-name">${escapeHTML(m.name)}</span><span class="person-note">${escapeHTML(memberIntroduction(m))}</span></span>${icon('chevron-right')}</button>`).join('') : `<p class="side-empty">${query ? '没有找到这位朋友。' : '一个名字，一个头像。<br>其余的，边聊边认识。'}</p>`;
  $('conversation-list').hidden = sideTab !== 'chats';
  $('member-list').hidden = sideTab !== 'people';
  $('chats-tab').setAttribute('aria-selected', String(sideTab === 'chats'));
  $('people-tab').setAttribute('aria-selected', String(sideTab === 'people'));
  $('chats-tab').tabIndex = sideTab === 'chats' ? 0 : -1; $('people-tab').tabIndex = sideTab === 'people' ? 0 : -1;
  // 桌面导航与窄屏标签共享同一选择；缩放后不留下另一套高亮状态。
  document.querySelectorAll('.rail-navigation [aria-pressed]').forEach(button => {
    const active = button.dataset.action === `rail-${sideTab}`;
    button.classList.toggle('active', active); button.setAttribute('aria-pressed', String(active));
  });
}
function renderWelcome() {
  $('welcome').hidden = false; $('chat-body').hidden = true; $('composer-wrap').hidden = true; $('show-artifacts').hidden = true; $('show-activity').hidden = true;
  $('room-title').textContent = '有空，来坐坐'; $('room-subtitle').textContent = '从认识一位朋友开始';
  $('header-members').innerHTML = ''; $('header-avatar').innerHTML = ''; $('room-details').disabled = true;
  $('welcome-action').innerHTML = state.members.length ? `邀请朋友，开一个群 ${icon('plus')}` : `认识第一位朋友 ${icon('plus')}`;
}

// 草稿属于具体会话。切换聊天时保留输入和引用，回来后可以接着写。
function rememberDraft() {
  if (selected) drafts.set(selected, {body: $('message-input').value, reply: replyTo});
}
function sizeComposer() {
  const input = $('message-input'); input.style.height = 'auto';
  input.style.height = `${Math.min(160, input.scrollHeight)}px`;
  $('send').disabled = sending || !input.value.trim();
}
async function openRoom(id) {
  closeTransient();
  closeWorkspace(false);
  if (id !== selected) {
    rememberDraft(); closePanel(); selected = id; messages = []; renderedMessages = '';
    const draft = drafts.get(id); replyTo = draft?.reply || null;
    $('message-input').value = draft?.body || ''; renderReply(); sizeComposer();
  }
  localStorage.setItem('dropby-room', id); $('sidebar').classList.remove('open'); $('mobile-nav').setAttribute('aria-expanded','false'); renderSidebar();
  await loadConversation(id, false);
}
async function loadConversation(id, preserve, signal) {
  const sequence = ++loadSequence;
  const data = await api(`/api/conversations/${id}`, {signal});
  if (sequence !== loadSequence || selected !== id) return;
  const changedRoom = roomData?.conversation.id !== id;
  roomData = data;
  if (changedRoom || !preserve) { messages = data.messages; hasEarlier = data.has_earlier; }
  else {
    const combined = new Map(messages.map(m => [m.id, m]));
    for (const m of data.messages) combined.set(m.id, m);
    messages = [...combined.values()].filter(m => !m.id.startsWith('budget_')).sort((a, b) => a.seq - b.seq);
  }
  const c = data.conversation;
  $('chat-header').dataset.kind=c.kind;
  $('welcome').hidden = true; $('chat-body').hidden = false; $('composer-wrap').hidden = false;
  $('room-title').textContent = c.name; $('room-details').disabled = false;
  $('header-avatar').innerHTML = c.kind === 'direct' ? avatar(member(c.member_ids[0])) : groupAvatar(c);
  $('room-subtitle').textContent = c.kind === 'direct' ? '私聊 · AI 朋友' : `${c.member_ids.length + 1} 位成员 · 点击查看大家`;
  if (c.discussion_paused) $('room-subtitle').textContent += ' · 讨论已暂停';
  if (c.research_paused) $('room-subtitle').textContent += ' · 研究已停止';
  $('header-members').innerHTML = c.member_ids.map(id => `<button data-action="profile" data-id="${id}" aria-label="查看 ${escapeHTML(authorName(id))}">${avatar(member(id), true)}</button>`).join('');
  $('show-artifacts').hidden = false; $('artifact-count').textContent = data.artifacts.length || '';
  $('message-input').placeholder = data.messages.some(m => m.question && m.question_status === 'open') ? '写下你的回答，或者聊点别的…' : '说点什么…';
  $('earlier').hidden = !hasEarlier;
  renderActivity();
  $('show-activity').hidden = false;
  if (panelView === 'activity') showActivity(false);
  if (panelView.startsWith('evidence:')) {
    const run=data.tool_runs.find(r=>r.id===panelView.slice(9));
    if(run) await refreshEvidence(run);
    if(sequence!==loadSequence || selected!==id)return;
  }
  refreshPersonCard();
  if (panelView === 'artifacts' && $('panel-content').dataset.artifactListSignature !== JSON.stringify(data.artifacts.map(a => [a.id,a.version,a.title]))) {
    const top = $('panel-content').scrollTop; showArtifacts(); $('panel-content').scrollTop = top;
  }
  if (panelView === 'room') roomDetails(false);
  if (panelView.startsWith('profile:')) {
    const id = panelView.slice(8), current = member(id);
    const signature = JSON.stringify([current, roomData.member_states?.find(s => s.member_id === id),state.personal_focus?.[id]]);
    if ($('panel-content').dataset.profileSignature !== signature) { const top = $('panel-content').scrollTop; fullProfile(id); $('panel-content').scrollTop = top; }
  }
  // 新版只提示，不重建用户正在试用的页面；点击后才载入新的版本。
  const update = $('artifact-new-version');
  if (update && panelView.startsWith('artifact:')) {
    const latest = data.artifacts.find(a => a.id === panelView.slice(9));
    update.hidden = !latest || latest.version <= (currentVersions[0]?.version || 0);
    if (!update.hidden) update.textContent = `已有第 ${latest.version} 版 · 打开新版`;
  }
  renderMessages(!preserve || changedRoom);
  const last = messages.at(-1)?.seq || 0;
  if (!document.hidden && $('chat-body').getClientRects().length && $('chat-body').scrollHeight - $('chat-body').scrollTop - $('chat-body').clientHeight < 110) { seen[id] = last; localStorage.setItem('dropby-seen', JSON.stringify(seen)); }
}

function renderMessages(scrollToBottom) {
  const body = $('chat-body'), previousTop = body.scrollTop;
  const nearBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 110;
  const signature = JSON.stringify([messages.map(({reactions, ...message}) => message), state.profile, state.members.map(m => [m.id,m.name,m.avatar,m.avatar_icon,m.color])]);
  // SSE 也会报告后台活动。聊天内容没变时保留 DOM 和键盘焦点，避免打断正在点选的问题。
  if (signature === renderedMessages && !scrollToBottom) { syncLoadedReactions(); return; }
  renderedMessages = signature;
  let lastDate = '';
  const lastHumanSeq = Math.max(0, ...messages.filter(m => m.author_id === 'user').map(m => m.seq));
  // 只展示公开消息明确分享的成果版本。安静研究保存的作品留在成果集合里，
  // 不因数据库多了一份草稿，就替角色自动在群里发布内容。
  closeMessageMenu();
  $('messages').innerHTML = messages.length ? messages.map(m => {
    const date = dateLabel(m.created_at), divider = date !== lastDate ? `<div class="date-divider">${date}</div>` : ''; lastDate = date;
    if (m.author_id === 'system') {
      if (m.id.startsWith('budget_')) return '';
      if (m.body.startsWith('这次运行暂未完成')) return `${divider}<details class="system-message"><summary>${timeLabel(m.created_at)} 的一次行动中断了 · 查看当时记录</summary><p>${escapeHTML(m.body)}</p><button class="quiet" data-action="activity">查看动态</button></details>`;
      return `${divider}<div class="system-message" role="status">${escapeHTML(m.body)}</div>`;
    }
    const isUser = m.author_id === 'user', person = isUser ? state.profile : member(m.author_id);
    const quoted = messages.find(q => q.id === m.reply_to);
    const quote = m.reply_to ? `<button class="quote" data-action="jump-message" data-id="${m.reply_to}" aria-label="查看引用的消息"><strong>${escapeHTML(quoted ? authorName(quoted.author_id) : '较早的消息')}</strong><span>${escapeHTML(quoted ? quoted.body.slice(0,100) : '点击查看这段交流')}</span></button>` : '';
    const q = m.question, open = q && m.question_status === 'open' && m.seq > lastHumanSeq;
    const question = q ? `${m.body !== q.text ? `<span class="question-text">${m.body ? '\n\n' : ''}${escapeHTML(q.text)}</span>` : ''}${open ? `<div class="question-options">${q.options.map((option,i) => `<button class="question-option" data-action="answer-question" data-id="${m.id}" data-option="${i}">${escapeHTML(option)}${icon('chevron-right')}</button>`).join('')}</div><span class="question-hint">选一个，或直接在下面说说你的想法。聊点别的也可以。</span>` : m.question_status === 'answered' ? '<span class="question-closed">已回答</span>' : ''}` : '';
    return `${divider}<article class="message ${isUser ? 'user-message' : ''}" id="message-${m.id}" data-message-id="${m.id}">${isUser ? avatar(person) : `<button class="avatar-button" data-action="profile" data-id="${m.author_id}" aria-label="了解 ${escapeHTML(authorName(m.author_id))}">${avatar(person)}</button>`}<div class="message-content"><div class="message-meta"><strong>${escapeHTML(authorName(m.author_id))}</strong><time datetime="${m.created_at}">${timeLabel(m.created_at)}</time><button type="button" class="message-menu-trigger" popovertarget="message-menu" data-id="${m.id}" aria-label="消息操作" aria-haspopup="dialog" aria-controls="message-menu" aria-expanded="false">${icon('ellipsis')}</button></div><div class="message-bubble">${quote}${richText(m.body)}${question}</div>${m.artifacts?.length ? `<div class="message-artifacts">${m.artifacts.map(artifactCard).join('')}</div>` : ''}<div class="message-reactions" hidden></div></div></article>`;
  }).join('') : `<div class="empty-chat"><div class="empty-avatars">${roomData.conversation.member_ids.map(id => avatar(member(id))).join('')}</div><strong>${roomData.conversation.kind === 'direct' ? `和 ${escapeHTML(authorName(roomData.conversation.member_ids[0]))} 打个招呼` : '人到齐了，随便聊聊'}</strong><p>一个念头、一件小事、一个好奇的问题。<br>从哪里开始，都可以。</p></div>`;
  syncLoadedReactions();
  if (scrollToBottom || nearBottom) body.scrollTop = body.scrollHeight; else body.scrollTop = previousTop;
  updateLatestButton();
}
function updateLatestButton() {
  const body = $('chat-body'), away = body.scrollHeight - body.scrollTop - body.clientHeight >= 110;
  $('latest-messages').hidden = !away || !messages.length;
  if (!away && selected && !document.hidden && body.getClientRects().length) { seen[selected] = messages.at(-1)?.seq || 0; localStorage.setItem('dropby-seen', JSON.stringify(seen)); }
}
async function jumpToMessage(id) {
  const room = selected;
  // 引用可能在当前页之外；沿已有历史分页找到原消息，才能准确回到当时的交流。
  while (!messages.some(m => m.id === id) && hasEarlier) {
    const data = await api(`/api/conversations/${room}?before=${messages[0]?.seq || 0}`);
    if (room !== selected) return;
    if (!data.messages.length) break;
    const combined = new Map([...data.messages, ...messages].map(m => [m.id,m]));
    messages = [...combined.values()].sort((a,b) => a.seq-b.seq); hasEarlier = data.has_earlier;
  }
  if (room !== selected) return;
  closePanel(); renderMessages(false); $('earlier').hidden = !hasEarlier;
  const target = $(`message-${id}`);
  if (!target) { toast('这条引用暂时无法找到。'); return; }
  target.scrollIntoView({block:'center',behavior:window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth'});
  target.classList.add('highlight'); setTimeout(() => target.classList.remove('highlight'),1800);
}

function renderReply() {
  $('reply-preview').hidden = !replyTo;
  $('reply-preview').innerHTML = replyTo ? `<span>接 ${escapeHTML(authorName(replyTo.author_id))} 的话：${escapeHTML(replyTo.body.slice(0, 140))}</span><button class="icon-button" data-action="clear-reply" aria-label="取消引用">${icon('x')}</button>` : '';
}
function showMentions() {
  const text = $('message-input').value.slice(0, $('message-input').selectionStart), match = text.match(/@([^\s@]{0,24})$/);
  if (!match || !roomData) { $('mention-options').hidden = true; return; }
  const choices = roomData.conversation.member_ids.map(member).filter(m => m.name.includes(match[1]));
  $('mention-options').innerHTML = choices.map(m => `<button type="button" data-action="mention" data-id="${m.id}">${avatar(m, true)}${escapeHTML(m.name)}</button>`).join('');
  $('mention-options').hidden = !choices.length;
}

// 详情是聊天旁的一个页面，不再用遮罩和模态弹窗打断交流。
function showPanel(title, description, body, view = '') {
  closeTransient();
  const opening=$('inspector').hidden;
  if (opening) panelInvoker=document.activeElement;
  closeWorkspace(false);
  panelRequest++; panelView=view;
  const app=document.querySelector('.app');
  app.classList.toggle('artifact-open',view.startsWith('artifact:'));
  if (!view.startsWith('artifact:')) app.classList.remove('artifact-expanded');
  $('panel-title').textContent=title;
  $('panel-content').innerHTML=`${description ? `<p class="panel-description">${escapeHTML(description)}</p>` : ''}${body}`;
  $('inspector').hidden=false; app.classList.add('panel-open'); $('sidebar').classList.remove('open'); $('mobile-nav').setAttribute('aria-expanded','false');
  if (opening) $('panel-title').focus();
}
function closePanel(restoreFocus = false) {
  $('inspector').hidden=true; $('panel-content').replaceChildren(); document.querySelector('.app').classList.remove('panel-open','artifact-open','artifact-expanded');
  panelRequest++; panelView=''; avatarRequest++; avatarLoading=false;
  if(restoreFocus) (panelInvoker?.isConnected && panelInvoker.getClientRects().length ? panelInvoker : $('message-input')).focus();
}

function formError(form, error) { const el = form.querySelector('.form-error'); el.textContent = error.message; el.hidden = false; }
function pending(form, text) { const button = form.querySelector('button[type=submit]'); const label = button.textContent; button.textContent = text; button.disabled = true; return () => { button.textContent = label; button.disabled = false; }; }

function avatarEditor(m = {}) {
  avatarRequest++; avatarLoading = false;
  avatarData = m.avatar || ''; avatarIcon = m.avatar_icon || (m.id ? presetFor(m) : 'portrait-cat'); avatarColor = m.color || 'jade';
  return `<div class="avatar-editor"><label class="avatar-edit-preview" for="avatar-file" title="更换头像"><span id="avatar-preview">${avatar({avatar:avatarData,avatar_icon:avatarIcon,color:avatarColor})}</span><span class="avatar-camera">${icon('image')}</span></label><div class="avatar-edit-copy"><strong>选一个喜欢的样子</strong><small>照片会在本机裁剪，保存后生效</small><label class="avatar-upload" for="avatar-file">${icon('upload')}更换头像</label></div><input class="sr-only" id="avatar-file" type="file" aria-label="上传头像图片" accept="image/png,image/jpeg"></div>
    <details class="avatar-options"><summary>默认头像与颜色 ${icon('chevron-down')}</summary><fieldset class="avatar-gallery"><legend class="sr-only">默认头像</legend><div class="avatar-grid">${avatarPresets.map(([id,label]) => `<button type="button" class="avatar-choice" data-action="avatar-preset" data-id="${id}" aria-label="${label}头像" aria-pressed="${!avatarData && id === avatarIcon}">${avatar({avatar_icon:id,color:avatarColor})}</button>`).join('')}</div></fieldset><div class="color-picker" role="group" aria-label="图标头像颜色">${colors.map(([color,label]) => `<label class="color-choice ${color}"><input type="radio" name="color" value="${color}" ${color === avatarColor ? 'checked' : ''} aria-label="${label}"><span></span></label>`).join('')}</div></details>`;
}
function renderAvatarPreview() {
  if ($('group-avatar-preview')) { $('group-avatar-preview').innerHTML = groupAvatar({avatar:avatarData,member_ids:groupSelection}); return; }
  if (!$('avatar-preview')) return;
  $('avatar-preview').innerHTML = avatar({avatar:avatarData,avatar_icon:avatarIcon,color:avatarColor});
  for (const choice of document.querySelectorAll('.avatar-choice')) {
    choice.setAttribute('aria-pressed',String(!avatarData && choice.dataset.id === avatarIcon));
    choice.innerHTML = avatar({avatar_icon:choice.dataset.id,color:avatarColor});
  }
}
function newMember() {
  showWorkspace('认识新朋友', '一个名字就可以开始，TA 会带着自己的特质来打招呼。', `<form id="member-form">${avatarEditor()}<label class="field"><span>名字</span><input name="name" maxlength="24" required placeholder="怎么称呼这位朋友？" autocomplete="off"></label><label class="field"><span>大致方向 · 可选</span><textarea name="direction" rows="2" placeholder="例如：喜欢简洁设计的设计师"></textarea><small class="field-hint">留空也可以。认识之后，随时能调整 TA 的特质。</small></label><div class="form-error" hidden role="alert"></div><button class="primary full-width" type="submit">开始聊天 ${icon('arrow-up')}</button></form>`, 'new-member');
  $('workspace-content').querySelector('input[name=name]').focus();
}
const domainExcerpt = text => String(text ?? '').replace(/\s+/g, ' ').trim();
// 可见章节只是视图。每篇领域始终留在隐藏行里，保存和恢复都读这些行。
function domainProfileStore({domain, profile} = {}) {
  return `<div data-domain-profile hidden><input name="domain" value="${escapeHTML(domain)}"><textarea name="domain_profile">${escapeHTML(profile)}</textarea></div>`;
}
function syncDomainEditor() {
  const form = $('profile-form');
  if (!form || domainEditorIndex == null) return;
  const row = form.querySelectorAll('[data-domain-profile]')[domainEditorIndex];
  const title = form.querySelector('[data-domain-draft="domain"]');
  const body = form.querySelector('[data-domain-draft="profile"]');
  if (!row || !title || !body) return;
  row.querySelector('[name=domain]').value = title.value;
  row.querySelector('[name=domain_profile]').value = body.value;
}
function collectDomainProfiles(form) {
  syncDomainEditor();
  for (const row of [...form.querySelectorAll('[data-domain-profile]')]) {
    if (!row.querySelector('[name=domain]').value.trim() && !row.querySelector('[name=domain_profile]').value.trim()) row.remove();
  }
  const domains = Array.from(form.querySelectorAll('[data-domain-profile]'), row => ({
    domain: row.querySelector('[name=domain]').value.trim(),
    profile: row.querySelector('[name=domain_profile]').value.trim(),
  }));
  const partial = domains.findIndex(item => !item.domain || !item.profile);
  if (partial >= 0) return {error: '领域名称和画像正文都需要填写', index: partial};
  const seen = new Set();
  for (let i = 0; i < domains.length; i++) {
    if (seen.has(domains[i].domain)) return {error: `领域名称重复：${domains[i].domain}`, index: i};
    seen.add(domains[i].domain);
  }
  return {domains};
}
function renderDomainEditor() {
  const form = $('profile-form');
  const view = form?.querySelector('[data-domain-view]');
  if (!view) return;
  const rows = [...form.querySelectorAll('[data-domain-profile]')];
  if (domainEditorIndex != null && !rows[domainEditorIndex]) { domainEditorIndex = null; domainEditorFresh = false; }
  if (domainEditorIndex == null) {
    const items = rows.map((row, index) => {
      const domain = row.querySelector('[name=domain]').value.trim();
      const profile = domainExcerpt(row.querySelector('[name=domain_profile]').value);
      return `<button class="domain-row" type="button" data-action="open-domain-profile" data-index="${index}"><span class="domain-row-copy"><strong>${escapeHTML(domain || '未命名领域')}</strong><span>${escapeHTML(profile || '还没有写下这篇')}</span></span>${icon('chevron-right')}</button>`;
    }).join('');
    view.innerHTML = `<h3 class="section-label">领域画像</h3><p class="domain-hint">聊到这些事时，TA 才会打开对应的一篇。可以留空。</p>${items ? `<div class="domain-group">${items}</div>` : '<p class="domain-empty">还没有领域画像。</p>'}<button class="domain-add" type="button" data-action="add-domain-profile">${icon('plus')}添加领域</button>`;
    return;
  }
  const row = rows[domainEditorIndex];
  const domain = row.querySelector('[name=domain]').value;
  const profile = row.querySelector('[name=domain_profile]').value;
  const title = domain.trim();
  const name = form.elements.name.value.trim() || '这位朋友';
  view.innerHTML = `<button class="domain-back" type="button" data-action="domain-profiles-back">${icon('arrow-left')}全部领域</button><p class="domain-note">${escapeHTML(name)} 的自我介绍留在全部领域里。这里只改这一篇。</p><label class="field"><span>领域</span><input data-domain-draft="domain" value="${escapeHTML(domain)}" placeholder="例如：写作与评论" autocomplete="off"></label><label class="field"><span>这篇画像</span><textarea data-domain-draft="profile" rows="8" placeholder="具体的品味、判断和做法">${escapeHTML(profile)}</textarea><small class="field-hint">聊到这个领域时，TA 才会读到这段。</small></label><button class="domain-remove" type="button" data-action="remove-domain-profile">${title ? `移除「${escapeHTML(title)}」` : '移除这篇'}</button>`;
}
function domainProfilesView(profiles = [], showEmpty = false) {
  const list = profiles || [];
  if (!list.length && !showEmpty) return '';
  const body = list.length ? list.map(({domain, profile}) => `<details class="domain-read"><summary><span class="domain-read-copy"><strong>${escapeHTML(domain)}</strong><span class="domain-excerpt">${escapeHTML(domainExcerpt(profile))}</span></span>${icon('chevron-right')}</summary><div class="profile-body">${escapeHTML(profile)}</div></details>`).join('') : '<p class="domain-empty">还没有领域画像。可以留空。</p>';
  return `<h3 class="section-label">领域画像</h3><p class="domain-hint">聊到这些事时，TA 才会打开对应的一篇。</p>${body}`;
}
function editProfile(id) {
  const m = member(id); if (!m) return;
  const domains = m.domain_profiles || [];
  domainEditorIndex = null; domainEditorFresh = false;
  showWorkspace('编辑朋友', '', `<form id="profile-form" data-id="${m.id}">
    ${avatarEditor(m)}
    <label class="field"><span>名字</span><input name="name" maxlength="24" required value="${escapeHTML(m.name)}" autocomplete="off"></label>
    <label class="field"><span>自我介绍与特质</span><textarea name="profile" rows="8">${escapeHTML(m.profile)}</textarea><small class="field-hint">这就是 TA 交流和行动时使用的人物说明。性格、兴趣、判断和相处方式，都可以直接调整。</small></label>
    <div data-domain-profiles hidden>${domains.map(domainProfileStore).join('')}</div>
    <div data-domain-view></div>
    <div class="form-error" hidden role="alert"></div><div class="panel-footer"><button class="secondary" type="button" data-action="close-workspace">取消</button><button class="primary" type="submit">保存修改</button></div>
  </form>`, 'edit-profile');
  renderDomainEditor();
  // 只提交用户实际改过的字段，避免头像修改覆盖同时从聊天中学到的新偏好。
  profileBaseline = {name:m.name.trim(),profile:m.profile.trim(),domain_profiles:domains,profile_revision:m.profile_revision || 0,color:avatarColor,avatar:avatarData,avatar_icon:avatarIcon};
}

// 历史按需读取。恢复先放回同一个编辑器，用户仍可修改，保存仍基于当前版本。
async function profileHistory(id) {
  const changes = await api(`/api/members/${id}/profile-history`);
  const origins = {created:'初次相识',edit:'你编辑了特质',instruction:'聊天中调整',answer:'情景问答',reflection:'旧版自动更新',previous:'原有特质'};
  showPanel('特质的变化', '', changes.length ? changes.map(change => `<details class="profile-change"><summary>${escapeHTML(origins[change.origin] || '特质更新')} · ${escapeHTML(dateLabel(change.created_at))}</summary><p>${escapeHTML(change.change_note)}</p><div class="profile-body">${escapeHTML(change.profile)}</div>${domainProfilesView(change.domain_profiles)}<button class="secondary" data-action="restore-profile" data-id="${id}" data-revision="${change.revision}">用这一版继续编辑</button></details>`).join('') : '<p class="empty-note">还没有特质变化。</p>', `profile-history:${id}`);
}
async function restoreProfile(id, revision) {
  const changes = await api(`/api/members/${id}/profile-history`);
  const change = changes.find(item => item.revision === Number(revision));
  if (!change) return;
  editProfile(id);
  const form = $('profile-form');
  form.elements.profile.value = change.profile;
  form.querySelector('[data-domain-profiles]').innerHTML = (change.domain_profiles || []).map(domainProfileStore).join('');
  domainEditorIndex = null; domainEditorFresh = false;
  renderDomainEditor();
}
async function openDirect(id) {
  const c = await post('/api/conversations', {name:'',kind:'direct',member_ids:[id]});
  closePanel(); await refresh(); await openRoom(c.id); return c;
}
async function askPreference(id, button) {
  button.disabled = true;
  try {
    const c = await openDirect(id);
    await post(`/api/members/${id}/question`, {conversation_id:c.id});
    await loadConversation(c.id,true); $('chat-body').scrollTop = $('chat-body').scrollHeight; $('message-input').focus();
  } catch (error) { button.disabled = false; throw error; }
}

function newGroup() { startGroup(); }

function focusBlock(id) {
  const focus = roomData?.member_states?.find(s => s.member_id === id);
  const personal=state.personal_focus?.[id];
  const own=personal?.body ? `<div class="focus-block"><h3 class="section-label">${icon('activity')}最近在意</h3><div class="focus-body">${escapeHTML(personal.body)}</div></div>` : '';
  if (!focus?.body) return own;
  return own+`<div class="focus-block"><h3 class="section-label">${icon('smile')}在这段聊天里</h3><div class="focus-body">${escapeHTML(focus.body)}</div>${focus.source_ids?.length ? `<div class="focus-sources">${focus.source_ids.map((source,i) => `<button data-action="${source.startsWith('tool_') ? 'evidence' : 'jump-message'}" data-id="${source}">${source.startsWith('tool_') ? '查看相关行动' : '回到相关交流'}${focus.source_ids.length > 1 ? ` ${i+1}` : ''} ↗</button>`).join('')}</div>` : ''}</div>`;
}
function fullProfile(id) {
  const m = member(id); if (!m) return;
  showPanel('朋友资料', '', `<div class="profile-head">${avatar(m)}<h3>${escapeHTML(m.name)}</h3><p>AI 朋友 · ${escapeHTML(dateLabel(m.created_at))} 相识</p></div><div class="profile-actions"><button class="primary" data-action="direct" data-id="${m.id}">${icon('message-circle')}单独聊聊</button><button class="secondary" data-action="edit-profile" data-id="${m.id}">${icon('pencil')}编辑</button></div>${focusBlock(id)}${capabilities(m)}<h3 class="section-label">自我介绍与特质</h3><div class="profile-body">${escapeHTML(m.profile)}</div>${domainProfilesView(m.domain_profiles, true)}<p class="profile-caption">你可以直接编辑、通过情景问答调整，或在聊天中明确要求修改特质。</p><div class="panel-nav"><button data-action="preference" data-id="${m.id}">${icon('circle-help')}用一个情景调整特质${icon('chevron-right')}</button><button data-action="profile-history" data-id="${m.id}">${icon('chevron-right')}看看特质的变化</button></div>`, `profile:${id}`);
  $('panel-content').dataset.profileSignature = JSON.stringify([m, roomData?.member_states?.find(s => s.member_id === id),state.personal_focus?.[id]]);
}
function roomDetails(scroll = true) {
  if (!roomData) return;
  const c = roomData.conversation;
  if (c.kind === 'direct') { profile(c.member_ids[0]); return; }
  const signature=JSON.stringify([c,state.members,roomData.member_states]);
  if (!scroll && $('panel-content').dataset.roomSignature===signature) return;
  const top=$('panel-content').scrollTop;
  const people=c.member_ids.map(id=>`<button class="person-button" data-action="profile" data-id="${id}">${avatar(member(id))}<span class="person-copy"><span class="person-name">${escapeHTML(authorName(id))}</span><span class="person-note">${escapeHTML(memberIntroduction(member(id)))}</span></span>${icon('chevron-right')}</button>`).join('');
  const roles=(c.responsibilities || []).map(r=>`<div class="responsibility"><strong>${escapeHTML(authorName(r.member_id))}<small>${r.scope_id ? '这件事' : '长期约定'}</small></strong><p>${escapeHTML(r.body)}</p>${r.source_id ? `<button class="quiet" data-action="jump-message" data-id="${r.source_id}">回到这次约定 ${icon('arrow-up')}</button>` : ''}</div>`).join('');
  showPanel('聊天详情', '', `<div class="room-details-head">${groupAvatar(c)}<h3>${escapeHTML(c.name)}</h3><p>${c.member_ids.length+1} 位成员</p></div><button class="secondary full-width" data-action="edit-group">${icon('pencil')}编辑群资料与成员</button><h3 class="section-label">群成员</h3><div class="person-button own-member">${avatar(state.profile)}<span class="person-copy"><span class="person-name">${escapeHTML(authorName('user'))}</span><span class="person-note">我</span></span></div>${people}<h3 class="section-label">在这里的约定</h3>${roles || '<p class="empty-note">还没有特别的分工。需要一起做事时，在聊天里告诉大家就好。</p>'}<p class="profile-caption">例如「@小舟，这次你帮我们安排一下」。约定会留在这里，平时大家仍按自己的方式聊天。</p><div class="panel-nav"><button data-action="activity">${icon('activity')}朋友动态${icon('chevron-right')}</button><button data-action="artifacts">${icon('notebook')}聊天成果${icon('chevron-right')}</button></div>`, 'room');
  $('panel-content').dataset.roomSignature=signature;
  if (!scroll) $('panel-content').scrollTop=top;
}
// 工具清单只展示当前真正可用的能力；不让未接通的网页服务看起来已经可用。
function capabilities(m) {
  const labels = ['对话选择题', '文字与网页创作'];
  if (state.search_ready && m.tools?.includes('search')) labels.push('搜索与阅读');
  labels.push('检索与记住经历', '定时跟进');
  if (roomData?.conversation.kind === 'group' && roomData.conversation.responsibilities?.some(r=>r.member_id===m.id)) labels.push('参与分工与跟进');
  return `<div class="capability-block"><h3 class="section-label">可以一起做</h3><div class="capability-list">${labels.map(label => `<span>${escapeHTML(label)}</span>`).join('')}</div>`;
}
function artifactCard(a) {
  const html = a.format === 'html';
  return `<button class="artifact-card" data-action="artifact" data-id="${escapeHTML(a.id)}" data-version="${a.version}"><span class="artifact-card-icon" aria-hidden="true">${icon(html ? 'external-link' : 'notebook')}</span><span class="artifact-card-copy"><span class="artifact-kind">${html ? '网页' : '文稿'} · 第 ${a.version} 版</span><strong>${escapeHTML(a.title)}</strong><span class="artifact-card-caption">${html ? '打开页面，试试里面的想法' : '打开阅读，也可以接着修改'}</span></span>${icon('chevron-right')}</button>`;
}
function showArtifacts() {
  if (!roomData) return;
  showPanel('一起留下的东西', '从聊天里长出来的作品和发现，都可以继续修改。', roomData.artifacts.length ? roomData.artifacts.map(a => `<div class="artifact-list-entry">${artifactCard(a)}<p>${escapeHTML(authorName(a.member_id))} · ${dateLabel(a.created_at)} ${timeLabel(a.created_at)}</p></div>`).join('') : '<p class="panel-description">还没有保存的成果。等一个念头长出形状，再把它留在这里。</p>', 'artifacts');
  $('panel-content').dataset.artifactListSignature = JSON.stringify(roomData.artifacts.map(a => [a.id,a.version,a.title]));
}
async function openArtifact(id, version = 0) {
  const room = selected;
  showPanel('打开成果', '', '<p class="panel-description" role="status">正在读取保存的版本…</p>', `artifact:${id}`);
  const request = panelRequest;
  const data = await api(`/api/artifacts/${encodeURIComponent(id)}`);
  // 用户可能已切到别的群或关闭详情；迟到的成果不能把新的页面覆盖掉。
  if (request !== panelRequest || selected !== room) return;
  if (!data.versions?.length) throw new Error('这个成果暂时没有可查看的版本。');
  currentVersions = data.versions; artifactMode = 'preview';
  const index = version ? currentVersions.findIndex(a => a.version === version) : 0;
  if (index < 0) throw new Error('这条消息分享的版本暂时无法读取。');
  renderArtifact(index);
}
function renderArtifact(index = 0) {
  const a = currentVersions[index]; if (!a) return;
  currentArtifactIndex = index;
  const html = a.format === 'html', base = `/api/artifacts/${encodeURIComponent(a.id)}`;
  const tabs = html ? `<div class="artifact-tabs" role="tablist" aria-label="成果查看方式"><button id="artifact-preview-tab" role="tab" data-action="artifact-tab" data-mode="preview" aria-controls="artifact-preview-pane">预览</button><button id="artifact-source-tab" role="tab" data-action="artifact-tab" data-mode="source" aria-controls="artifact-source-pane">源代码</button></div>` : '';
  // 生成页面始终位于无同源权限的 sandbox。源代码仅作为转义文本展示，不能进入宿主页 DOM。
  // 切换预览/源码只隐藏已有节点，避免反复重建 iframe 丢失正在试用的小工具状态。
  const content = html ? `<div id="artifact-preview-pane" role="tabpanel" aria-labelledby="artifact-preview-tab"><iframe class="artifact-preview" title="${escapeHTML(a.title)} · 第 ${a.version} 版预览" src="${base}/preview?version=${a.version}" sandbox="allow-scripts" referrerpolicy="no-referrer"></iframe><p class="artifact-preview-note">预览中的修改不会自动保存。想调整作品，可以继续在聊天里说。</p></div><div id="artifact-source-pane" role="tabpanel" aria-labelledby="artifact-source-tab" hidden><div class="source-actions"><button class="quiet" data-action="copy-artifact">复制源代码</button></div><pre class="artifact-source"><code>${escapeHTML(a.content)}</code></pre></div>` : `<div class="artifact-content">${richText(a.content)}</div>`;
  const versions=currentVersions.length > 1 ? `<details class="version-menu"><summary id="version-trigger" aria-label="选择成果版本">第 ${a.version} 版 ${icon('chevron-down')}</summary><div class="version-options" role="group" aria-label="已保存的版本">${currentVersions.map((v,i)=>`<button data-action="artifact-version" data-index="${i}" aria-pressed="${i===index}"><span>${icon(i===index?'check':'notebook')}<strong>第 ${v.version} 版${i===0?' · 最新':''}</strong></span><time>${dateLabel(v.created_at)} ${timeLabel(v.created_at)}</time></button>`).join('')}</div></details>` : `<span class="single-version">第 ${a.version} 版</span>`;
  showPanel(a.title, '', `<div class="artifact-meta"><button class="quiet" data-action="artifacts">${icon('arrow-left')}成果</button><span>${escapeHTML(authorName(a.member_id))} · ${dateLabel(a.created_at)}</span></div><div class="artifact-toolbar">${versions}${tabs}<span class="toolbar-spacer"></span><a class="artifact-download" href="${base}/download?version=${a.version}" download aria-label="下载第 ${a.version} 版${html?' HTML':'文稿'}">${icon('arrow-down')}<span>下载</span></a><button class="icon-button artifact-expand" data-action="expand-artifact" aria-label="展开预览" aria-pressed="${document.querySelector('.app').classList.contains('artifact-expanded')}">${icon('expand')}</button></div><button id="artifact-new-version" class="artifact-update" data-action="artifact" data-id="${escapeHTML(a.id)}" hidden></button>${content}<div class="panel-footer artifact-footer"><button class="secondary" data-action="continue-artifact" data-id="${escapeHTML(a.id)}">${icon('message-circle')}接着聊这个成果</button></div>`, `artifact:${a.id}`);
  if (html) selectArtifactTab(artifactMode);
}
// 从详情回到聊天只是补上一句引子，用户原来写到一半的内容必须保留。
function composeFollowup(prompt) {
  const input = $('message-input');
  const next = input.value ? `${input.value}\n\n${prompt}` : prompt;
  closePanel();
  if (next.length > input.maxLength) { toast('草稿已经很长了，先整理一下再继续这个话题。'); input.focus(); return; }
  input.value = next; sizeComposer(); rememberDraft(); input.focus(); input.setSelectionRange(next.length, next.length);
}
function selectArtifactTab(mode) {
  artifactMode = mode === 'source' ? 'source' : 'preview';
  for (const name of ['preview','source']) {
    const tab = $(`artifact-${name}-tab`), pane = $(`artifact-${name}-pane`);
    if (!tab || !pane) return;
    const active = name === artifactMode;
    tab.setAttribute('aria-selected', String(active)); tab.tabIndex = active ? 0 : -1; pane.hidden = !active;
  }
}

document.addEventListener('click', async event => {
  const button = event.target.closest('[data-action]'); if (!button) return;
  const {action,id} = button.dataset;
  try {
    if (action === 'open-room') await openRoom(id);
    if (action === 'profile') profile(id, button);
    if (action === 'edit-profile') editProfile(id);
    if (action === 'add-domain-profile') {
      const form = button.form;
      syncDomainEditor();
      const domains = form.querySelector('[data-domain-profiles]');
      domains.insertAdjacentHTML('beforeend', domainProfileStore());
      domainEditorIndex = domains.querySelectorAll('[data-domain-profile]').length - 1;
      domainEditorFresh = true;
      renderDomainEditor();
      form.querySelector('[data-domain-draft="domain"]')?.focus();
    }
    if (action === 'open-domain-profile') {
      syncDomainEditor();
      domainEditorIndex = Number(button.dataset.index);
      domainEditorFresh = false;
      renderDomainEditor();
      button.form.querySelector('[data-domain-draft="domain"]')?.focus();
    }
    if (action === 'domain-profiles-back') {
      const form = button.form;
      syncDomainEditor();
      const row = form.querySelectorAll('[data-domain-profile]')[domainEditorIndex];
      if (row && domainEditorFresh && !row.querySelector('[name=domain]').value.trim() && !row.querySelector('[name=domain_profile]').value.trim()) row.remove();
      domainEditorIndex = null; domainEditorFresh = false;
      renderDomainEditor();
      form.querySelector('[data-action="add-domain-profile"]')?.focus();
    }
    if (action === 'remove-domain-profile') {
      const form = button.form;
      form.querySelectorAll('[data-domain-profile]')[domainEditorIndex]?.remove();
      domainEditorIndex = null; domainEditorFresh = false;
      renderDomainEditor();
      form.querySelector('[data-action="add-domain-profile"]')?.focus();
    }
    if (action === 'profile-history') await profileHistory(id);
    if (action === 'restore-profile') await restoreProfile(id, button.dataset.revision);
    if (action === 'close-panel') closePanel(true);
    if (action === 'activity') showActivity();
    if (action === 'artifacts') showArtifacts();
    if (action === 'preference') await askPreference(id,button);
    if (action === 'avatar-preset') {
      avatarRequest++;
      if (avatarLoading) { avatarLoading = false; const save = button.closest('form')?.querySelector('button[type=submit]'); if (save) save.disabled = false; }
      avatarIcon = id; avatarData = ''; renderAvatarPreview();
    }
    if (action === 'continue-member') {
      composeFollowup(`@${authorName(id)} 刚才的话题，你有什么想法？`);
      replyTo=messages.find(m=>m.id===button.dataset.source) || null; renderReply();
    }
    if (action === 'continue-work') { button.disabled = true; try { await post('/api/work/' + encodeURIComponent(id) + '/resume', {}); await refresh(); } finally { button.disabled = false; } }
    if (action === 'reply') { replyTo = messages.find(m => m.id === id); renderReply(); $('message-input').focus(); }
    if (action === 'clear-reply') { replyTo = null; renderReply(); }
    if (action === 'jump-message') await jumpToMessage(id);
    if (action === 'answer-question') {
      const question = messages.find(m => m.id === id);
      if (!question || question.question_status !== 'open' || sending) return;
      button.disabled = true;
      await sendMessage(question.question.options[Number(button.dataset.option)],id,false);
    }
    if (action === 'mention') {
      const input = $('message-input'), caret = input.selectionStart;
      const prefix = input.value.slice(0,caret).replace(/@[^\s@]{0,24}$/, `@${authorName(id)} `);
      input.value = prefix + input.value.slice(caret); input.focus(); input.setSelectionRange(prefix.length,prefix.length); $('mention-options').hidden = true; sizeComposer();
    }
    if (action === 'direct') { button.disabled = true; await openDirect(id); }
    if (action === 'artifact') await openArtifact(id, Number(button.dataset.version || 0));
    if (action === 'artifact-tab') selectArtifactTab(button.dataset.mode);
    if (action === 'copy-artifact') { await navigator.clipboard.writeText(currentVersions[currentArtifactIndex].content); toast('源代码已复制。'); }
    if (action === 'continue-artifact') { const a = currentVersions[currentArtifactIndex]; composeFollowup(`关于《${a.title}》第 ${a.version} 版，`); }
    if (action === 'evidence') await showEvidence(id);

  } catch (error) { toast(error.message); button.disabled = false; }
});

document.addEventListener('input', event => {
  if (!event.target.matches('[data-domain-draft]')) return;
  syncDomainEditor();
  if (event.target.dataset.domainDraft !== 'domain') return;
  const remove = event.target.form.querySelector('[data-action="remove-domain-profile"]');
  if (!remove) return;
  const title = event.target.value.trim();
  remove.textContent = title ? `移除「${title}」` : '移除这篇';
});
document.addEventListener('submit', async event => {
  const form = event.target;
  if (!['member-form','profile-form'].includes(form.id)) return;
  event.preventDefault(); if (avatarLoading) return;
  const reset = pending(form, form.id === 'profile-form' ? '正在保存…' : '正在准备…');
  try {
    const data = new FormData(form);
    if (form.id === 'member-form') {
      const m = await post('/api/members', {name:data.get('name').trim(),direction:data.get('direction').trim(),color:avatarColor,avatar:avatarData,avatar_icon:avatarIcon});
      await openDirect(m.id); $('message-input').focus();
    } else if (form.id === 'profile-form') {
      const values = {name:data.get('name').trim(),profile:data.get('profile').trim(),color:avatarColor,avatar:avatarData,avatar_icon:avatarIcon};
      const changes = Object.fromEntries(Object.entries(values).filter(([key,value]) => value !== profileBaseline?.[key]));
      const collected = collectDomainProfiles(form);
      if (collected.error) {
        domainEditorIndex = collected.index; domainEditorFresh = false;
        renderDomainEditor();
        formError(form, {message: collected.error});
        form.querySelector('[data-domain-draft="domain"]')?.focus();
        reset();
        return;
      }
      const domains = collected.domains;
      if (JSON.stringify(domains) !== JSON.stringify(profileBaseline.domain_profiles)) changes.domain_profiles = domains;
      if ('profile' in changes || 'domain_profiles' in changes) changes.profile_revision = profileBaseline.profile_revision;
      if (Object.keys(changes).length) await api(`/api/members/${form.dataset.id}`, {method:'PATCH',body:JSON.stringify(changes)});
      await refresh(); if (form.isConnected) { closeWorkspace(true); toast('修改已保存。'); }

    }
  } catch (error) { if (form.isConnected) { formError(form,error); reset(); } else toast(error.message); }
});

// 只发送裁剪后的缩略图，原始照片和 EXIF 信息保留在本机。
document.addEventListener('change', async event => {
  if (event.target.id === 'avatar-file') {
    const file = event.target.files[0], form = event.target.closest('form');
    // 清空文件值后，再次选择同一张照片也会触发 change。
    event.target.value = ''; if (!file) return;
    const request = ++avatarRequest, save = form.querySelector('button[type=submit]');
    avatarLoading = true; save.disabled = true; form.querySelector('.form-error').hidden=true;
    try {
      if (!['image/png','image/jpeg'].includes(file.type) || file.size > 10*1024*1024) throw new Error('请选择 10MB 以内的 PNG 或 JPEG 图片。');
      const cropped = await cropAvatar(file, request);
      if (cropped && request === avatarRequest && form.isConnected) { avatarData = cropped; renderAvatarPreview(); }
    } catch (error) { if (request === avatarRequest && form.isConnected) formError(form,error); }
    finally {
      if (request === avatarRequest) { avatarLoading = false; if (form.isConnected) save.disabled = false; }
    }
  }
  if (event.target.name === 'color') { avatarColor = event.target.value; renderAvatarPreview(); }
  if (event.target.id === 'artifact-version') renderArtifact(Number(event.target.value));
});
document.addEventListener('keydown', event => {
  if (event.target.matches('[data-action="artifact-tab"]') && ['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) {
    event.preventDefault();
    const next = event.key === 'Home' ? 'preview' : event.key === 'End' ? 'source' : artifactMode === 'preview' ? 'source' : 'preview';
    selectArtifactTab(next); $(`artifact-${next}-tab`).focus();
  }
  if (event.key === 'Escape') {
    if ($('crop-dialog').open) return;
    if (closeTransient(true)) { event.preventDefault(); return; }
    if (!$('mention-options').hidden) $('mention-options').hidden = true;
    else if (document.querySelector('.version-menu[open]')) { const menu=document.querySelector('.version-menu[open]'); menu.open=false; menu.querySelector('summary').focus(); }
    else if (!$('inspector').hidden) closePanel(true);
    else if (!$('workspace-page').hidden) closeWorkspace(true);
    else { $('sidebar').classList.remove('open'); $('mobile-nav').setAttribute('aria-expanded','false'); }
  }
});

async function sendMessage(body, quotedID = '', clearDraft = true) {
  if (!selected || !body.trim() || sending) return;
  const id = selected, original = $('message-input').value;
  const mentions = roomData.conversation.member_ids.filter(memberID => {
    const name = authorName(memberID).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    return new RegExp(`(^|\\s)@${name}(?=$|\\s|[，。！？：:,!?])`).test(body);
  });
  sending = true; sizeComposer();
  try {
    await post(`/api/conversations/${id}/messages`, {body:body.trim(),reply_to:quotedID,mentions});
    if (id === selected) {
      if (clearDraft && $('message-input').value === original) { $('message-input').value = ''; drafts.delete(id); replyTo = null; renderReply(); }
      $('mention-options').hidden = true;
      await loadConversation(id,true); $('chat-body').scrollTop = $('chat-body').scrollHeight; updateLatestButton();
    } else if (clearDraft && drafts.get(id)?.body === original) drafts.delete(id);
    scheduleRefresh();
  } finally { sending = false; sizeComposer(); if (id === selected) $('message-input').focus(); }
}
$('composer').addEventListener('submit', async event => {
  event.preventDefault();
  try { await sendMessage($('message-input').value,replyTo?.id || ''); } catch (error) { toast(error.message); }
});
$('message-input').addEventListener('keydown', event => {
  // 输入法选词用的回车不发送消息；确认组词后再按回车才发送。
  if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && event.keyCode !== 229) {
    event.preventDefault(); if (!$('send').disabled) $('composer').requestSubmit();
  }
});
$('message-input').addEventListener('input', () => { showMentions(); sizeComposer(); rememberDraft(); });
$('mention-trigger').onclick = () => {
  const input = $('message-input'); input.value += (input.value && !/\s$/.test(input.value) ? ' ' : '') + '@';
  input.focus(); input.setSelectionRange(input.value.length,input.value.length); showMentions(); sizeComposer();
};
$('new-member').onclick = () => startChat();
$('welcome-action').onclick = () => state.members.length ? startGroup() : newMember();
$('show-artifacts').onclick = showArtifacts; $('show-activity').onclick = () => showActivity();
$('room-details').onclick = () => roomDetails();
$('close-sidebar').onclick = () => { $('sidebar').classList.remove('open'); $('mobile-nav').setAttribute('aria-expanded','false'); };
$('chats-tab').onclick = () => { sideTab = 'chats'; renderSidebar(); };
$('people-tab').onclick = () => { sideTab = 'people'; renderSidebar(); };
$('chat-search').addEventListener('input',renderSidebar);
for (const tab of [$('chats-tab'),$('people-tab')]) tab.addEventListener('keydown', event => {
  if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
  event.preventDefault(); const next = tab.id === 'chats-tab' ? $('people-tab') : $('chats-tab'); next.click(); next.focus();
});
$('mobile-nav').onclick = () => { const open = $('sidebar').classList.toggle('open'); $('mobile-nav').setAttribute('aria-expanded',String(open)); };
$('latest-messages').onclick = () => { $('chat-body').scrollTop = $('chat-body').scrollHeight; updateLatestButton(); };
$('chat-body').addEventListener('scroll',updateLatestButton,{passive:true});
$('earlier').onclick = async () => {
  const button = $('earlier'); button.disabled = true;
  try {
    const room = selected, height = $('chat-body').scrollHeight, top = $('chat-body').scrollTop;
    const data = await api(`/api/conversations/${room}?before=${messages[0]?.seq || 0}`); if (room !== selected) return;
    const combined = new Map([...data.messages,...messages].map(m => [m.id,m])); messages = [...combined.values()].sort((a,b) => a.seq-b.seq);
    hasEarlier = data.has_earlier; renderMessages(false); $('chat-body').scrollTop = top + $('chat-body').scrollHeight - height; button.hidden = !hasEarlier;
  } catch (error) { toast(error.message); } finally { button.disabled = false; }
};
async function requestNotifications() {
  if (!('Notification' in window)) return toast('这个浏览器不支持系统通知，聊天中的新消息仍会保留。');
  const result = await Notification.requestPermission(); toast(result === 'granted' ? '已开启通知。值得及时提醒的新消息会通知你。' : '继续在聊天里查看新消息就好。');
};
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) {
    if (waitingForMessageCheck) syncMessages(true);
    else scheduleRefresh();
  }
});

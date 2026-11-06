'use strict';

// 前端只呈现真实持久化记录。没有示例聊天、模拟输入状态或浏览器端后台定时生成。
const $ = id => document.getElementById(id);
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let state = { members: [], conversations: [], active_rooms: [] };
let selected = localStorage.getItem('dropby-room') || '';
let roomData = null, messages = [], replyTo = null, loadSequence = 0, hasEarlier = false;
let refreshTimer, toastTimer, currentVersions = [];
let currentArtifactIndex = 0, artifactMode = 'preview', panelRequest = 0;
let panelView = '', avatarData = '', avatarIcon = 'cat', avatarColor = 'jade';
let sideTab = 'chats', sending = false, renderedMessages = '', profileBaseline = null;
let avatarRequest = 0, avatarLoading = false;
const drafts = new Map();
let seen = {};
try { seen = JSON.parse(localStorage.getItem('dropby-seen') || '{}'); } catch { /* 损坏的本机已读标记不影响聊天数据。 */ }
const notificationCursor = {};
const avatarPresets = [['cat','小猫'],['fox','狐狸'],['bear','小熊'],['rabbit','兔子'],['owl','猫头鹰'],['sprout','嫩芽'],['flower','花朵'],['sun','太阳'],['moon','月亮'],['cloud','云朵'],['ghost','小幽灵'],['orbit','星球']];
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
function authorName(id) { return id === 'user' ? '你' : member(id)?.name || '成员'; }
// 已有成员使用稳定的图形头像；视觉升级不重写成员身份或历史记录。
function presetFor(m) {
  const seed = Array.from(m?.id || m?.name || '友').reduce((total, c) => total + c.codePointAt(0), 0);
  return avatarPresets.some(([id]) => id === m?.avatar_icon) ? m.avatar_icon : avatarPresets[seed % avatarPresets.length][0];
}
function avatar(m, small = false) {
  const preset = presetFor(m);
  const portrait = m?.color === 'user' ? icon('user-round') : `<svg aria-hidden="true"><use href="/avatars.svg#${preset}"/></svg>`;
  return `<span class="avatar ${escapeHTML(m?.color || 'jade')} ${small ? 'small' : ''}" aria-hidden="true">${m?.avatar ? `<img src="${escapeHTML(m.avatar)}" alt="">` : portrait}</span>`;
}
function groupAvatar(c) {
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
    return `${reset.toLocaleString('zh-CN', {...format,...(timeZone ? {timeZone} : {})})}${timeZone ? `（${timeZone}）` : '（本地时间）'}`;
  } catch {
    // 未配置时区的服务可能返回 Local；仍显示同一绝对时刻，并明确这是浏览器本地时间。
    return `${reset.toLocaleString('zh-CN', format)}（本地时间）`;
  }
}
function weeklyBudgetNote() {
  const usage = state.usage || {}, reset = budgetResetLabel();
  const dollars = value => (Number(value || 0) / 1e6).toFixed(2);
  return `<p class="budget-note">本周估算 $${dollars(usage.cost_micros)} / $${dollars(state.weekly_budget_micros)}${state.autonomous_weekly_budget_micros > 0 ? `<br>其中自发探索 $${dollars(usage.autonomous_cost_micros)} / $${dollars(state.autonomous_weekly_budget_micros)} · 为你回来聊天留有余量` : '<br>自发探索额度未开启'}${usage.week_start ? `<br>本周从 ${escapeHTML(String(usage.week_start).split('T')[0])} 开始` : ''}${reset ? `<br>额度重置 ${escapeHTML(reset)}` : ''}</p>`;
}

// 先转义，再只允许 HTTP(S) 链接；模型产出的 HTML、脚本和任意协议都不会执行。
function richText(text) {
  const inline = value => escapeHTML(value).replace(/`([^`\n]+)`/g, '<code>$1</code>').replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
  const parts = [], pattern = /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>"\u3000-\u303f\uff00-\uffef]+)/g;
  let start = 0, match;
  while ((match = pattern.exec(text))) {
    parts.push(inline(text.slice(start, match.index)));
    const href = match[2] || match[3], label = match[1] || href;
    parts.push(`<a href="${escapeHTML(href)}" target="_blank" rel="noopener noreferrer">${escapeHTML(label)}</a>`);
    start = match.index + match[0].length;
  }
  parts.push(inline(text.slice(start)));
  return parts.join('');
}

async function refresh() {
  try {
    state = await api('/api/state');
    // 新私聊也能提醒；通知游标和已读游标分开，围观其他群不会把它们误标为已读。
    for (const c of state.conversations) {
      const previous = notificationCursor[c.id];
      if (!initialLoad && document.hidden && c.last_seq > (previous || 0) && 'Notification' in window && Notification.permission === 'granted') {
        const data = await api(`/api/conversations/${c.id}`);
        for (const m of data.messages.filter(m => m.seq > (previous || 0) && m.notify && m.author_id !== 'user' && m.author_id !== 'system')) {
          const n = new Notification(`${authorName(m.author_id)} · ${c.name}`, { body: m.body.slice(0, 120), tag: m.id });
          n.onclick = () => { window.focus(); openRoom(c.id); n.close(); };
        }
      }
      notificationCursor[c.id] = c.last_seq;
    }
    initialLoad = false;
    renderSidebar();
    const budgetPause = state.usage.cost_micros >= state.weekly_budget_micros ? '本周运行预算已用完' : state.usage.calls >= state.weekly_calls ? '本周调用次数已达到上限' : '';
    const reset = budgetResetLabel();
    const banner = !state.model_ready ? '模型服务尚未配置。设置服务端的 OPENAI_API_KEY 后重启即可聊天，创建成员仍然可用。' : budgetPause ? `${budgetPause}。聊天和成果仍可查看，未完工作会在${reset || '下一预算周'}恢复。` : !state.search_ready ? '网页探索尚未接通，成员目前不能进行真实搜索或读取网页。' : '';
    $('service-banner').textContent = banner; $('service-banner').hidden = !banner;
    if (!selected || !state.conversations.some(c => c.id === selected)) selected = state.conversations[0]?.id || '';
    if (selected) await loadConversation(selected, true); else renderWelcome();
  } catch (error) { toast(error.message); }
}
function scheduleRefresh() { clearTimeout(refreshTimer); refreshTimer = setTimeout(refresh, 150); }

function renderSidebar() {
  const query = $('chat-search').value.trim().toLocaleLowerCase();
  const conversations = state.conversations.filter(c => `${c.name} ${c.last_message}`.toLocaleLowerCase().includes(query));
  const people = state.members.filter(m => `${m.name} ${m.profile}`.toLocaleLowerCase().includes(query));
  $('conversation-list').innerHTML = conversations.length ? conversations.map(c => {
    const m = c.kind === 'direct' ? member(c.member_ids[0]) : null;
    const unread = c.last_seq > (seen[c.id] || 0) && c.id !== selected;
    return `<button class="room-button ${c.id === selected ? 'selected' : ''}" data-action="open-room" data-id="${c.id}" ${c.id === selected ? 'aria-current="page"' : ''}>${m ? avatar(m) : groupAvatar(c)}<span class="room-copy"><span class="room-heading"><span class="room-name">${escapeHTML(c.name)}</span><time class="room-time">${c.updated_at ? timeLabel(c.updated_at) : ''}</time></span><span class="room-preview">${escapeHTML(c.last_message || '从一句随口的话开始')}</span></span>${unread ? '<span class="unread" aria-label="有新消息"></span>' : ''}</button>`;
  }).join('') : `<p class="side-empty">${query ? '没有找到，换个词试试。' : '还没有聊天。<br>先认识一位新朋友吧。'}</p>`;
  $('member-list').innerHTML = people.length ? people.map(m => `<button class="person-button" data-action="profile" data-id="${m.id}">${avatar(m)}<span class="person-copy"><span class="person-name">${escapeHTML(m.name)}</span><span class="person-note">${escapeHTML(m.direction || '边聊，边认识')}</span></span>${icon('chevron-right')}</button>`).join('') : `<p class="side-empty">${query ? '没有找到这位朋友。' : '一个名字，一个头像。<br>其余的，边聊边认识。'}</p>`;
  $('conversation-list').hidden = sideTab !== 'chats';
  $('member-list').hidden = sideTab !== 'people';
  $('chats-tab').setAttribute('aria-selected', String(sideTab === 'chats'));
  $('people-tab').setAttribute('aria-selected', String(sideTab === 'people'));
  $('chats-tab').tabIndex = sideTab === 'chats' ? 0 : -1; $('people-tab').tabIndex = sideTab === 'people' ? 0 : -1;
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
  if (id !== selected) {
    rememberDraft(); closePanel(); selected = id; messages = []; renderedMessages = '';
    const draft = drafts.get(id); replyTo = draft?.reply || null;
    $('message-input').value = draft?.body || ''; renderReply(); sizeComposer();
  }
  localStorage.setItem('dropby-room', id); $('sidebar').classList.remove('open'); $('mobile-nav').setAttribute('aria-expanded','false'); renderSidebar();
  await loadConversation(id, false);
}
async function loadConversation(id, preserve) {
  const sequence = ++loadSequence;
  const data = await api(`/api/conversations/${id}`);
  if (sequence !== loadSequence || selected !== id) return;
  const changedRoom = roomData?.conversation.id !== id;
  roomData = data;
  if (changedRoom || !preserve) { messages = data.messages; hasEarlier = data.has_earlier; }
  else {
    const combined = new Map(messages.map(m => [m.id, m]));
    for (const m of data.messages) combined.set(m.id, m);
    messages = [...combined.values()].sort((a, b) => a.seq - b.seq);
  }
  const c = data.conversation;
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
  if (panelView === 'artifacts' && $('panel-content').dataset.artifactListSignature !== JSON.stringify(data.artifacts.map(a => [a.id,a.version,a.title]))) {
    const top = $('panel-content').scrollTop; showArtifacts(); $('panel-content').scrollTop = top;
  }
  if (panelView === 'room') roomDetails(false);
  if (panelView.startsWith('profile:')) {
    const id = panelView.slice(8), current = member(id);
    const signature = JSON.stringify([current, roomData.member_states?.find(s => s.member_id === id)]);
    if ($('panel-content').dataset.profileSignature !== signature) { const top = $('panel-content').scrollTop; profile(id); $('panel-content').scrollTop = top; }
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
  const signature = JSON.stringify([messages, state.members.map(m => [m.id,m.name,m.avatar,m.avatar_icon,m.color])]);
  // SSE 也会报告后台活动。聊天内容没变时保留 DOM 和键盘焦点，避免打断正在点选的问题。
  if (signature === renderedMessages && !scrollToBottom) return;
  renderedMessages = signature;
  let lastDate = '';
  const lastHumanSeq = Math.max(0, ...messages.filter(m => m.author_id === 'user').map(m => m.seq));
  // 只展示公开消息明确分享的成果版本。安静研究保存的作品留在成果集合里，
  // 不因数据库多了一份草稿，就替角色自动在群里发布内容。
  $('messages').innerHTML = messages.length ? messages.map(m => {
    const date = dateLabel(m.created_at), divider = date !== lastDate ? `<div class="date-divider">${date}</div>` : ''; lastDate = date;
    if (m.author_id === 'system') {
      if (m.body.startsWith('这次运行暂未完成')) return `${divider}<details class="system-message"><summary>${timeLabel(m.created_at)} 的一次行动中断了 · 查看当时记录</summary><p>${escapeHTML(m.body)}</p><button class="quiet" data-action="activity">查看动态</button></details>`;
      return `${divider}<div class="system-message" role="status">${escapeHTML(m.body)}</div>`;
    }
    const isUser = m.author_id === 'user', person = isUser ? {name:'你',color:'user'} : member(m.author_id);
    const quoted = messages.find(q => q.id === m.reply_to);
    const quote = m.reply_to ? `<button class="quote" data-action="jump-message" data-id="${m.reply_to}" aria-label="查看引用的消息"><strong>${escapeHTML(quoted ? authorName(quoted.author_id) : '较早的消息')}</strong><span>${escapeHTML(quoted ? quoted.body.slice(0,100) : '点击查看这段交流')}</span></button>` : '';
    const q = m.question, open = q && m.question_status === 'open' && m.seq > lastHumanSeq;
    const question = q ? `${m.body !== q.text ? `<span class="question-text">${m.body ? '\n\n' : ''}${escapeHTML(q.text)}</span>` : ''}${open ? `<div class="question-options">${q.options.map((option,i) => `<button class="question-option" data-action="answer-question" data-id="${m.id}" data-option="${i}">${escapeHTML(option)}${icon('chevron-right')}</button>`).join('')}</div><span class="question-hint">选一个，或直接在下面说说你的想法。聊点别的也可以。</span>` : m.question_status === 'answered' ? '<span class="question-closed">已回答</span>' : ''}` : '';
    return `${divider}<article class="message ${isUser ? 'user-message' : ''}" id="message-${m.id}" data-message-id="${m.id}">${isUser ? avatar(person) : `<button class="avatar-button" data-action="profile" data-id="${m.author_id}" aria-label="了解 ${escapeHTML(authorName(m.author_id))}">${avatar(person)}</button>`}<div class="message-content"><div class="message-meta"><strong>${escapeHTML(authorName(m.author_id))}</strong><time datetime="${m.created_at}">${timeLabel(m.created_at)}</time></div><div class="message-bubble">${quote}${richText(m.body)}${question}</div>${m.artifacts?.length ? `<div class="message-artifacts">${m.artifacts.map(artifactCard).join('')}</div>` : ''}<div class="message-actions"><button data-action="reply" data-id="${m.id}" aria-label="回复 ${escapeHTML(authorName(m.author_id))} 的消息">${icon('reply')}回复</button>${(m.evidence || []).map((id,i) => `<button data-action="evidence" data-id="${id}">${icon('external-link')}查看行动${m.evidence.length > 1 ? ' ' + (i+1) : ''}</button>`).join('')}</div></div></article>`;
  }).join('') : `<div class="empty-chat"><div class="empty-avatars">${roomData.conversation.member_ids.map(id => avatar(member(id))).join('')}</div><strong>${roomData.conversation.kind === 'direct' ? `和 ${escapeHTML(authorName(roomData.conversation.member_ids[0]))} 打个招呼` : '人到齐了，随便聊聊'}</strong><p>一个念头、一件小事、一个好奇的问题。<br>从哪里开始，都可以。</p></div>`;
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
  panelRequest++;
  panelView = view;
  document.querySelector('.app').classList.toggle('artifact-open', view.startsWith('artifact:'));
  $('panel-title').textContent = title;
  $('panel-content').innerHTML = `${description ? `<p class="panel-description">${escapeHTML(description)}</p>` : ''}${body}`;
  $('inspector').hidden = false; document.querySelector('.app').classList.add('panel-open'); $('sidebar').classList.remove('open');
}
function closePanel() {
  $('inspector').hidden = true; document.querySelector('.app').classList.remove('panel-open','artifact-open'); panelRequest++; panelView = ''; avatarRequest++; avatarLoading = false;
}
function formError(form, error) { const el = form.querySelector('.form-error'); el.textContent = error.message; el.hidden = false; }
function pending(form, text) { const button = form.querySelector('button[type=submit]'); const label = button.textContent; button.textContent = text; button.disabled = true; return () => { button.textContent = label; button.disabled = false; }; }

function avatarEditor(m = {}) {
  avatarRequest++; avatarLoading = false;
  avatarData = m.avatar || ''; avatarIcon = m.id ? presetFor(m) : 'cat'; avatarColor = m.color || 'jade';
  return `<div class="avatar-editor"><span id="avatar-preview">${avatar({avatar:avatarData,avatar_icon:avatarIcon,color:avatarColor})}</span><div class="avatar-edit-copy"><strong>选一个你喜欢的头像</strong><small>也可以用自己的照片</small><label class="avatar-upload">${icon('upload')}上传图片<input class="sr-only" id="avatar-file" type="file" accept="image/png,image/jpeg"></label></div></div>
    <fieldset class="avatar-gallery"><legend>默认头像</legend><div class="avatar-grid">${avatarPresets.map(([id,label]) => `<button type="button" class="avatar-choice" data-action="avatar-preset" data-id="${id}" aria-label="${label}头像" aria-pressed="${!avatarData && id === avatarIcon}">${avatar({avatar_icon:id,color:avatarColor})}</button>`).join('')}</div></fieldset>
    <div class="color-picker" role="group" aria-label="头像颜色">${colors.map(([color,label]) => `<label class="color-choice ${color}"><input type="radio" name="color" value="${color}" ${color === avatarColor ? 'checked' : ''} aria-label="${label}"><span></span></label>`).join('')}</div>`;
}
function renderAvatarPreview() {
  if (!$('avatar-preview')) return;
  $('avatar-preview').innerHTML = avatar({avatar:avatarData,avatar_icon:avatarIcon,color:avatarColor});
  for (const choice of document.querySelectorAll('.avatar-choice')) {
    choice.setAttribute('aria-pressed',String(!avatarData && choice.dataset.id === avatarIcon));
    choice.innerHTML = avatar({avatar_icon:choice.dataset.id,color:avatarColor});
  }
}
function newMember() {
  showPanel('认识新朋友', '先有个名字，其他的边聊边认识。', `<form id="member-form">${avatarEditor()}<label class="field"><span>名字</span><input name="name" maxlength="24" required placeholder="怎么称呼这位朋友？" autocomplete="off"></label><div class="form-error" hidden role="alert"></div><button class="primary full-width" type="submit">开始聊天 ${icon('arrow-up')}</button></form>`, 'new-member');
  $('panel-content').querySelector('input[name=name]').focus();
}
function editProfile(id) {
  const m = member(id); if (!m) return;
  showPanel('编辑朋友', '', `<form id="profile-form" data-id="${m.id}">${avatarEditor(m)}<label class="field"><span>名字</span><input name="name" maxlength="24" required value="${escapeHTML(m.name)}" autocomplete="off"></label><label class="field"><span>特质与偏好</span><textarea name="profile" maxlength="3000" rows="7">${escapeHTML(m.profile)}</textarea><small class="field-hint">性格、兴趣、说话习惯，都可以用自己的话写。也可以在私聊里慢慢告诉 TA。</small></label><div class="form-error" hidden role="alert"></div><div class="panel-footer"><button class="secondary" type="button" data-action="profile" data-id="${m.id}">取消</button><button class="primary" type="submit">保存修改</button></div></form>`, 'edit-profile');
  // 只提交用户实际改过的字段，避免头像修改覆盖同时从聊天中学到的新偏好。
  profileBaseline = {name:m.name.trim(),profile:m.profile.trim(),color:avatarColor,avatar:avatarData,avatar_icon:avatarIcon};
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

function renderActivity() {
  const active = (state.activities || []).filter(a => a.conversation_id === selected);
  $('activity').hidden = !active.length;
  $('activity').innerHTML = active.map(a=>`<button data-action="activity"><span class="activity-dot"></span>${escapeHTML(authorName(a.member_id))} · ${escapeHTML(a.label)}</button>`).join('');
  const jobs = roomData.work || [], working = jobs.filter(w=>['pending','running','waiting_budget'].includes(w.status));
  $('work-count').textContent = working.length || '';
  const latest = jobs[0], failed = latest && ['failed','paused'].includes(latest.status);
  $('work-notice').hidden = !failed;
  $('work-notice').textContent = failed ? `${latest.member_id ? authorName(latest.member_id)+'的' : '上一次'}行动暂时停下了 · 查看已保存的进展 →` : '';
}
const actionLabels = {ask_question:'提出问题',search:'搜索网页',read_page:'阅读网页',save_artifact:'保存成果',remember:'更新经历',schedule_exploration:'安排探索',schedule_followup:'安排定时跟进',cancel_work:'放下不再需要的后续',read_history:'回顾聊天',read_artifact:'查看成果',read_tool_result:'核对已有材料',read_memory:'回顾经历',search_memory:'寻找相关经历',read_work:'查看工作进展',delegate_work:'委托朋友协作',send_private:'发起私聊'};
const statusLabels = {pending:'已安排',running:'进行中',waiting_budget:'等待预算恢复',done:'已完成',failed:'暂时中断',paused:'已暂停',stopped:'已停止',superseded:'已根据新消息调整',succeeded:'已完成',cancelled:'已被新消息打断'};
function showActivity(scroll = true) {
  if (!roomData) return;
  const expanded = [...$('panel-content').querySelectorAll('details[open][data-detail]')].map(d=>d.dataset.detail);
  const top = $('panel-content').scrollTop, jobs = roomData.work || [], actions = roomData.tool_runs || [];
  const decisions = new Map();
  for (const p of roomData.participation || []) if (!decisions.has(p.member_id)) decisions.set(p.member_id,p);
  const live = (state.activities || []).filter(a=>a.conversation_id===selected);
  const people = roomData.conversation.member_ids.map(id=>{
    const p=decisions.get(id), action=live.find(a=>a.member_id===id);
    const focus = roomData.member_states?.find(s => s.member_id === id);
    const label = action?.label || focus?.body || (!p ? '还没有参与记录' : p.action==='pass' ? '暂时没有补充' : p.action==='revisit' ? '想稍后再看看' : p.action==='error' ? '上次判断未完成' : p.selected ? (p.action==='explore' ? '已安排探索' : '最近参与了交流') : '有想法，尚未发言');
    return `<div class="member-activity">${avatar(member(id),true)}<div><strong>${escapeHTML(authorName(id))}</strong><span>${escapeHTML(label)}</span>${p ? `<details data-detail="person-${id}"><summary>查看最近参与记录</summary><p>${escapeHTML(p.action === 'pass' ? '这次选择保持安静。' : p.action === 'error' ? '这次参与判断未完成。' : p.action === 'explore' ? '这次选择先探索。' : p.action === 'revisit' ? '这次想稍后再看看。' : '这次选择参与交流。')}</p><small>${timeLabel(p.created_at)}</small></details>` : ''}</div></div>`;
  }).join('');
  showPanel('最近动态', '', `<p class="background-note">浏览器关掉后，服务仍会继续探索。电脑睡眠或服务停止时会暂停，重新运行后恢复。新消息、约好的后续和闲时好奇都可以带来一次机会，是否继续由朋友们自己决定。</p><h3 class="section-label">朋友们</h3>${people}<h3 class="section-label">后台行动</h3>${jobs.filter(w=>['research','revisit'].includes(w.kind) || ['failed','paused','waiting_budget'].includes(w.status)).slice(0,12).map(w=>`<details class="work-item" data-detail="${w.id}"><summary><span>${escapeHTML(w.member_id ? authorName(w.member_id) : '这段交流')}</span><span class="status ${w.status}">${escapeHTML(statusLabels[w.status]||w.status)}</span><strong>${escapeHTML(w.instruction.slice(0,90))}</strong></summary>${w.assigned_by ? `<div class="work-assignment">${escapeHTML(authorName(w.assigned_by))} 委托 · ${escapeHTML(new Date(w.created_at).toLocaleString('zh-CN'))}</div>` : ''}<p>${escapeHTML(w.result || w.reason || '还没有执行结果')}</p>${w.status==='pending' ? `<small>${w.kind === 'revisit' ? '下次考虑' : '安排执行'} ${new Date(w.due_at).toLocaleString('zh-CN')}</small>`:''}${['failed','paused'].includes(w.status) ? `<button class="quiet" data-action="continue-work" data-id="${w.id}">在聊天中继续这个方向 →</button>`:''}</details>`).join('') || '<p class="empty-note">暂时没有安排研究。普通聊天不必变成任务。</p>'}<h3 class="section-label">实际做过的事</h3>${actions.slice(0,15).map(a=>`<button class="action-item" data-action="evidence" data-id="${a.id}"><span>${escapeHTML(authorName(a.member_id))} · ${escapeHTML(actionLabels[a.name]||a.name)}</span><small>${timeLabel(a.created_at)} · ${escapeHTML(statusLabels[a.status]||a.status)} ↗</small></button>`).join('')||'<p class="empty-note">还没有工具行动。</p>'}${weeklyBudgetNote()}`, 'activity');
  if (!scroll) { $('panel-content').scrollTop = top; for (const d of $('panel-content').querySelectorAll('details[data-detail]')) d.open = expanded.includes(d.dataset.detail); }
}
function newGroup() {
  if (!state.members.length) { newMember(); return; }
  showPanel('一起坐坐', '逐个邀请你想放在一起的朋友。群可以没有预设话题。', `<form id="group-form"><label class="field"><span>群名 <span class="field-hint">（可留空）</span></span><input name="name" maxlength="80" placeholder="随便聊聊"></label><span class="field-label">邀请朋友</span>${state.members.map(m => `<label class="member-choice">${avatar(m)}<span class="copy"><strong>${escapeHTML(m.name)}</strong><small>${escapeHTML(m.direction.slice(0, 70))}</small></span><input type="checkbox" name="members" value="${m.id}" aria-label="邀请 ${escapeHTML(m.name)}"></label>`).join('')}<div class="form-error" hidden role="alert"></div><div class="panel-footer"><button class="primary" type="submit">开一个群 →</button></div></form>`);
}
function focusBlock(id) {
  const focus = roomData?.member_states?.find(s => s.member_id === id);
  if (!focus?.body) return '';
  return `<div class="focus-block"><h3 class="section-label">${icon('smile')}此刻在意</h3><div class="focus-body">${escapeHTML(focus.body)}</div>${focus.source_ids?.length ? `<div class="focus-sources">${focus.source_ids.map((source,i) => `<button data-action="${source.startsWith('tool_') ? 'evidence' : 'jump-message'}" data-id="${source}">${source.startsWith('tool_') ? '查看相关行动' : '回到相关交流'}${focus.source_ids.length > 1 ? ` ${i+1}` : ''} ↗</button>`).join('')}</div>` : ''}</div>`;
}
function profile(id) {
  const m = member(id); if (!m) return;
  showPanel('朋友资料', '', `<div class="profile-head">${avatar(m)}<h3>${escapeHTML(m.name)}</h3><p>AI 朋友 · ${escapeHTML(dateLabel(m.created_at))} 相识</p></div><div class="profile-actions"><button class="primary" data-action="direct" data-id="${m.id}">${icon('message-circle')}单独聊聊</button><button class="secondary" data-action="edit-profile" data-id="${m.id}">${icon('pencil')}编辑</button></div>${focusBlock(id)}${capabilities(m)}<h3 class="section-label">特质与偏好</h3><div class="profile-body">${escapeHTML(m.profile)}</div><p class="profile-caption">你可以随时编辑，也可以在聊天里说「以后……」，让 TA 记住你的偏好。</p><div class="panel-nav"><button data-action="preference" data-id="${m.id}">${icon('circle-help')}聊一个偏好${icon('chevron-right')}</button></div>`, `profile:${id}`);
  $('panel-content').dataset.profileSignature = JSON.stringify([m, roomData?.member_states?.find(s => s.member_id === id)]);
}
function roomDetails(scroll = true) {
  if (!roomData) return;
  const c = roomData.conversation;
  if (c.kind === 'direct') { profile(c.member_ids[0]); return; }
  const top = $('panel-content').scrollTop;
  const members = c.member_ids.map(id => {
    const m = member(id), focus = roomData.member_states?.find(s => s.member_id === id);
    return `<button class="person-button" data-action="profile" data-id="${id}">${avatar(m)}<span class="person-copy"><span class="person-name">${escapeHTML(authorName(id))}${id === c.coordinator_id ? '<span class="coordinator-badge">协调者</span>' : ''}</span><span class="person-note">${escapeHTML(focus?.body || m?.direction || '边聊，边认识')}</span></span>${icon('chevron-right')}</button>`;
  }).join('');
  const current = c.coordinator_id ? `目前由 ${authorName(c.coordinator_id)} 协调这个群。` : '目前由大家各自参与，没有指定协调者。';
  const existing = $('coordinator-form');
  // 后台动态只更新阅读内容，不能重建正在选择的下拉框或丢掉尚未保存的选择。
  if (!scroll && existing?.dataset.room === c.id) {
    $('room-detail-name').textContent = c.name;
    $('room-detail-people').textContent = `你和 ${c.member_ids.map(authorName).join('、')}`;
    if ($('room-members').innerHTML !== members && !$('room-members').contains(document.activeElement)) $('room-members').innerHTML = members;
    $('coordinator-current').textContent = current;
    const select = existing.elements.member_id;
    if (existing.dataset.saving !== 'true' && document.activeElement !== select && select.value === existing.dataset.savedId) {
      // 只有用户未改动的表单才跟随服务端设置；另一个窗口的修改不覆盖本地未保存选择。
      select.value = c.coordinator_id || '';
      existing.dataset.savedId = select.value;
      existing.querySelector('button[type=submit]').disabled = true;
    }
    $('panel-content').scrollTop = top;
    return;
  }
  showPanel('聊天详情', '', `<div class="room-details-head">${groupAvatar(c)}<h3 id="room-detail-name">${escapeHTML(c.name)}</h3><p id="room-detail-people">你和 ${c.member_ids.map(authorName).map(escapeHTML).join('、')}</p></div><h3 class="section-label">在这里的朋友</h3><div class="room-members" id="room-members">${members}</div><form id="coordinator-form" class="coordinator-form" data-room="${escapeHTML(c.id)}" data-saved-id="${escapeHTML(c.coordinator_id || '')}"><label class="field" for="coordinator-select"><span>群里的协调者</span><select id="coordinator-select" name="member_id" aria-describedby="coordinator-hint coordinator-current"><option value="">各自参与</option>${c.member_ids.map(id => `<option value="${escapeHTML(id)}" ${id === c.coordinator_id ? 'selected' : ''}>${escapeHTML(authorName(id))}</option>`).join('')}</select><small class="field-hint" id="coordinator-hint">让一位朋友帮忙分工、跟进和衔接进展。其他人仍按自己的特质参与，也可以提出不同意见。只在这个群生效。</small></label><p class="coordinator-current" id="coordinator-current">${escapeHTML(current)}</p><div class="form-error" hidden role="alert"></div><div class="coordinator-save"><span class="form-status" role="status"></span><button class="secondary" type="submit" disabled>保存设置</button></div></form><div class="panel-nav"><button data-action="activity">${icon('activity')}朋友动态${icon('chevron-right')}</button><button data-action="artifacts">${icon('notebook')}聊天成果${icon('chevron-right')}</button></div><p class="profile-caption">有的人爱接话，有的人先观察。安静也可以是一次自己的选择。</p>`, 'room');
  if (!scroll) $('panel-content').scrollTop = top;
}

async function saveCoordinator(form) {
  if (form.dataset.saving === 'true') return;
  const room = form.dataset.room, select = form.elements.member_id, id = select.value;
  const button = form.querySelector('button[type=submit]'), status = form.querySelector('.form-status');
  form.dataset.saving = 'true'; select.disabled = true; button.disabled = true;
  button.textContent = '正在保存…'; form.querySelector('.form-error').hidden = true; status.textContent = '';
  try {
    await api(`/api/conversations/${room}/coordinator`, {method:'PATCH',body:JSON.stringify({member_id:id})});
    // 保存仅更新群设置，不改写聊天草稿；用户若已关闭或切换侧栏，也不把它重新打开。
    if (roomData?.conversation.id === room) roomData.conversation.coordinator_id = id;
    const c = state.conversations.find(c => c.id === room); if (c) c.coordinator_id = id;
    if (form.isConnected) { form.dataset.savedId = id; status.textContent = '已保存'; }
    await refresh();
  } catch (error) {
    if (form.isConnected) formError(form,error); else toast(error.message);
  } finally {
    if (form.isConnected) {
      form.dataset.saving = 'false'; select.disabled = false; button.textContent = '保存设置';
      button.disabled = select.value === form.dataset.savedId;
    }
  }
}
// 工具清单只展示当前真正可用的能力；不让未接通的网页服务看起来已经可用。
function capabilities(m) {
  const labels = ['对话选择题', '文字与网页创作'];
  if (state.search_ready && m.tools?.includes('search')) labels.push('搜索与阅读');
  labels.push('检索与记住经历', '定时跟进');
  if (roomData?.conversation.kind === 'group' && roomData.conversation.coordinator_id === m.id) labels.push('协调这个群');
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
  showPanel(a.title, `由 ${authorName(a.member_id)} 保存 · ${dateLabel(a.created_at)}`, `<button class="artifact-back quiet" data-action="artifacts">${icon('arrow-left')}所有成果</button><div class="artifact-toolbar"><select class="version-select" id="artifact-version" aria-label="选择成果版本">${currentVersions.map((v, i) => `<option value="${i}" ${index === i ? 'selected' : ''}>第 ${v.version} 版 · ${dateLabel(v.created_at)} ${timeLabel(v.created_at)}</option>`).join('')}</select><a class="artifact-download" href="${base}/download?version=${a.version}" download>下载${html ? ' HTML' : '文稿'}</a></div><button id="artifact-new-version" class="artifact-update" data-action="artifact" data-id="${escapeHTML(a.id)}" hidden></button>${tabs}${content}<div class="panel-footer"><button class="primary" data-action="continue-artifact" data-id="${escapeHTML(a.id)}">继续聊这个成果 ${icon('message-circle')}</button></div>`, `artifact:${a.id}`);
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
    if (action === 'profile') profile(id);
    if (action === 'edit-profile') editProfile(id);
    if (action === 'close-panel') { closePanel(); $('message-input').focus(); }
    if (action === 'activity') showActivity();
    if (action === 'artifacts') showArtifacts();
    if (action === 'preference') await askPreference(id,button);
    if (action === 'avatar-preset') {
      avatarRequest++;
      if (avatarLoading) { avatarLoading = false; const save = button.closest('form')?.querySelector('button[type=submit]'); if (save) save.disabled = false; }
      avatarIcon = id; avatarData = ''; renderAvatarPreview();
    }
    if (action === 'continue-work') { const w = roomData.work.find(w => w.id === id); if (w) composeFollowup(`继续这件事：${w.instruction}`); }
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
    if (action === 'evidence') {
      const run = await api(`/api/tool-runs/${id}`);
      showPanel(actionLabels[run.name] || '实际行动', `${authorName(run.member_id)} · ${dateLabel(run.created_at)} ${timeLabel(run.created_at)}`, `<div class="evidence"><p>执行结果：${escapeHTML(statusLabels[run.status] || run.status)}</p><div class="action-summary">${Object.entries(run.arguments).map(([key,value]) => `<p>${escapeHTML(String(value))}</p>`).join('')}</div><details><summary>查看完整记录</summary><pre>${escapeHTML(run.result)}</pre></details></div>`);
    }
  } catch (error) { toast(error.message); button.disabled = false; }
});

document.addEventListener('submit', async event => {
  const form = event.target;
  if (form.id === 'coordinator-form') {
    event.preventDefault(); await saveCoordinator(form); return;
  }
  if (!['member-form','profile-form','group-form'].includes(form.id)) return;
  event.preventDefault(); if (avatarLoading && form.id !== 'group-form') return;
  const reset = pending(form, form.id === 'profile-form' ? '正在保存…' : '正在准备…');
  try {
    const data = new FormData(form);
    if (form.id === 'member-form') {
      const m = await post('/api/members', {name:data.get('name').trim(),color:avatarColor,avatar:avatarData,avatar_icon:avatarIcon});
      await openDirect(m.id); $('message-input').focus();
    } else if (form.id === 'profile-form') {
      const values = {name:data.get('name').trim(),profile:data.get('profile').trim(),color:avatarColor,avatar:avatarData,avatar_icon:avatarIcon};
      const changes = Object.fromEntries(Object.entries(values).filter(([key,value]) => value !== profileBaseline?.[key]));
      if (Object.keys(changes).length) await api(`/api/members/${form.dataset.id}`, {method:'PATCH',body:JSON.stringify(changes)});
      await refresh(); profile(form.dataset.id); toast('修改已保存。');
    } else {
      const ids = data.getAll('members'); if (!ids.length) throw new Error('先邀请至少一位朋友吧。');
      const c = await post('/api/conversations',{name:data.get('name'),kind:'group',member_ids:ids});
      closePanel(); await refresh(); await openRoom(c.id);
    }
  } catch (error) { formError(form,error); reset(); }
});

// 只发送裁剪后的缩略图，原始照片和 EXIF 信息保留在本机。
document.addEventListener('change', async event => {
  if (event.target.id === 'coordinator-select') {
    const form = event.target.form, changed = event.target.value !== form.dataset.savedId;
    form.querySelector('button[type=submit]').disabled = !changed;
    form.querySelector('.form-error').hidden = true;
    form.querySelector('.form-status').textContent = changed ? '修改尚未保存' : '';
  }
  if (event.target.id === 'avatar-file') {
    const file = event.target.files[0], form = event.target.closest('form'); if (!file) return;
    const request = ++avatarRequest, save = form.querySelector('button[type=submit]');
    avatarLoading = true; save.disabled = true;
    try {
      if (!['image/png','image/jpeg'].includes(file.type) || file.size > 10*1024*1024) throw new Error('请选择 10MB 以内的 PNG 或 JPEG 图片。');
      const bitmap = await createImageBitmap(file), canvas = document.createElement('canvas');
      // 新图片、默认头像或关闭页面会使旧解码结果失效，避免迟到的照片覆盖最新选择。
      if (request !== avatarRequest || !form.isConnected) { bitmap.close(); return; }
      canvas.width = 128; canvas.height = 128;
      const size = Math.min(bitmap.width,bitmap.height);
      canvas.getContext('2d').drawImage(bitmap,(bitmap.width-size)/2,(bitmap.height-size)/2,size,size,0,0,128,128);
      bitmap.close(); avatarData = canvas.toDataURL('image/png');
      if (avatarData.length > 88000) avatarData = canvas.toDataURL('image/jpeg',.85);
      renderAvatarPreview();
    } catch (error) { if (request === avatarRequest) toast(error.message); }
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
    if (!$('mention-options').hidden) $('mention-options').hidden = true;
    else if (!$('inspector').hidden) { closePanel(); $('room-details').focus(); }
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
$('new-member').onclick = newMember; $('new-group').onclick = newGroup;
$('welcome-action').onclick = () => state.members.length ? newGroup() : newMember();
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
$('notifications').onclick = async () => {
  if (!('Notification' in window)) return toast('这个浏览器不支持系统通知，聊天中的新消息仍会保留。');
  const result = await Notification.requestPermission(); toast(result === 'granted' ? '已开启通知。值得及时提醒的新消息会通知你。' : '继续在聊天里查看新消息就好。');
};
document.addEventListener('visibilitychange', () => { if (!document.hidden) scheduleRefresh(); });

const stream = new EventSource('/api/events');
stream.addEventListener('ready', () => { $('connection-dot').className = 'ready'; $('connection-label').textContent = '已连接'; scheduleRefresh(); });
stream.addEventListener('change',scheduleRefresh);
stream.onerror = () => { $('connection-dot').className = ''; $('connection-label').textContent = '正在重连'; };
refresh();

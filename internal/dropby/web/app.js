'use strict';

// 前端只呈现真实持久化记录。没有示例聊天、模拟输入状态或浏览器端后台定时生成。
const $ = id => document.getElementById(id);
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let state = { members: [], conversations: [], active_rooms: [] };
let selected = localStorage.getItem('dropby-room') || '';
let roomData = null, messages = [], replyTo = null, loadSequence = 0, hasEarlier = false;
let refreshTimer, toastTimer, currentVersions = [];
let panelView = '', panelRequest = 0, avatarData = '', preferenceQuestion = null;
let seen = {};
try { seen = JSON.parse(localStorage.getItem('dropby-seen') || '{}'); } catch { /* 损坏的本机已读标记不影响聊天数据。 */ }
const notificationCursor = {};
let initialLoad = true;

async function api(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { 'Content-Type': 'application/json', ...options.headers } });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || '请求没有完成，请稍后再试。');
  return data;
}
const post = (path, data) => api(path, { method: 'POST', body: JSON.stringify(data) });
function toast(text) { $('toast').textContent = text; $('toast').hidden = false; clearTimeout(toastTimer); toastTimer = setTimeout(() => $('toast').hidden = true, 7000); }
function member(id) { return state.members.find(m => m.id === id); }
function authorName(id) { return id === 'user' ? '你' : member(id)?.name || '成员'; }
function avatar(m, small = false) { return `<span class="avatar ${escapeHTML(m?.color || 'jade')} ${small ? 'small' : ''}" aria-hidden="true">${m?.avatar ? `<img src="${escapeHTML(m.avatar)}" alt="">` : escapeHTML(Array.from(m?.name || '友')[0])}</span>`; }
function timeLabel(date) { return new Date(date).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }); }
function dateLabel(date) { return new Date(date).toLocaleDateString('zh-CN', { month: 'long', day: 'numeric' }); }

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
    const banner = !state.model_ready ? '模型服务尚未配置。设置服务端的 OPENAI_API_KEY 后重启即可聊天，创建成员仍然可用。' : state.usage.calls >= state.daily_calls || state.usage.cost_micros >= state.daily_budget_micros ? '今日运行预算已用完。聊天和成果仍可查看，未完工作会在下一预算日恢复。' : !state.search_ready ? '网页探索尚未接通，成员目前不能进行真实搜索或读取网页。' : '';
    $('service-banner').textContent = banner; $('service-banner').hidden = !banner;
    if (!selected || !state.conversations.some(c => c.id === selected)) selected = state.conversations[0]?.id || '';
    if (selected) await loadConversation(selected, true); else renderWelcome();
  } catch (error) { toast(error.message); }
}
function scheduleRefresh() { clearTimeout(refreshTimer); refreshTimer = setTimeout(refresh, 150); }

function renderSidebar() {
  $('conversation-list').innerHTML = state.conversations.length ? state.conversations.map(c => {
    const m = c.kind === 'direct' ? member(c.member_ids[0]) : null;
    return `<button class="room-button ${c.id === selected ? 'selected' : ''}" data-action="open-room" data-id="${c.id}" ${c.id === selected ? 'aria-current="page"' : ''}>${m ? avatar(m) : '<span class="group-icon" aria-hidden="true">#</span>'}<span class="room-copy"><span class="room-name">${escapeHTML(c.name)}</span><span class="room-preview">${escapeHTML(c.last_message || '从一句随口的话开始')}</span></span>${c.last_seq > (seen[c.id] || 0) && c.id !== selected ? '<span class="unread" aria-label="有新消息"></span>' : ''}</button>`;
  }).join('') : '<p class="side-empty">还没有聊天。<br>选几位朋友，开一个群吧。</p>';
  $('member-list').innerHTML = state.members.length ? state.members.map(m => `<button class="person-button" data-action="direct" data-id="${m.id}">${avatar(m, true)}<span class="person-name">${escapeHTML(m.name)}</span></button>`).join('') : '<p class="side-empty">给一个名字，先认识起来。<br>兴趣和偏好可以边聊边发现。</p>';
}
function renderWelcome() {
  $('welcome').hidden = false; $('chat-body').hidden = true; $('composer-wrap').hidden = true; $('show-artifacts').hidden = true; $('show-activity').hidden = true;
  $('room-title').textContent = '一个可以随时回来的地方'; $('room-subtitle').textContent = '认识朋友，聊聊最近的发现。';
  $('header-members').innerHTML = '';
  $('welcome-action').innerHTML = state.members.length ? '邀请朋友，开一个群 <span>↗</span>' : '认识第一位朋友 <span>↗</span>';
}

async function openRoom(id) {
  if (id !== selected) { closePanel(); selected = id; messages = []; replyTo = null; $('message-input').value = ''; renderReply(); }
  localStorage.setItem('dropby-room', id); $('sidebar').classList.remove('open'); renderSidebar();
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
  $('room-title').textContent = c.name;
  $('room-subtitle').textContent = c.kind === 'direct' ? `你与 ${authorName(c.member_ids[0])} 的私聊` : `你和 ${c.member_ids.map(authorName).join('、')}`;
  if (c.discussion_paused) $('room-subtitle').textContent += ' · 讨论已暂停';
  if (c.research_paused) $('room-subtitle').textContent += ' · 研究已停止';
  $('header-members').innerHTML = c.member_ids.map(id => `<button data-action="profile" data-id="${id}" aria-label="查看 ${escapeHTML(authorName(id))}">${avatar(member(id), true)}</button>`).join('');
  $('show-artifacts').hidden = false; $('artifact-count').textContent = data.artifacts.length;
  $('earlier').hidden = !hasEarlier;
  renderActivity();
  $('show-activity').hidden = false;
  if (panelView === 'activity') showActivity(false);
  renderMessages(!preserve || changedRoom);
  const last = messages.at(-1)?.seq || 0;
  if (!document.hidden) { seen[id] = last; localStorage.setItem('dropby-seen', JSON.stringify(seen)); }
}

function renderMessages(scrollToBottom) {
  const body = $('chat-body'), previousTop = body.scrollTop;
  const nearBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 110;
  let lastDate = '';
  $('messages').innerHTML = messages.length ? messages.map(m => {
    const date = dateLabel(m.created_at), divider = date !== lastDate ? `<div class="date-divider">${date}</div>` : ''; lastDate = date;
    if (m.author_id === 'system') {
      if (m.body.startsWith('这次运行暂未完成')) return `${divider}<details class="system-message"><summary>${timeLabel(m.created_at)} 的一次行动中断了 · 查看当时记录</summary><p>${escapeHTML(m.body)}</p><button class="quiet" data-action="activity">查看动态 →</button></details>`;
      return `${divider}<div class="system-message" role="status">${escapeHTML(m.body)}</div>`;
    }
    const isUser = m.author_id === 'user', person = isUser ? { name: '你', color: 'user' } : member(m.author_id);
    const quoted = messages.find(q => q.id === m.reply_to);
    return `${divider}<article class="message ${isUser ? 'user-message' : ''}" id="message-${m.id}">${avatar(person)}<div class="message-content"><div class="message-meta"><strong>${escapeHTML(authorName(m.author_id))}</strong><time datetime="${m.created_at}">${timeLabel(m.created_at)}</time></div><div class="message-bubble">${quoted ? `<span class="quote">${escapeHTML(authorName(quoted.author_id))}：${escapeHTML(quoted.body.slice(0, 90))}</span>` : ''}${richText(m.body)}</div><div class="message-actions"><button data-action="reply" data-id="${m.id}" aria-label="回复 ${escapeHTML(authorName(m.author_id))} 的消息">接这句话</button>${(m.evidence || []).map((id, i) => `<button data-action="evidence" data-id="${id}">查看行动${m.evidence.length > 1 ? ' ' + (i + 1) : ''} ↗</button>`).join('')}</div></div></article>`;
  }).join('') : `<div class="empty-chat"><div class="empty-avatars">${roomData.conversation.member_ids.map(id => avatar(member(id))).join('')}</div><strong>${roomData.conversation.kind === 'direct' ? `和 ${escapeHTML(authorName(roomData.conversation.member_ids[0]))} 打个招呼` : '从一个念头开始'}</strong><p>今天的小事、一个有趣的发现，都可以聊。<br>也可以先忙自己的，让朋友们慢慢找点话题。</p>${roomData.conversation.kind === 'direct' ? `<button class="quiet" data-action="profile" data-id="${roomData.conversation.member_ids[0]}">了解这位朋友 →</button>` : ''}</div>`;
  if (scrollToBottom || nearBottom) body.scrollTop = body.scrollHeight; else body.scrollTop = previousTop;
}

function renderReply() {
  $('reply-preview').hidden = !replyTo;
  $('reply-preview').innerHTML = replyTo ? `<span>接 ${escapeHTML(authorName(replyTo.author_id))} 的话：${escapeHTML(replyTo.body.slice(0, 140))}</span><button class="icon-button" data-action="clear-reply" aria-label="取消引用">×</button>` : '';
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
  panelView = view; panelRequest++;
  $('panel-title').textContent = title;
  $('panel-content').innerHTML = `${description ? `<p class="panel-description">${escapeHTML(description)}</p>` : ''}${body}`;
  $('inspector').hidden = false;
}
function closePanel() {
  $('inspector').hidden = true; panelView = ''; panelRequest++; preferenceQuestion = null;
}
function formError(form, error) { const el = form.querySelector('.form-error'); el.textContent = error.message; el.hidden = false; }
function pending(form, text) { const button = form.querySelector('button[type=submit]'); const label = button.textContent; button.textContent = text; button.disabled = true; return () => { button.textContent = label; button.disabled = false; }; }

function newMember() {
  avatarData = '';
  const colors = ['jade', 'apricot', 'blue', 'rose', 'violet', 'moss'];
  showPanel('认识新朋友', '其他的，边聊边认识。', `<form id="member-form">
    <div class="avatar-editor"><label class="avatar-upload" for="avatar-file" title="选择头像"><span id="avatar-preview">${avatar({name:'友',color:'jade'})}</span><span class="avatar-edit-mark">＋</span></label><input class="sr-only" id="avatar-file" type="file" accept="image/png,image/jpeg"><span>选择头像 <small>也可以用名字生成的头像</small></span></div>
    <div class="color-picker" aria-label="头像颜色">${colors.map((color,i)=>`<label class="color-choice ${color}"><input type="radio" name="color" value="${color}" ${i===0?'checked':''} aria-label="${['青灰','暖砂','雾蓝','浅玫瑰','浅紫','苔绿'][i]}"><span></span></label>`).join('')}</div>
    <label class="field"><span>名字</span><input name="name" maxlength="24" required placeholder="怎么称呼这位朋友？" autocomplete="off"></label>
    <div class="form-error" hidden role="alert"></div><button class="primary full-width" type="submit">开始聊天 →</button>
    <p class="field-hint">不需要填写问卷。偏好可以以后再说。</p>
  </form>`);
  $('panel-content').querySelector('input[name=name]').focus();
}

async function askPreference(id, button) {
  const m = member(id), request = panelRequest;
  button.disabled = true; button.textContent = '正在准备一个问题…';
  try {
    const result = await post('/api/member-questions', { direction: (m.direction || m.profile).slice(0, 2000) });
    // 用户已经关闭或切换详情时，迟到的模型返回不能把页面重新打开。
    if (request !== panelRequest) return;
    const q = result.questions[0];
    showPanel(`认识 ${m.name}`, '只聊这一个。可以自己回答，也可以跳过。', `<form id="preference-form" data-id="${m.id}"><fieldset class="question"><legend>${escapeHTML(q.text)}</legend>${q.options.map((option,i)=>`<label class="option"><input type="radio" name="option" value="${i}"><span>${escapeHTML(option)}</span></label>`).join('')}<label class="field"><span class="sr-only">自己的回答</span><textarea name="answer" maxlength="2000" placeholder="或者，用你自己的话说…"></textarea></label></fieldset><div class="form-error" hidden role="alert"></div><div class="panel-footer"><button class="quiet" type="button" data-action="close-panel">跳过，去聊天</button><button class="primary" type="submit">记住这个偏好</button></div></form>`);
    preferenceQuestion = q;
  } catch (error) { if (request === panelRequest) { button.disabled = false; button.textContent = '聊一个偏好（可选）'; toast(error.message); } }
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
const actionLabels = {search:'搜索网页',read_page:'阅读网页',save_artifact:'保存成果',remember:'更新经历',schedule_exploration:'安排探索',read_history:'回顾聊天',read_artifact:'查看成果',read_tool_result:'核对已有材料',read_memory:'回顾经历',send_private:'发起私聊'};
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
    const label = action?.label || (!p ? '还没有参与记录' : p.action==='pass' ? '暂时没有补充' : p.action==='error' ? '上次判断未完成' : p.selected ? (p.action==='explore' ? '已安排探索' : '最近参与了交流') : '有想法，尚未发言');
    return `<div class="member-activity">${avatar(member(id),true)}<div><strong>${escapeHTML(authorName(id))}</strong><span>${escapeHTML(label)}</span>${p ? `<details data-detail="person-${id}"><summary>这次的参与理由</summary><p>${escapeHTML(p.reason || '没有补充理由')}</p><small>${timeLabel(p.created_at)}</small></details>` : ''}</div></div>`;
  }).join('');
  showPanel('最近动态', '', `<p class="background-note">浏览器关掉后，服务仍会继续探索。电脑睡眠或服务停止时会暂停，重新运行后恢复。每 ${state.explore_interval_minutes || 10} 分钟提供一次自主机会，有新发现才分享。</p><h3 class="section-label">朋友们</h3>${people}<h3 class="section-label">后台行动</h3>${jobs.filter(w=>w.kind==='research' || ['failed','paused','waiting_budget'].includes(w.status)).slice(0,12).map(w=>`<details class="work-item" data-detail="${w.id}"><summary><span>${escapeHTML(w.member_id ? authorName(w.member_id) : '这段交流')}</span><span class="status ${w.status}">${escapeHTML(statusLabels[w.status]||w.status)}</span><strong>${escapeHTML(w.instruction.slice(0,90))}</strong></summary><p>${escapeHTML(w.result || w.reason || '还没有执行结果')}</p>${w.status==='pending' ? `<small>下次机会 ${new Date(w.due_at).toLocaleString('zh-CN')}</small>`:''}${['failed','paused'].includes(w.status) ? `<button class="quiet" data-action="continue-work" data-id="${w.id}">在聊天中继续这个方向 →</button>`:''}</details>`).join('') || '<p class="empty-note">暂时没有安排研究。普通聊天不必变成任务。</p>'}<h3 class="section-label">实际做过的事</h3>${actions.slice(0,15).map(a=>`<button class="action-item" data-action="evidence" data-id="${a.id}"><span>${escapeHTML(authorName(a.member_id))} · ${escapeHTML(actionLabels[a.name]||a.name)}</span><small>${timeLabel(a.created_at)} · ${escapeHTML(statusLabels[a.status]||a.status)} ↗</small></button>`).join('')||'<p class="empty-note">还没有工具行动。</p>'}<p class="budget-note">今日估算 $${(state.usage.cost_micros/1e6).toFixed(2)} / $${(state.daily_budget_micros/1e6).toFixed(2)}</p>`, 'activity');
  if (!scroll) { $('panel-content').scrollTop = top; for (const d of $('panel-content').querySelectorAll('details[data-detail]')) d.open = expanded.includes(d.dataset.detail); }
}
function newGroup() {
  if (!state.members.length) { newMember(); return; }
  showPanel('一起坐坐', '逐个邀请你想放在一起的朋友。群可以没有预设话题。', `<form id="group-form"><label class="field"><span>群名 <span class="field-hint">（可留空）</span></span><input name="name" maxlength="80" placeholder="随便聊聊"></label><span class="field-label">邀请朋友</span>${state.members.map(m => `<label class="member-choice">${avatar(m)}<span class="copy"><strong>${escapeHTML(m.name)}</strong><small>${escapeHTML(m.direction.slice(0, 70))}</small></span><input type="checkbox" name="members" value="${m.id}" aria-label="邀请 ${escapeHTML(m.name)}"></label>`).join('')}<div class="form-error" hidden role="alert"></div><div class="panel-footer"><button class="primary" type="submit">开一个群 →</button></div></form>`);
}
function profile(id) {
  const m = member(id); if (!m) return;
  showPanel(m.name, '', `<div class="profile-head">${avatar(m)}<p class="panel-description">${escapeHTML(m.direction)}</p></div><div class="profile-body">${escapeHTML(m.profile)}</div><p class="profile-caption">这些是相识时的起点。之后的判断与关系，会随着实际共同经历而变化。</p><div class="profile-actions"><button class="primary" data-action="direct" data-id="${m.id}">单独聊聊 →</button><button class="quiet" data-action="preference" data-id="${m.id}">聊一个偏好（可选）</button></div>`);
}
function showArtifacts() {
  if (!roomData) return;
  showPanel('一起留下的东西', '从聊天里长出来的作品和发现，都可以继续修改。', roomData.artifacts.length ? roomData.artifacts.map(a => `<button class="artifact-item" data-action="artifact" data-id="${a.id}"><strong>${escapeHTML(a.title)} ↗</strong><small>${escapeHTML(authorName(a.member_id))} · 第 ${a.version} 版 · ${dateLabel(a.created_at)}</small></button>`).join('') : '<p class="panel-description">还没有保存的成果。等一个念头长出形状，再把它留在这里。</p>');
}
function renderArtifact(index = 0) {
  const a = currentVersions[index];
  showPanel(a.title, `由 ${authorName(a.member_id)} 保存 · ${dateLabel(a.created_at)}`, `<div class="artifact-toolbar"><select class="version-select" id="artifact-version" aria-label="选择成果版本">${currentVersions.map((v, i) => `<option value="${i}" ${index === i ? 'selected' : ''}>第 ${v.version} 版 · ${dateLabel(v.created_at)} ${timeLabel(v.created_at)}</option>`).join('')}</select><a href="/api/artifacts/${a.id}?download=1" download>下载最新版</a></div><div class="artifact-content">${richText(a.content)}</div><div class="panel-footer"><button class="primary" data-action="continue-artifact" data-id="${a.id}">继续聊这个成果 ↗</button></div>`);
}

document.addEventListener('click', async event => {
  const button = event.target.closest('[data-action]'); if (!button) return;
  const { action, id } = button.dataset;
  try {
    if (action === 'open-room') await openRoom(id);
    if (action === 'profile') profile(id);
    if (action === 'close-panel') { closePanel(); $('message-input').focus(); }
    if (action === 'activity') showActivity();
    if (action === 'preference') await askPreference(id, button);
    if (action === 'continue-work') { const w = roomData.work.find(w=>w.id===id); closePanel(); $('message-input').value = `继续这件事：${w.instruction}`; $('message-input').focus(); }
    if (action === 'reply') { replyTo = messages.find(m => m.id === id); renderReply(); $('message-input').focus(); }
    if (action === 'clear-reply') { replyTo = null; renderReply(); }
    if (action === 'mention') {
      const input = $('message-input'), caret = input.selectionStart;
      const prefix = input.value.slice(0, caret).replace(/@[^\s@]{0,24}$/, `@${authorName(id)} `);
      input.value = prefix + input.value.slice(caret); input.focus(); input.setSelectionRange(prefix.length, prefix.length); $('mention-options').hidden = true;
    }
    if (action === 'direct') { button.disabled = true; const c = await post('/api/conversations', { name: '', kind: 'direct', member_ids: [id] }); closePanel(); selected = c.id; await refresh(); await openRoom(c.id); }
    if (action === 'artifact') { const data = await api(`/api/artifacts/${id}`); currentVersions = data.versions; renderArtifact(); }
    if (action === 'continue-artifact') { const a = currentVersions[0]; closePanel(); $('message-input').value = `关于《${a.title}》，`; $('message-input').focus(); }
    if (action === 'evidence') {
      const run = await api(`/api/tool-runs/${id}`);
      const labels = { search: '网页搜索', read_page: '网页阅读', save_artifact: '保存成果', remember: '更新经历', schedule_exploration: '安排探索', read_history: '查看历史', read_artifact: '查看成果', send_private: '主动私聊' };
      showPanel(labels[run.name] || '实际行动', `${authorName(run.member_id)} · ${dateLabel(run.created_at)} ${timeLabel(run.created_at)}`, `<div class="evidence"><p>执行结果：${escapeHTML({ succeeded: '已完成', failed: '未完成', cancelled: '被新消息打断', running: '进行中' }[run.status] || run.status)}</p><div class="action-summary">${Object.entries(run.arguments).map(([key,value])=>`<p>${escapeHTML(String(value))}</p>`).join('')}</div><details><summary>查看完整记录</summary><pre>${escapeHTML(run.result)}</pre></details></div>`);
    }
  } catch (error) { toast(error.message); button.disabled = false; }
});

document.addEventListener('submit', async event => {
  const form = event.target;
  if (!['member-form','preference-form','group-form'].includes(form.id)) return;
  event.preventDefault(); const reset = pending(form, form.id==='preference-form' ? '正在记住…' : '正在准备…');
  try {
    const data = new FormData(form);
    if (form.id==='member-form') {
      const m = await post('/api/members', {name:data.get('name').trim(), color:data.get('color'), avatar:avatarData});
      const c = await post('/api/conversations', {kind:'direct',member_ids:[m.id]});
      closePanel(); selected=c.id; await refresh(); await openRoom(c.id); $('message-input').focus();
    } else if (form.id==='preference-form') {
      const choice=data.get('option'), answer=data.get('answer').trim() || (choice===null ? '' : preferenceQuestion.options[Number(choice)]);
      if (!answer) { closePanel(); return; }
      await post(`/api/members/${form.dataset.id}/preferences`, {question:preferenceQuestion, answer});
      closePanel(); await refresh(); toast('记住了，接着聊吧。');
    } else {
      const ids=data.getAll('members'); if (!ids.length) throw new Error('先邀请至少一位朋友吧。');
      const c=await post('/api/conversations',{name:data.get('name'),kind:'group',member_ids:ids});
      closePanel(); selected=c.id; await refresh(); await openRoom(c.id);
    }
  } catch (error) { formError(form,error); reset(); }
});

// 头像在浏览器缩成 128px 的本地图片后再保存，不上传原始照片或 EXIF 信息。
document.addEventListener('change', async event => {
  if (event.target.id==='avatar-file') {
    const file=event.target.files[0]; if (!file) return;
    try {
      if (!['image/png','image/jpeg'].includes(file.type) || file.size>10*1024*1024) throw new Error('请选择 10MB 以内的 PNG 或 JPEG 图片。');
      const bitmap=await createImageBitmap(file), canvas=document.createElement('canvas');
      canvas.width=128; canvas.height=128;
      const size=Math.min(bitmap.width,bitmap.height);
      canvas.getContext('2d').drawImage(bitmap,(bitmap.width-size)/2,(bitmap.height-size)/2,size,size,0,0,128,128);
      bitmap.close(); avatarData=canvas.toDataURL('image/png');
      if (avatarData.length>88000) avatarData=canvas.toDataURL('image/jpeg',0.85);
      $('avatar-preview').innerHTML=avatar({avatar:avatarData});
    } catch(error) { toast(error.message); }
  }
  if (event.target.name==='color' && $('avatar-preview')) { avatarData=''; $('avatar-preview').innerHTML=avatar({name:document.querySelector('#member-form input[name=name]').value||'友',color:event.target.value}); }
});
document.addEventListener('input',event=>{
  if (event.target.matches('#member-form input[name=name]') && !avatarData) $('avatar-preview').innerHTML=avatar({name:event.target.value||'友',color:document.querySelector('#member-form input[name=color]:checked').value});
});
document.addEventListener('keydown',event=>{ if (event.key==='Escape' && !$('inspector').hidden) closePanel(); });

$('composer').addEventListener('submit', async event => {
  event.preventDefault(); if (!selected) return;
  const body = $('message-input').value.trim(); if (!body) return;
  const id = selected, original = $('message-input').value;
  const mentions = roomData.conversation.member_ids.filter(id => {
    const name = authorName(id).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    return new RegExp(`(^|\\s)@${name}(?=$|\\s|[，。！？：:,!?])`).test(body);
  });
  $('send').disabled = true;
  try {
    await post(`/api/conversations/${id}/messages`, { body, reply_to: replyTo?.id || '', mentions });
    if (id === selected) { if ($('message-input').value === original) $('message-input').value = ''; replyTo = null; renderReply(); $('mention-options').hidden = true; await loadConversation(id, true); $('chat-body').scrollTop = $('chat-body').scrollHeight; }
    scheduleRefresh();
  } catch (error) { toast(error.message); }
  finally { $('send').disabled = false; $('message-input').focus(); }
});
$('message-input').addEventListener('keydown', event => { if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) { event.preventDefault(); if (!$('send').disabled) $('composer').requestSubmit(); } });
$('message-input').addEventListener('input', showMentions);
$('new-member').onclick = newMember; $('new-group').onclick = newGroup;
$('welcome-action').onclick = () => state.members.length ? newGroup() : newMember();
$('show-artifacts').onclick = showArtifacts; $('show-activity').onclick = () => showActivity();
$('mobile-nav').onclick = () => $('sidebar').classList.toggle('open');
$('main').addEventListener('click', event => { if (!event.target.closest('#mobile-nav')) $('sidebar').classList.remove('open'); });
document.addEventListener('change', event => { if (event.target.id === 'artifact-version') renderArtifact(Number(event.target.value)); });
$('earlier').onclick = async () => {
  try { const room = selected, height = $('chat-body').scrollHeight, top = $('chat-body').scrollTop; const data = await api(`/api/conversations/${room}?before=${messages[0]?.seq || 0}`); if (room !== selected) return; const combined = new Map([...data.messages, ...messages].map(m => [m.id, m])); messages = [...combined.values()].sort((a, b) => a.seq - b.seq); hasEarlier = data.has_earlier; renderMessages(false); $('chat-body').scrollTop = top + $('chat-body').scrollHeight - height; $('earlier').hidden = !hasEarlier; }
  catch (error) { toast(error.message); }
};
$('notifications').onclick = async () => { if (!('Notification' in window)) return toast('这个浏览器不支持系统通知，聊天中的新消息仍会保留。'); const result = await Notification.requestPermission(); toast(result === 'granted' ? '已开启通知。只有成员判断值得及时提醒的消息才会通知你。' : '继续在聊天里查看新消息就好。'); };
document.addEventListener('visibilitychange', () => { if (!document.hidden) scheduleRefresh(); });

const stream = new EventSource('/api/events');
stream.addEventListener('ready', () => { $('connection-dot').className = 'ready'; $('connection-label').textContent = '已连接'; scheduleRefresh(); });
stream.addEventListener('change', scheduleRefresh);
stream.onerror = () => { $('connection-dot').className = ''; $('connection-label').textContent = '正在重连'; };
refresh();

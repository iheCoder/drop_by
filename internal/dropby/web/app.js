'use strict';

// 前端只呈现真实持久化记录。没有示例聊天、模拟输入状态或浏览器端后台定时生成。
const $ = id => document.getElementById(id);
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let state = { members: [], conversations: [], active_rooms: [] };
let selected = localStorage.getItem('dropby-room') || '';
let roomData = null, messages = [], replyTo = null, loadSequence = 0, hasEarlier = false;
let refreshTimer, toastTimer, wizard = null, currentVersions = [];
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
function avatar(m, small = false) { return `<span class="avatar ${escapeHTML(m?.color || 'jade')} ${small ? 'small' : ''}" aria-hidden="true">${escapeHTML(Array.from(m?.name || '友')[0])}</span>`; }
function timeLabel(date) { return new Date(date).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }); }
function dateLabel(date) { return new Date(date).toLocaleDateString('zh-CN', { month: 'long', day: 'numeric' }); }

// 先转义，再只允许 HTTP(S) 链接；模型产出的 HTML、脚本和任意协议都不会执行。
function richText(text) {
  const parts = [], pattern = /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>"\u3000-\u303f\uff00-\uffef]+)/g;
  let start = 0, match;
  while ((match = pattern.exec(text))) {
    parts.push(escapeHTML(text.slice(start, match.index)));
    const href = match[2] || match[3], label = match[1] || href;
    parts.push(`<a href="${escapeHTML(href)}" target="_blank" rel="noopener noreferrer">${escapeHTML(label)}</a>`);
    start = match.index + match[0].length;
  }
  parts.push(escapeHTML(text.slice(start)));
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
    const banner = !state.model_ready ? '模型服务尚未配置。设置服务端的 DEEPSEEK_API_KEY 后重启，便可以认识成员、开始聊天。' : state.usage.calls >= state.daily_calls || state.usage.cost_micros >= state.daily_budget_micros ? '今日运行预算已用完。聊天和成果仍可查看，未完工作会在下一预算日恢复。' : !state.search_ready ? '网页探索尚未接通，成员目前不能进行真实搜索或读取网页。' : '';
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
  $('member-list').innerHTML = state.members.length ? state.members.map(m => `<button class="person-button" data-action="profile" data-id="${m.id}">${avatar(m, true)}<span class="person-name">${escapeHTML(m.name)}</span></button>`).join('') : '<p class="side-empty">每位朋友都有自己的关注。<br>从你给的大方向慢慢长出来。</p>';
}
function renderWelcome() {
  $('welcome').hidden = false; $('chat-body').hidden = true; $('composer-wrap').hidden = true; $('show-artifacts').hidden = true;
  $('room-title').textContent = '一个可以随时回来的地方'; $('room-subtitle').textContent = '认识朋友，聊聊最近的发现。';
  $('header-members').innerHTML = '';
  $('welcome-action').innerHTML = state.members.length ? '邀请朋友，开一个群 <span>↗</span>' : '认识第一位朋友 <span>↗</span>';
}

async function openRoom(id) {
  if (id !== selected) { selected = id; messages = []; replyTo = null; $('message-input').value = ''; renderReply(); }
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
  $('activity').hidden = !state.active_rooms.includes(id);
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
    if (m.author_id === 'system') return `${divider}<div class="system-message" role="status">${escapeHTML(m.body)}</div>`;
    const isUser = m.author_id === 'user', person = isUser ? { name: '你', color: 'user' } : member(m.author_id);
    const quoted = messages.find(q => q.id === m.reply_to);
    return `${divider}<article class="message ${isUser ? 'user-message' : ''}" id="message-${m.id}">${avatar(person)}<div class="message-content"><div class="message-meta"><strong>${escapeHTML(authorName(m.author_id))}</strong><time datetime="${m.created_at}">${timeLabel(m.created_at)}</time></div><div class="message-bubble">${quoted ? `<span class="quote">${escapeHTML(authorName(quoted.author_id))}：${escapeHTML(quoted.body.slice(0, 90))}</span>` : ''}${richText(m.body)}</div><div class="message-actions"><button data-action="reply" data-id="${m.id}" aria-label="回复 ${escapeHTML(authorName(m.author_id))} 的消息">接这句话</button>${(m.evidence || []).map((id, i) => `<button data-action="evidence" data-id="${id}">查看行动${m.evidence.length > 1 ? ' ' + (i + 1) : ''} ↗</button>`).join('')}</div></div></article>`;
  }).join('') : '<div class="empty-chat"><strong>都到齐了，随便坐。</strong>可以分享一个念头、一条链接，或者今天的小事。<br>不用每个人都接话。</div>';
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

function showDialog(title, description, body) {
  $('dialog-content').innerHTML = `<div class="dialog-heading"><h2 id="dialog-title">${escapeHTML(title)}</h2><button class="icon-button" data-action="close-dialog" aria-label="关闭">×</button></div>${description ? `<p class="dialog-description">${escapeHTML(description)}</p>` : ''}${body}`;
  if (!$('dialog').open) $('dialog').showModal();
}
function formError(form, error) { const el = form.querySelector('.form-error'); el.textContent = error.message; el.hidden = false; }
function pending(form, text) { const button = form.querySelector('button[type=submit]'); const label = button.textContent; button.textContent = text; button.disabled = true; return () => { button.textContent = label; button.disabled = false; }; }

function newMember() {
  wizard = null;
  const colors = ['jade', 'apricot', 'blue', 'rose', 'violet', 'moss'];
  showDialog('认识一位新朋友', '先给一点大方向，再通过几个具体情境，看看他会如何选择。', `<form id="member-direction-form"><label class="field"><span>怎么称呼</span><input name="name" maxlength="24" required placeholder="给这位朋友起个名字" autocomplete="off"></label><label class="field"><span>你希望他是怎样的人</span><textarea name="direction" maxlength="2000" required placeholder="比如：好奇日常事物背后的设计，喜欢动手试试；有自己的审美，但愿意被有趣的证据说服。"></textarea></label><span class="field-label">选一个头像颜色</span><div class="color-picker">${colors.map((color, i) => `<label class="color-choice"><input type="radio" name="color" value="${color}" ${i === 0 ? 'checked' : ''} aria-label="${['青绿','杏色','蓝色','玫瑰','紫色','苔绿'][i]}">${avatar({ name: '友', color })}</label>`).join('')}</div><p class="field-hint">同一成员会记得自己的跨群与私聊经历。首版请用非敏感内容体验，不要放入需要严格隔离的私人资料。</p><div class="form-error" hidden role="alert"></div><div class="dialog-footer"><button class="primary" type="submit">看看情境问题 →</button></div></form>`);
}
function showQuestions() {
  showDialog(`再认识一下${wizard.name}`, '这些选择只是起点。每一道题都可以用你自己的话回答。', `<form id="member-answers-form">${wizard.questions.map((q, i) => `<fieldset class="question"><legend>${i + 1}. ${escapeHTML(q.text)}</legend>${(q.options || []).map((option, j) => `<label class="option"><input type="radio" name="option-${i}" value="${j}"><span>${escapeHTML(option)}</span></label>`).join('')}<label class="field"><span class="sr-only">第 ${i + 1} 题自填回答</span><textarea name="answer-${i}" maxlength="2000" placeholder="${q.options?.length ? '或者，说说你自己的答案…' : '说说你想象中的反应…'}"></textarea></label></fieldset>`).join('')}<div class="form-error" hidden role="alert"></div><div class="dialog-footer"><button class="primary" type="submit">认识${escapeHTML(wizard.name)}</button></div></form>`);
}
function newGroup() {
  if (!state.members.length) { newMember(); return; }
  showDialog('一起坐坐', '逐个邀请你想放在一起的朋友。群可以没有预设话题。', `<form id="group-form"><label class="field"><span>群名 <span class="field-hint">（可留空）</span></span><input name="name" maxlength="80" placeholder="随便聊聊"></label><span class="field-label">邀请朋友</span>${state.members.map(m => `<label class="member-choice">${avatar(m)}<span class="copy"><strong>${escapeHTML(m.name)}</strong><small>${escapeHTML(m.direction.slice(0, 70))}</small></span><input type="checkbox" name="members" value="${m.id}" aria-label="邀请 ${escapeHTML(m.name)}"></label>`).join('')}<div class="form-error" hidden role="alert"></div><div class="dialog-footer"><button class="primary" type="submit">开一个群 →</button></div></form>`);
}
function profile(id) {
  const m = member(id); if (!m) return;
  showDialog(m.name, '', `<div class="profile-head">${avatar(m)}<p class="dialog-description">${escapeHTML(m.direction)}</p></div><div class="profile-body">${escapeHTML(m.profile)}</div><p class="profile-caption">这些是相识时的起点。之后的判断与关系，会随着实际共同经历而变化。</p><div class="dialog-footer"><button class="primary" data-action="direct" data-id="${m.id}">单独聊聊 ↗</button></div>`);
}
function showArtifacts() {
  if (!roomData) return;
  showDialog('一起留下的东西', '从聊天里长出来的作品和发现，都可以继续修改。', roomData.artifacts.length ? roomData.artifacts.map(a => `<button class="artifact-item" data-action="artifact" data-id="${a.id}"><strong>${escapeHTML(a.title)} ↗</strong><small>${escapeHTML(authorName(a.member_id))} · 第 ${a.version} 版 · ${dateLabel(a.created_at)}</small></button>`).join('') : '<p class="dialog-description">还没有保存的成果。等一个念头长出形状，再把它留在这里。</p>');
}
function renderArtifact(index = 0) {
  const a = currentVersions[index];
  showDialog(a.title, `由 ${authorName(a.member_id)} 保存 · ${dateLabel(a.created_at)}`, `<div class="artifact-toolbar"><select class="version-select" id="artifact-version" aria-label="选择成果版本">${currentVersions.map((v, i) => `<option value="${i}" ${index === i ? 'selected' : ''}>第 ${v.version} 版 · ${dateLabel(v.created_at)} ${timeLabel(v.created_at)}</option>`).join('')}</select><a href="/api/artifacts/${a.id}?download=1" download>下载最新版</a></div><div class="artifact-content">${richText(a.content)}</div><div class="dialog-footer"><button class="primary" data-action="continue-artifact" data-id="${a.id}">继续聊这个成果 ↗</button></div>`);
}

document.addEventListener('click', async event => {
  const button = event.target.closest('[data-action]'); if (!button) return;
  const { action, id } = button.dataset;
  try {
    if (action === 'open-room') await openRoom(id);
    if (action === 'profile') profile(id);
    if (action === 'close-dialog') $('dialog').close();
    if (action === 'reply') { replyTo = messages.find(m => m.id === id); renderReply(); $('message-input').focus(); }
    if (action === 'clear-reply') { replyTo = null; renderReply(); }
    if (action === 'mention') {
      const input = $('message-input'), caret = input.selectionStart;
      const prefix = input.value.slice(0, caret).replace(/@[^\s@]{0,24}$/, `@${authorName(id)} `);
      input.value = prefix + input.value.slice(caret); input.focus(); input.setSelectionRange(prefix.length, prefix.length); $('mention-options').hidden = true;
    }
    if (action === 'direct') { button.disabled = true; const c = await post('/api/conversations', { name: '', kind: 'direct', member_ids: [id] }); $('dialog').close(); selected = c.id; await refresh(); await openRoom(c.id); }
    if (action === 'artifact') { const data = await api(`/api/artifacts/${id}`); currentVersions = data.versions; renderArtifact(); }
    if (action === 'continue-artifact') { const a = currentVersions[0]; $('dialog').close(); $('message-input').value = `关于《${a.title}》，`; $('message-input').focus(); }
    if (action === 'evidence') {
      const run = await api(`/api/tool-runs/${id}`);
      const labels = { search: '网页搜索', read_page: '网页阅读', save_artifact: '保存成果', remember: '更新经历', schedule_exploration: '安排探索', read_history: '查看历史', read_artifact: '查看成果', send_private: '主动私聊' };
      showDialog(labels[run.name] || '实际行动', `${authorName(run.member_id)} · ${dateLabel(run.created_at)} ${timeLabel(run.created_at)}`, `<div class="evidence"><p>执行结果：${escapeHTML({ succeeded: '已完成', failed: '未完成', cancelled: '被新消息打断', running: '进行中' }[run.status] || run.status)}</p><pre>${escapeHTML(JSON.stringify(run.arguments, null, 2))}</pre><pre>${escapeHTML(run.result)}</pre></div>`);
    }
  } catch (error) { toast(error.message); button.disabled = false; }
});

document.addEventListener('submit', async event => {
  const form = event.target;
  if (!['member-direction-form', 'member-answers-form', 'group-form'].includes(form.id)) return;
  event.preventDefault(); const reset = pending(form, form.id === 'group-form' ? '正在邀请…' : '正在认识这位朋友…');
  try {
    const data = new FormData(form);
    if (form.id === 'member-direction-form') {
      wizard = { name: data.get('name').trim(), direction: data.get('direction').trim(), color: data.get('color') };
      const result = await post('/api/member-questions', { direction: wizard.direction }); wizard.questions = result.questions; showQuestions();
    } else if (form.id === 'member-answers-form') {
      const answers = wizard.questions.map((q, i) => { const typed = data.get(`answer-${i}`).trim(); const choice = data.get(`option-${i}`); return typed || (choice !== null ? q.options[Number(choice)] : ''); });
      if (answers.some(a => !a)) throw new Error('每个情境都需要一个回答，可以选一项，也可以自己写。');
      const m = await post('/api/members', { ...wizard, answers }); $('dialog').close(); await refresh(); profile(m.id);
    } else {
      const ids = data.getAll('members'); if (!ids.length) throw new Error('先邀请至少一位朋友吧。');
      const c = await post('/api/conversations', { name: data.get('name'), kind: 'group', member_ids: ids }); $('dialog').close(); selected = c.id; await refresh(); await openRoom(c.id);
    }
  } catch (error) { formError(form, error); reset(); }
});

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
$('show-artifacts').onclick = showArtifacts;
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

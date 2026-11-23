'use strict';

function sourceLink(id) {
  return id ? `<button class="quiet activity-source" data-action="jump-message" data-id="${escapeHTML(id)}">${icon('message-circle')}在聊天中查看</button>` : '';
}
function technicalJSON(value) {
  return `<pre tabindex="0">${escapeHTML(JSON.stringify(value,null,2))}</pre>`;
}
function technicalRecord(value) {
  return `<details class="technical-record"><summary>技术细节</summary>${technicalJSON(value)}</details>`;
}
function activityPanelName() {
  return roomData?.conversation.kind === 'group' ? '成员动态' : '朋友动态';
}
function activityMember(id) {
  if (roomData?.conversation.kind !== 'group') return '';
  return id ? `<span class="activity-byline">${avatar(member(id),true)}<span>${escapeHTML(authorName(id))}</span></span>`
    : '<span class="activity-byline">群聊</span>';
}

// Acknowledging a notice only changes its visibility on this device. Every
// diagnostic keeps its original work ID across recovery and refresh.
const readReplyNotices = new Set();
try {
  for (const id of JSON.parse(localStorage.getItem('dropby-read-reply-notices') || '[]')) readReplyNotices.add(id);
} catch { /* The next notice can still be acknowledged in this page. */ }
function renderActivity() {
  const active = (state.activities || []).filter(a => a.conversation_id === selected);
  $('activity').hidden = !active.length;
  $('activity').innerHTML = active.map(a => `<button data-action="activity"><span class="activity-dot"></span>${escapeHTML(authorName(a.member_id))} · ${escapeHTML(a.label)}</button>`).join('');
  $('work-count').textContent = roomData.plans?.length || '';
  const title = activityPanelName();
  $('show-activity').title = title;
  $('show-activity').setAttribute('aria-label', '查看' + title);
}
function replyNoticeFor(message) {
  const notice = roomData?.reply_notice;
  if (!notice || notice.source_id !== message.id || readReplyNotices.has(notice.work_id)) return '';
  return `<button class="work-notice" data-action="activity-notice">${activityMember(notice.member_id)}<span>这次回复未发出 · 查看详情</span></button>`;
}
function openReplyNotice() {
  const notice = roomData?.reply_notice;
  if (!notice) return;
  readReplyNotices.add(notice.work_id);
  try { localStorage.setItem('dropby-read-reply-notices', JSON.stringify([...readReplyNotices])); }
  catch { /* In-memory acknowledgement is enough until the next reload. */ }
  showActivity();
  renderMessages(false);
  openActivityRecord(notice.work_id, notice.member_id);
}

function activityQuote(message) {
  if (!message) return '';
  return `<figure class="activity-quote"><figcaption>${escapeHTML(authorName(message.author_id))} · ${escapeHTML(activityTime(message.at))}</figcaption><blockquote>${richText(message.body)}</blockquote>${message.question?.options?.length ? `<ul>${message.question.options.map(o=>`<li>${escapeHTML(o)}</li>`).join('')}</ul>` : ''}</figure>`;
}
function activityTime(at) { return `${dateLabel(at)} ${timeLabel(at)}`; }
function activityPlanTitle(w) {
  const body = w.display?.detail || '';
  const first = body.split(/[。！!\n；;]/, 1)[0].trim();
  return first || w.display?.title || '后续安排';
}
function activityPlans() {
  const plans = roomData.plans || [];
  return plans.length ? plans.map(w=>{
    const d = w.display || {};
    const title = activityPlanTitle(w);
    return `<details class="activity-work" data-plan="${escapeHTML(w.id)}"><summary><span>${activityMember(w.member_id)}<strong>${escapeHTML(title)}</strong><small>${escapeHTML(d.status || '')}${w.status === 'pending' && w.due_at ? ' · '+escapeHTML(activityTime(w.due_at)) : ''}</small></span>${icon('chevron-down')}</summary><p>${escapeHTML(d.detail || title)}</p>${technicalRecord({kind:w.kind,status:w.status,instruction:w.instruction,result:w.result})}</details>`;
  }).join('') : '<p class="activity-empty">暂时没有安排。</p>';
}
function wakeSection() {
  const rhythms = roomData.wake_rhythms || {};
  const blocks = roomData.conversation.member_ids.map(id=>{
    const r = rhythms[id];
    if (!r) return '';
    const lines = (r.summary || []).map(escapeHTML).join('<br>');
    const next = r.next_label ? `<br>下一次大约在 ${escapeHTML(r.next_label)}。` : '';
    const note = r.enabled ? '<br>实际时间会前后浮动，醒来也不一定发消息。' : '';
    return `<div class="activity-person">${avatar(member(id),true)}<div>${roomData.conversation.kind === 'group' ? `<strong>${escapeHTML(authorName(id))}</strong>` : ''}<div class="wake-lines">${lines}${next}${note}</div></div></div>`;
  }).join('');
  return blocks ? `<h3 class="section-label">自主活动</h3>${blocks}` : '';
}
function activityPeople() {
  const group = roomData.conversation.kind === 'group';
  return roomData.conversation.member_ids.map(id=>{
    const focus = roomData.member_states?.find(s=>s.member_id===id)?.body || state.personal_focus?.[id]?.body || '还没有新的近况';
    return `<div class="activity-person">${avatar(member(id),true)}<div>${group ? `<strong>${escapeHTML(authorName(id))}</strong>` : ''}<p>${escapeHTML(focus)}</p></div></div>`;
  }).join('');
}
function activityFacts(entry) {
  return (entry.facts || []).map(f=>{
    const more = f.at ? activityTime(f.at) : f.more;
    let text = f.text;
    if (f.message_excerpt) {
      const excerpt = document.createElement('div');
      excerpt.innerHTML = richText(text);
      text = excerpt.textContent.trim();
    }
    return `<span class="activity-fact"><span>${escapeHTML(f.label)}</span><span title="${escapeHTML(text)}">${escapeHTML(text)}</span>${more ? `<small>${escapeHTML(more)}</small>` : ''}</span>`;
  }).join('');
}
function activityEntryKey(entry) { return entry.work_id + ':' + entry.member_id; }
function activityMeta(entry) {
  return [entry.trigger, activityTime(entry.at), entry.status].filter(Boolean).map(escapeHTML).join(' · ');
}
function activityRow(entry) {
  return `<button type="button" class="activity-entry" data-action="activity-record" data-id="${escapeHTML(entry.work_id)}" data-member="${escapeHTML(entry.member_id)}"><span class="activity-entry-copy">${activityMember(entry.member_id)}<strong${entry.failed ? ' class="activity-failure"' : ''}>${escapeHTML(entry.title)}</strong>${activityFacts(entry)}<small>${activityMeta(entry)}</small></span>${icon('chevron-right')}</button>`;
}

let activityFeed = null;
function disposeActivity() {
  activityFeed?.observer?.disconnect();
  activityFeed = null;
}
function refreshActivityOverview() {
  const parts = [['activity-people',activityPeople()], ['activity-plans',activityPlans()], ['activity-wake',wakeSection()]];
  for (const [id,html] of parts) {
    const el = activityFeed?.overview.querySelector('#'+id);
    if (!el || el.dataset.rendered === html) continue;
    // Only update sections whose values changed. Keep expanded plans and the
    // already paginated action list in place during unrelated SSE updates.
    const expanded = new Set([...el.querySelectorAll('details[data-plan][open]')].map(d=>d.dataset.plan));
    el.innerHTML = html;
    el.dataset.rendered = html;
    for (const d of el.querySelectorAll('details[data-plan]')) d.open = expanded.has(d.dataset.plan);
  }
}
function showActivity() {
  if (!roomData) return;
  showPanel(activityPanelName(), '', `<div class="activity-view" id="activity-overview"><div id="activity-wake"></div><h3 class="section-label">近况</h3><div id="activity-people"></div><h3 class="section-label">后续安排</h3><div id="activity-plans"></div><h3 class="section-label">最近行动</h3><div id="activity-rows"></div><p id="activity-page-status" class="activity-empty" role="status"></p><div id="activity-end" aria-hidden="true"></div></div>`, 'activity');
  const overview = $('activity-overview');
  activityFeed = {room:selected, overview, rows:$('activity-rows'), status:$('activity-page-status'), end:$('activity-end'), next:'', loaded:false, loading:false, seen:new Set(), top:0};
  const feed = activityFeed;
  feed.observer = new IntersectionObserver(entries=>{
    if (entries.some(e=>e.isIntersecting)) void loadActivityPage(feed);
  }, {root:$('panel-content'), rootMargin:'0px 0px 120px 0px'});
  refreshActivityOverview();
  $('panel-content').scrollTop = 0;
  void loadActivityPage(feed);
}
async function loadActivityPage(feed) {
  if (feed !== activityFeed || panelView !== 'activity' || feed.room !== selected || feed.loading || feed.refreshing || (feed.loaded && !feed.next)) return;
  feed.loading = true;
  feed.observer.unobserve(feed.end);
  feed.status.textContent = '正在读取行动…';
  try {
    const page = await api(`/api/conversations/${encodeURIComponent(feed.room)}/activity${feed.next ? '?before='+encodeURIComponent(feed.next) : ''}`);
    if (feed !== activityFeed || feed.room !== selected) return;
    for (const entry of page.entries) {
      const key = activityEntryKey(entry);
      if (feed.seen.has(key)) continue;
      feed.seen.add(key);
      feed.rows.insertAdjacentHTML('beforeend', activityRow(entry));
    }
    feed.loaded = true;
    feed.next = page.next;
    feed.status.textContent = feed.seen.size ? '' : '还没有行动记录。';
    if (feed.next && panelView === 'activity') feed.observer.observe(feed.end);
  } catch {
    if (feed !== activityFeed) return;
    feed.status.textContent = '暂时无法读取行动。向下滚动可再试一次。';
  } finally {
    feed.loading = false;
  }
}
// Refresh the visible head after a chat update without throwing away pages
// already loaded below it or replacing a row the user is about to open.
async function refreshActivityHead() {
  const feed = activityFeed;
  if (!feed?.loaded || feed.loading || feed.refreshing || panelView !== 'activity') return;
  feed.refreshing = true;
  try {
    const path = `/api/conversations/${encodeURIComponent(feed.room)}/activity`;
    let page = await api(path);
    const incoming = [...page.entries];
    // A burst of new actions can span pages. Read through to a known row so a
    // refresh cannot leave a gap between the new head and the retained pages.
    while (feed.seen.size && page.next && !page.entries.some(e=>feed.seen.has(activityEntryKey(e)))) {
      if (activityFeed !== feed || panelView !== 'activity') return;
      page = await api(path+'?before='+encodeURIComponent(page.next));
      incoming.push(...page.entries);
    }
    if (activityFeed !== feed || panelView !== 'activity') return;
    const rows = new Map([...feed.rows.children].map(row=>[row.dataset.id+':'+row.dataset.member, row]));
    const content = $('panel-content');
    const anchor = content.scrollTop > 0 && [...rows.values()].find(row=>row.getBoundingClientRect().bottom > content.getBoundingClientRect().top);
    const anchorTop = anchor?.getBoundingClientRect().top;
    let next = feed.rows.firstElementChild;
    const temp = document.createElement('template');
    for (const entry of incoming) {
      const key = activityEntryKey(entry);
      temp.innerHTML = activityRow(entry);
      let row = rows.get(key);
      if (!row) {
        row = temp.content.firstElementChild;
        feed.seen.add(key);
      } else if (row.innerHTML !== temp.content.firstElementChild.innerHTML) {
        row.innerHTML = temp.content.firstElementChild.innerHTML;
      }
      if (row === next) next = next.nextElementSibling;
      else feed.rows.insertBefore(row, next);
    }
    if (anchor) content.scrollTop += anchor.getBoundingClientRect().top - anchorTop;
    if (!rows.size && page.next) {
      feed.next = page.next;
      feed.observer.observe(feed.end);
    }
    feed.status.textContent = feed.seen.size ? '' : '还没有行动记录。';
  } catch { /* A refresh error leaves the currently readable list in place. */ }
  finally {
    feed.refreshing = false;
    if (feed === activityFeed && panelView === 'activity') activityScrolled();
  }
}
function activityScrolled() {
  const feed = activityFeed, content = $('panel-content');
  if (panelView === 'activity' && feed && content.scrollHeight - content.scrollTop - content.clientHeight < 120) void loadActivityPage(feed);
}

// Keep the list mounted behind its detail. A back action restores the same
// rows and scroll position, while closing the panel discards this session.
async function openActivityRecord(work, actor, invoker) {
  if (!activityFeed || panelView !== 'activity') return;
  const feed = activityFeed, content = $('panel-content');
  feed.top = content.scrollTop;
  feed.invoker = invoker;
  feed.overview.hidden = true;
  feed.observer.unobserve(feed.end);
  const detail = document.createElement('article');
  detail.className = 'activity-detail';
  detail.innerHTML = '<p class="activity-empty" role="status">正在读取…</p>';
  content.append(detail);
  panelView = 'activity-detail';
  $('panel-title').textContent = '行动详情';
  $('panel-back').hidden = false;
  $('panel-title').focus({preventScroll:true});
  content.scrollTop = 0;
  try {
    const d = await api(`/api/conversations/${encodeURIComponent(feed.room)}/activity/${encodeURIComponent(work)}?member=${encodeURIComponent(actor || '')}`);
    if (activityFeed !== feed || panelView !== 'activity-detail' || !detail.isConnected) return;
    const trace = (d.errors || []).map(e=>`<details class="technical-record"><summary>${escapeHTML(activityTime(e.at))} · 输入计数失败</summary><pre tabindex="0">${escapeHTML(e.output || '未保存服务商响应')}</pre></details>`).join('');
    const tools = (d.tools || []).map(t=>{
      const title = toolNamesForDetail[t.name] || t.name;
      return `<details class="technical-record"><summary>${escapeHTML(activityTime(t.created_at))} · ${escapeHTML(title)}</summary><pre tabindex="0">${escapeHTML(JSON.stringify({status:t.status,arguments:t.arguments},null,2))}</pre><pre tabindex="0">${escapeHTML(t.result)}</pre></details>`;
    }).join('');
    detail.innerHTML = `${activityMember(d.member_id)}<p class="detail-meta">${activityMeta(d)}</p><h3 class="detail-title${d.failed ? ' activity-failure' : ''}">${escapeHTML(d.title)}</h3>${activityFacts(d)}
      ${d.source ? `<section class="detail-part"><h4>${d.reply_reference ? '回复的消息' : '从这条消息开始'}</h4>${activityQuote(d.source)}${sourceLink(d.source.id)}</section>` : ''}
      ${d.messages?.length ? `<section class="detail-part"><h4>发出的消息</h4>${d.messages.map(activityQuote).join('')}</section>` : ''}
      ${activitySources(d.tools)}
      ${d.work || trace || tools ? `<details class="technical-record activity-technical"><summary>技术细节</summary>${trace}${tools}${d.work ? technicalJSON({work:d.work,failure_reason:d.failure_reason}) : ''}</details>` : ''}`;
  } catch (error) {
    if (activityFeed === feed && detail.isConnected) detail.textContent = error.message || '无法读取行动详情。';
  }
}
function backToActivity() {
  const feed = activityFeed;
  if (!feed || panelView !== 'activity-detail') return;
  const content = $('panel-content');
  content.querySelector('.activity-detail')?.remove();
  panelView = 'activity';
  $('panel-title').textContent = activityPanelName();
  $('panel-back').hidden = true;
  feed.overview.hidden = false;
  refreshActivityOverview();
  content.scrollTop = feed.top;
  (feed.invoker?.isConnected ? feed.invoker : $('panel-title')).focus({preventScroll:true});
  if (!feed.loaded || feed.next) feed.observer.observe(feed.end);
}
function activitySources(tools) {
  const items = [];
  for (const tool of tools || []) {
    if (tool.status !== 'succeeded') continue;
    if (tool.name === 'search' && tool.arguments?.query) {
      items.push(`<li>搜索 · ${escapeHTML(tool.arguments.query)}</li>`);
    }
    if (tool.name !== 'read_page' || !tool.arguments?.url) continue;
    try {
      const url = new URL(tool.arguments.url);
      if (url.protocol !== 'http:' && url.protocol !== 'https:') continue;
      items.push(`<li>阅读 · <a href="${escapeHTML(url.href)}" target="_blank" rel="noopener noreferrer">${escapeHTML(url.href)}</a></li>`);
    } catch { /* The original argument remains in the technical record. */ }
  }
  return items.length ? `<section class="detail-part"><h4>搜索和阅读</h4><ul class="activity-source-list">${[...new Set(items)].join('')}</ul></section>` : '';
}
const toolNamesForDetail = {search:'搜索网页',read_page:'阅读网页',send_message:'发送消息',ask_question:'提问',remember:'保存笔记',save_artifact:'保存作品',schedule_exploration:'安排后续',schedule_followup:'安排后续',cancel_work:'取消后续'};

async function showEvidence(id) {
  const room=selected;
  showPanel('行动详情','', '<p class="panel-description" role="status">正在读取…</p>',`evidence:${id}`);
  const request=panelRequest;
  const run=await api(`/api/tool-runs/${encodeURIComponent(id)}`);
  // 迟到的读取不能把用户刚打开的另一张浮层覆盖，也不能串到另一个聊天。
  if(request!==panelRequest || selected!==room)return;
  renderEvidence(run);
}
function renderEvidence(run) {
  const d=run.display || {};
  let result=run.result;
  try { result=JSON.parse(result); } catch { /* 历史原文仍可诊断。 */ }
  showPanel(d.title || '行动详情','',`<div class="activity-detail"><div class="activity-owner">${avatar(member(run.member_id),true)}<span><strong>${escapeHTML(authorName(run.member_id))}</strong><small>${dateLabel(run.created_at)} · ${timeLabel(run.created_at)}</small></span></div><p class="activity-outcome">${escapeHTML(d.status)}</p><p class="activity-description">${escapeHTML(d.detail)}</p><div class="activity-links">${sourceLink(d.source_id)}</div>${technicalRecord({status:run.status,arguments:run.arguments,result})}</div>`,`evidence:${run.id}`);
  $('panel-content').dataset.actionSignature=JSON.stringify(run.display);
}

// 工具执行中打开的详情会随 SSE 补上最终结果；保持展开项、滚动和焦点原位。
async function refreshEvidence(summary) {
  // 首次详情仍在读取时由 showEvidence 接管；SSE 不写入尚未形成的内容节点。
  if(!$('panel-content').querySelector('.activity-detail'))return;
  if($('panel-content').dataset.actionSignature===JSON.stringify(summary.display))return;
  const request=panelRequest, room=selected;
  const run=await api(`/api/tool-runs/${encodeURIComponent(summary.id)}`);
  if(request!==panelRequest || selected!==room || panelView!==`evidence:${run.id}`)return;
  const d=run.display || {};
  $('panel-content').querySelector('.activity-description').textContent=d.detail || '';
  $('panel-content').querySelector('.activity-outcome').textContent=d.status || '';
  let result=run.result;
  try { result=JSON.parse(result); } catch { /* 原文保留。 */ }
  $('panel-content').querySelector('.technical-record pre').textContent=JSON.stringify({status:run.status,arguments:run.arguments,result},null,2);
  $('panel-content').dataset.actionSignature=JSON.stringify(d);
}

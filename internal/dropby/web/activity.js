'use strict';

// 动态是已有记录的阅读入口，沿用浮动详情面，不把每次模型判断排成任务看板。
const openWorkStatuses = ['pending','running','waiting_budget','paused'];
const backgroundWork = w => ['research','revisit'].includes(w.kind) && !w.id.startsWith('coordination_');
const sourceLink = id => id ? `<button class="quiet activity-source" data-action="jump-message" data-id="${escapeHTML(id)}">${icon('message-circle')}回到相关交流</button>` : '';
function technicalRecord(value, key='technical') {
  return `<details class="technical-record" data-detail="${escapeHTML(key)}"><summary>技术详情</summary><pre>${escapeHTML(JSON.stringify(value,null,2))}</pre></details>`;
}

function renderActivity() {
  const active=(state.activities || []).filter(a=>a.conversation_id===selected);
  $('activity').hidden=!active.length;
  $('activity').innerHTML=active.map(a=>`<button data-action="activity"><span class="activity-dot"></span>${escapeHTML(authorName(a.member_id))} · ${escapeHTML(a.label)}</button>`).join('');
  const jobs=roomData.work || [];
  $('work-count').textContent=jobs.filter(w=>backgroundWork(w) && openWorkStatuses.includes(w.status)).length || '';
  // 普通聊天的局部判断失败留在成员近况。只有当前交流确实没有任何回应、
  // 或具体后台工作需要处理时，才占用输入框上方的一行提示。
  const latest=jobs[0];
  const noReply=latest && latest.kind==='chat' && latest.status==='failed' && !messages.some(m=>m.author_id!=='user' && m.author_id!=='system' && m.created_at>=latest.created_at);
  const unfinished=jobs.find(w=>backgroundWork(w) && ['failed','paused'].includes(w.status));
  const notice=noReply ? '这次回应未生成 · 查看详情' : unfinished ? `${authorName(unfinished.member_id)}的${unfinished.kind==='revisit'?'后续':'工作'}${unfinished.status==='paused'?'已暂停':'未完成'} · 查看详情` : '';
  $('work-notice').hidden=!notice;
  $('work-notice').textContent=notice;
}

function participationText(p) {
  if (!p) return '还没有新的近况';
  if (p.action==='error') {
    const w=roomData.work.find(w=>w.id===p.work_id);
    const spoke=w && messages.some(m=>m.author_id===p.member_id && m.created_at>=w.created_at && m.created_at<=p.created_at);
    return spoke ? '后续回应未生成' : '这次回应未生成';
  }
  if (p.action==='pass') return '这次没有补充';
  if (p.action==='revisit') return '想稍后再看看';
  if (p.action==='explore') return '想继续探索';
  return p.selected ? '最近参与了交流' : '有个想法，还没说出来';
}
function actionRow(run) {
  const d=run.display || {};
  return `<button class="activity-entry" data-action="evidence" data-id="${escapeHTML(run.id)}"><span class="activity-entry-copy"><strong>${escapeHTML(authorName(run.member_id))} · ${escapeHTML(d.title || '查看行动')}</strong>${d.detail ? `<span class="activity-entry-description">${escapeHTML(d.detail.split('\n')[0])}</span>`:''}<small>${escapeHTML(d.status || '查看记录')} · ${timeLabel(run.created_at)}</small></span>${icon('chevron-right')}</button>`;
}
function workEntry(w) {
  const d=w.display || {};
  return `<details class="activity-work" data-detail="${escapeHTML(w.id)}"><summary><span><strong>${escapeHTML(d.title || '后续安排')}</strong><small>${escapeHTML(d.status || '查看记录')}</small></span>${icon('chevron-down')}</summary><p>${escapeHTML(d.detail)}</p>${w.status==='pending' && w.kind==='revisit' ? `<p class="activity-time">${escapeHTML(new Date(w.due_at).toLocaleString('zh-CN'))} 再看看</p>`:''}<div class="activity-links">${sourceLink(d.source_id)}${['failed','paused'].includes(w.status) ? `<button class="quiet" data-action="continue-work" data-id="${escapeHTML(w.id)}">接着聊这件事 ${icon('arrow-up')}</button>`:''}</div>${technicalRecord({kind:w.kind,status:w.status,instruction:w.instruction,result:w.result},`technical-${w.id}`)}</details>`;
}
function showActivity(scroll=true) {
  if(!roomData)return;
  const content=$('panel-content'), expanded=[...content.querySelectorAll('details[open][data-detail]')].map(d=>d.dataset.detail), top=content.scrollTop;
  const decisions=new Map();
  for(const p of roomData.participation || []) if(!decisions.has(p.member_id))decisions.set(p.member_id,p);
  const live=(state.activities || []).filter(a=>a.conversation_id===selected);
  const people=roomData.conversation.member_ids.map(id=>{
    const p=decisions.get(id), focus=roomData.member_states?.find(s=>s.member_id===id)?.body, action=live.find(a=>a.member_id===id);
    const status=participationText(p), source=roomData.work.find(w=>w.id===p?.work_id)?.source_id;
    return `<div class="activity-person">${avatar(member(id),true)}<div><strong>${escapeHTML(authorName(id))}</strong><p>${escapeHTML(action?.label || (p?.action==='error' ? status : focus || status))}</p>${p?.action==='error' ? `<details class="participation-note" data-detail="person-${escapeHTML(id)}"><summary>查看这次回应</summary><p>这次没能生成新的回应，已经发出的消息仍然保留。</p><div class="activity-links">${sourceLink(source)}<button class="quiet" data-action="continue-member" data-id="${escapeHTML(id)}" data-source="${escapeHTML(source || '')}">再聊一句 ${icon('arrow-up')}</button></div>${technicalRecord({reason:p.reason,at:p.created_at},`reason-${id}`)}</details>`:''}</div></div>`;
  }).join('');
  const jobs=(roomData.work || []).filter(backgroundWork), current=jobs.filter(w=>openWorkStatuses.includes(w.status)||w.status==='failed');
  const actions=roomData.tool_runs || [];
  // 读记录、整理经历保留在第二层；用户先看到真正的安排、产出和收尾。
  const meaningful=actions.filter(a=>!['read_work','read_history','read_memory','read_tool_result','read_artifact','search_memory','remember'].includes(a.name));
  const other=actions.filter(a=>!meaningful.includes(a));
  const failedChat=roomData.work.find(w=>w.kind==='chat' && w.status==='failed' && w.id===roomData.work[0]?.id);
  const replyIssue=failedChat && ![...decisions.values()].some(p=>p.work_id===failedChat.id && p.action==='error') ? `<div class="reply-issue"><p>这次回应未生成，已有消息保留。</p><div class="activity-links">${sourceLink(failedChat.source_id)}<button class="quiet" data-action="continue-work" data-id="${escapeHTML(failedChat.id)}">接着聊 ${icon('arrow-up')}</button></div>${technicalRecord({reason:failedChat.result},'reply-error')}</div>` : '';
  const history=[...jobs.filter(w=>!current.includes(w)).slice(0,6).map(workEntry),...other.slice(0,12).map(actionRow)].join('');
  showPanel('朋友动态','',`<div class="activity-view"><h3 class="section-label">近况</h3>${people}${replyIssue}${current.length ? `<h3 class="section-label">后续安排</h3>${current.slice(0,12).map(workEntry).join('')}`:''}${meaningful.length ? `<h3 class="section-label">最近行动</h3>${meaningful.slice(0,12).map(actionRow).join('')}`:''}${history ? `<details class="activity-history" data-detail="history"><summary>查看其他记录 ${icon('chevron-down')}</summary>${history}</details>`:''}${!jobs.length && !meaningful.length ? '<p class="activity-empty">没有额外安排，接着聊就好。</p>':''}</div>`,'activity');
  if(!scroll) {
    for(const d of content.querySelectorAll('details[data-detail]'))d.open=expanded.includes(d.dataset.detail);
    content.scrollTop=top;
  }
}

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
  showPanel(d.title || '行动详情','',`<div class="activity-detail"><div class="activity-owner">${avatar(member(run.member_id),true)}<span><strong>${escapeHTML(authorName(run.member_id))}</strong><small>${dateLabel(run.created_at)} · ${timeLabel(run.created_at)}</small></span></div><p class="activity-outcome">${escapeHTML(d.status)}</p><p class="activity-description">${escapeHTML(d.detail)}</p><div class="activity-links">${sourceLink(d.source_id)}<button class="quiet" data-action="activity">全部动态 ${icon('chevron-right')}</button></div>${technicalRecord({status:run.status,arguments:run.arguments,result})}</div>`,`evidence:${run.id}`);
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

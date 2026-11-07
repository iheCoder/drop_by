'use strict';

// 账户、发起聊天和群编辑属于全局页面。它们与右侧阅读详情分开，
// SSE 继续更新聊天数据，但不会重建正在输入的资料或创建表单。
let workspaceView = '', workspaceInvoker = null, panelInvoker = null;
let groupSelection = [], groupDraft = {name:'',avatar:''};
function showWorkspace(title, description, body, view = '') {
  if (!$('workspace-page').hidden) {
    // 流程内的下一步沿用最初入口，取消后回到原聊天而不是已经移除的按钮。
  } else workspaceInvoker = document.activeElement;
  rememberDraft(); closePanel();
  workspaceView = view;
  $('workspace-title').textContent = title;
  $('workspace-content').innerHTML = `${description ? `<p class="workspace-description">${escapeHTML(description)}</p>` : ''}${body}`;
  $('workspace-page').hidden = false;
  document.querySelector('.app').classList.add('workspace-open');
  $('sidebar').classList.remove('open'); $('mobile-nav').setAttribute('aria-expanded','false');
  $('workspace-title').focus();
}
function closeWorkspace(restoreFocus = false) {
  if ($('workspace-page').hidden) return;
  $('workspace-page').hidden = true; workspaceView = ''; $('workspace-content').replaceChildren();
  document.querySelector('.app').classList.remove('workspace-open');
  avatarRequest++; avatarLoading = false;
  if (restoreFocus) {
    const target=workspaceInvoker?.isConnected && workspaceInvoker.getClientRects().length ? workspaceInvoker : $('message-input');
    target.focus();
  }
}
function renderMyProfile() {
  $('my-avatar').innerHTML=avatar(state.profile);
  $('my-name').textContent=state.profile?.name || '你';
}
function startChat() {
  showWorkspace('发起聊天', '找一位朋友聊聊，或者把大家聚到一起。', `<div class="start-actions"><button data-action="new-group">${icon('users')}<span><strong>新建群聊</strong><small>邀请几位朋友，一起聊点什么</small></span>${icon('chevron-right')}</button><button data-action="new-member">${icon('plus')}<span><strong>认识新朋友</strong><small>一个名字，一个喜欢的头像</small></span>${icon('chevron-right')}</button></div><h2 class="section-label">单独聊聊</h2>${state.members.map(m=>`<button class="person-button" data-action="direct" data-id="${m.id}">${avatar(m)}<span class="person-copy"><span class="person-name">${escapeHTML(m.name)}</span><span class="person-note">${escapeHTML(m.direction || '从一句随口的话开始')}</span></span>${icon('message-circle')}</button>`).join('') || '<p class="empty-note">先认识一位朋友吧。</p>'}`, 'start');
}
function memberChoices(ids) {
  return state.members.map(m=>`<label class="member-choice">${avatar(m)}<span class="copy"><strong>${escapeHTML(m.name)}</strong><small>${escapeHTML(m.direction || '边聊，边认识')}</small></span><input type="checkbox" name="members" value="${m.id}" ${ids.includes(m.id)?'checked':''} aria-label="选择 ${escapeHTML(m.name)}"></label>`).join('');
}
function startGroup(reset = true) {
  if (!state.members.length) { newMember(); return; }
  if (reset) { groupSelection=[]; groupDraft={name:'',avatar:''}; }
  showWorkspace('新建群聊', '先邀请朋友。之后还可以随时添加或移除成员。', `<form id="group-select-form"><div class="selection-heading"><span>选择朋友</span><span id="selected-count">已选 ${groupSelection.length} 位</span></div>${memberChoices(groupSelection)}<div class="form-error" hidden role="alert"></div><div class="workspace-footer"><button type="button" class="secondary" data-action="close-workspace">取消</button><button class="primary" type="submit">下一步 ${icon('chevron-right')}</button></div></form>`, 'group-select');
}
function groupPortraitEditor(c) {
  avatarRequest++; avatarLoading=false; avatarData=c.avatar || '';
  return `<div class="avatar-editor"><span id="group-avatar-preview">${groupAvatar(c)}</span><div class="avatar-edit-copy"><strong>群头像</strong><small>默认用大家的头像拼在一起</small><label class="avatar-upload">${icon('upload')}上传图片<input class="sr-only" id="avatar-file" type="file" accept="image/png,image/jpeg"></label><button type="button" class="quiet" data-action="reset-group-avatar">使用成员头像</button></div></div>`;
}
function groupDetailsForm() {
  showWorkspace('给群起个名字', '', `<form id="create-group-form">${groupPortraitEditor({...groupDraft,member_ids:groupSelection})}<label class="field"><span>群名 <small class="field-hint">可留空</small></span><input name="name" maxlength="80" value="${escapeHTML(groupDraft.name)}" placeholder="随便聊聊" autocomplete="off"></label><div class="invite-summary">${groupSelection.map(id=>avatar(member(id),true)).join('')}<span>${groupSelection.length} 位朋友</span><button class="quiet" type="button" data-action="group-back">重新选择</button></div><div class="form-error" hidden role="alert"></div><div class="workspace-footer"><button type="button" class="secondary" data-action="close-workspace">取消</button><button class="primary" type="submit">创建群聊 ${icon('users')}</button></div></form>`, 'group-details');
  $('workspace-content').querySelector('input[name=name]').focus();
}
function editGroup() {
  const c=roomData?.conversation; if (!c || c.kind!=='group') return;
  groupSelection=[...c.member_ids];
  showWorkspace('编辑群聊', '群成员的变化从保存时生效，过去的交流会保留。', `<form id="edit-group-form" data-id="${c.id}" data-revision="${c.revision}">${groupPortraitEditor(c)}<label class="field"><span>群名</span><input name="name" maxlength="80" required value="${escapeHTML(c.name)}"></label><div class="selection-heading"><span>群成员 <small>勾选添加，取消勾选移除</small></span><span id="selected-count">已选 ${groupSelection.length} 位</span></div>${memberChoices(c.member_ids)}<div class="form-error" hidden role="alert"></div><div class="workspace-footer"><button class="secondary" type="button" data-action="close-workspace">取消</button><button class="primary" type="submit">保存修改</button></div></form>`, 'group-edit');
}
function accountUsage() {
  const usage=state.usage || {}, total=Number(state.weekly_budget_micros || 0), used=Number(usage.cost_micros || 0);
  const dollars=value=>(value/1e6).toFixed(2);
  return `<div class="account-usage-heading"><h2>本周额度</h2><span>${state.budget_blocked?'暂时不足':'可用'}</span></div><p class="account-balance"><strong>$${dollars(Math.max(0,total-used))}</strong><span>剩余 / $${dollars(total)}</span></p><progress value="${Math.min(used,total)}" max="${total || 1}" aria-label="本周已使用额度"></progress><p class="account-usage-note">已使用约 $${dollars(used)}${budgetResetLabel()?` · ${escapeHTML(budgetResetLabel())} 重置`:''}</p><details class="usage-details"><summary>查看使用详情</summary>${weeklyBudgetNote()}<p>金额按请求用量估算，正在处理的请求会先预留额度，完成后结算。</p></details>`;
}
function updateAccountUsage() {
  const section=$('account-usage'); if(!section) return;
  const signature=JSON.stringify([state.usage,state.budget_blocked,state.weekly_budget_micros]);
  if(section.dataset.signature===signature) return;
  const open=section.querySelector('details')?.open; section.innerHTML=accountUsage(); section.dataset.signature=signature;
  section.querySelector('details').open=Boolean(open);
}
function account() {
  const p=state.profile || {name:'你'};
  showWorkspace('个人中心', '', `<div class="account-person">${avatar(p)}<div><h2>${escapeHTML(p.name)}</h2><p>以你喜欢的样子，出现在聊天里。</p></div><button class="secondary" data-action="edit-account">${icon('pencil')}编辑资料</button></div><section id="account-usage" class="account-usage">${accountUsage()}</section><div class="account-setting"><span>${icon('bell')}<strong>消息通知</strong></span><p>只提醒值得及时知道的消息，不打断每一次闲聊。</p><button class="secondary" data-action="notifications">${'Notification' in window && Notification.permission==='granted'?'通知已开启':'开启浏览器通知'}</button></div>`, 'account');
}
function editAccount() {
  showWorkspace('编辑我的资料', '', `<form id="account-form">${avatarEditor(state.profile)}<label class="field"><span>名字</span><input name="name" required maxlength="24" value="${escapeHTML(state.profile?.name || '你')}" autocomplete="nickname"></label><div class="form-error" hidden role="alert"></div><div class="workspace-footer"><button class="secondary" type="button" data-action="account">取消</button><button class="primary" type="submit">保存资料</button></div></form>`, 'account-edit');
}

// 资料保存使用表单提交，不自动把勾选或照片预览写入数据库。取消可完整撤回。
document.addEventListener('submit', async event=>{
  const form=event.target;
  if (!['group-select-form','create-group-form','edit-group-form','account-form'].includes(form.id)) return;
  event.preventDefault(); if (avatarLoading) return;
  const data=new FormData(form), reset=pending(form,'正在保存…');
  try {
    if (form.id==='group-select-form') {
      groupSelection=data.getAll('members');
      if (!groupSelection.length) throw new Error('先邀请至少一位朋友吧。');
      if (groupSelection.length>12) throw new Error('一个群可以邀请最多 12 位朋友。');
      groupDetailsForm(); return;
    }
    if (form.id==='account-form') {
      state.profile=await api('/api/profile',{method:'PATCH',body:JSON.stringify({name:data.get('name').trim(),avatar:avatarData,avatar_icon:avatarIcon,color:avatarColor})});
      renderMyProfile(); if (roomData) renderMessages(false); if (form.isConnected) account(); toast('资料已保存。'); return;
    }
    const editing=form.id==='edit-group-form';
    const ids=editing?data.getAll('members'):groupSelection;
    if (!ids.length) throw new Error('群里至少留一位朋友吧。');
    const payload={name:data.get('name'),avatar:avatarData,member_ids:ids};
    const c=editing ? await api(`/api/conversations/${form.dataset.id}`,{method:'PATCH',body:JSON.stringify({...payload,revision:Number(form.dataset.revision)})}) : await post('/api/conversations',{...payload,kind:'group'});
    if (!form.isConnected) { await refresh(); toast(editing?'群资料与成员已更新。':'群聊已创建，可以在聊天列表中打开。'); return; }
    closeWorkspace(); await refresh(); await openRoom(c.id); $('message-input').focus();
    if (editing) toast('群资料与成员已更新。');
  } catch(error) { if(form.isConnected) {formError(form,error);reset();} else toast(error.message); }
});
document.addEventListener('click',event=>{
  const menu=document.querySelector('.version-menu[open]'); if(menu && !menu.contains(event.target)) menu.open=false;
  const target=event.target.closest('[data-action]'); if (!target) return;
  switch(target.dataset.action) {
    case 'new-group': startGroup(); break;
    case 'new-member': newMember(); break;
    case 'edit-group': editGroup(); break;
    case 'account': account(); break;
    case 'edit-account': editAccount(); break;
    case 'notifications': requestNotifications(); break;
    case 'close-workspace': closeWorkspace(true); break;
    case 'reset-group-avatar': avatarRequest++; avatarData=''; avatarLoading=false; renderAvatarPreview(); target.closest('form').querySelector('button[type=submit]').disabled=false; break;
    case 'group-back': groupDraft={name:target.closest('form').elements.name.value,avatar:avatarData};startGroup(false);break;
    case 'artifact-version': renderArtifact(Number(target.dataset.index)); $('version-trigger')?.focus(); break;
    case 'expand-artifact': {
      const expanded=document.querySelector('.app').classList.toggle('artifact-expanded');
      target.setAttribute('aria-pressed',String(expanded)); target.setAttribute('aria-label',expanded?'还原预览宽度':'展开预览');
      break;
    }
  }
});
document.addEventListener('change',event=>{
  if(event.target.name!=='members') return;
  groupSelection=[...event.target.form.querySelectorAll('input[name=members]:checked')].map(el=>el.value);
  $('selected-count').textContent=`已选 ${groupSelection.length} 位`;
  if ($('group-avatar-preview')) renderAvatarPreview();
});
$('my-profile').onclick=account;

// 两份脚本的函数都就绪后再建立连接，避免首屏快响应早于全局页面初始化。
const stream = new EventSource('/api/events');
stream.addEventListener('ready', () => { $('connection-dot').className = 'ready'; $('connection-label').textContent = '已连接'; scheduleRefresh(); });
stream.addEventListener('change',scheduleRefresh);
stream.onerror = () => { $('connection-dot').className = ''; $('connection-label').textContent = '正在重连'; };
refresh();

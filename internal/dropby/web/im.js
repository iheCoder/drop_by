'use strict';

// 创建与编辑使用短暂的对话框，聊天保留在原位置。
// SSE 继续更新数据，但不重建正在填写的表单或改变当前选择。
let workspaceView = '', workspaceInvoker = null, panelInvoker = null;
let groupSelection = [];
function showWorkspace(title, description, body, view = '') {
  if (!$('workspace-page').hidden) {
    // 流程内的下一步沿用最初入口，取消后回到原聊天而不是已经移除的按钮。
  } else workspaceInvoker = $('person-popover').contains(document.activeElement) ? personAnchor : document.activeElement;
  closeTransient(); rememberDraft(); closePanel();
  workspaceView = view;
  $('workspace-title').textContent = title;
  $('workspace-content').innerHTML = `${description ? `<p class="workspace-description">${escapeHTML(description)}</p>` : ''}${body}`;
  $('workspace-page').hidden = false;
  if (!$('workspace-page').open) $('workspace-page').showModal();
  document.querySelector('.app').classList.add('workspace-open');
  $('sidebar').classList.remove('open'); $('mobile-nav').setAttribute('aria-expanded','false');
  $('workspace-title').focus();
}
function closeWorkspace(restoreFocus = false) {
  if ($('workspace-page').hidden) return;
  cancelAvatarCrop(); $('workspace-page').close();
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
// 创建入口只打开就近菜单；选择朋友一次即可创建，群名可以之后修改。
function startChat(trigger=document.activeElement) {
  const pop=$('start-popover');
  if (pop.matches(':popover-open')) { pop.hidePopover(); return; }
  closeTransient();
  pop.innerHTML=`<button data-action="new-group">${icon('users')}<span>新建群聊<small>邀请朋友，一起聊聊</small></span></button><button data-action="new-member">${icon('plus')}<span>认识新朋友<small>从名字和头像开始</small></span></button>`;
  startAnchor=trigger;pop.showPopover(); positionFloating(pop,trigger);
}
function memberPicker() {
  return `<label class="picker-search">${icon('search')}<input id="member-search" type="search" placeholder="搜索朋友" aria-label="搜索群成员" autocomplete="off"></label><div id="selected-people" class="selected-people" aria-label="已选朋友"></div><div id="member-options" class="member-options"></div><p id="picker-empty" class="empty-note" hidden>没有找到这位朋友，试试别的名字。</p><div id="picker-status" class="picker-status" role="status" aria-live="polite"></div>`;
}
function renderMemberPicker() {
  if (!$('member-options')) return;
  const query=$('member-search').value.trim().toLocaleLowerCase();
  const people=state.members.filter(m=>m.name.toLocaleLowerCase().includes(query));
  $('member-options').innerHTML=people.map(m=>`<label class="member-choice">${avatar(m)}<span class="copy"><strong>${escapeHTML(m.name)}</strong><small>${escapeHTML(memberIntroduction(m).split(/[，。；\n]/)[0].slice(0,28))}</small></span><input type="checkbox" name="members" value="${m.id}" ${groupSelection.includes(m.id)?'checked':''} aria-label="选择 ${escapeHTML(m.name)}"><span class="selection-check" aria-hidden="true">${icon('check')}</span></label>`).join('');
  $('picker-empty').hidden=Boolean(people.length);
  $('selected-people').innerHTML=groupSelection.map(id=>`<button type="button" class="selected-person" data-action="unselect-member" data-id="${id}" aria-label="移除已选 ${escapeHTML(authorName(id))}">${avatar(member(id),true)}<span>${escapeHTML(authorName(id))}</span>${icon('x')}</button>`).join('');
  const count=groupSelection.length;
  $('picker-status').textContent=count ? `已选 ${count} 位朋友` : '选择至少一位朋友，就可以开始聊了。';
  const create=$('create-group-button');
  if(create) { create.textContent=`创建群聊${count ? `（${count}）` : ''}`; create.disabled=count===0; }
}
function startGroup() {
  if (!state.members.length) { newMember(); return; }
  groupSelection=[]; avatarData='';
  showWorkspace('选择一起聊天的朋友', '', `<form id="create-group-form">${memberPicker()}<div class="form-error" hidden role="alert"></div><div class="workspace-footer"><button id="create-group-button" class="primary full-width" type="submit" disabled>创建群聊</button></div></form>`, 'group-select');
  renderMemberPicker(); $('member-search').focus();
}
function groupPortraitEditor(c) {
  avatarRequest++; avatarLoading=false; avatarData=c.avatar || '';
  return `<div class="avatar-editor"><button type="button" class="avatar-edit-preview" data-action="upload-avatar" aria-label="更换群头像"><span id="group-avatar-preview">${groupAvatar(c)}</span><span class="avatar-camera">${icon('image')}</span></button><div class="avatar-edit-copy"><strong>群头像</strong><small>也可以用大家的头像</small><button type="button" class="avatar-upload" data-action="upload-avatar">${icon('upload')}更换头像</button><button type="button" class="quiet" data-action="reset-group-avatar">使用成员头像</button></div><input hidden id="avatar-file" type="file" accept="image/png,image/jpeg"></div>`;
}
function editGroup() {
  const c=roomData?.conversation; if (!c || c.kind!=='group') return;
  groupSelection=[...c.member_ids];
  showWorkspace('编辑群聊', '', `<form id="edit-group-form" data-id="${c.id}" data-revision="${c.revision}"><details class="group-settings"><summary>群名与头像 ${icon('chevron-down')}</summary>${groupPortraitEditor(c)}<label class="field"><span>群名</span><input name="name" maxlength="80" required value="${escapeHTML(c.name)}"></label></details>${memberPicker()}<div class="form-error" hidden role="alert"></div><div class="workspace-footer"><button class="secondary" type="button" data-action="close-workspace">取消</button><button class="primary" type="submit">保存修改</button></div></form>`, 'group-edit');
  renderMemberPicker(); $('member-search').focus();
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
  if (!['create-group-form','edit-group-form','account-form'].includes(form.id)) return;
  event.preventDefault(); if (avatarLoading) return;
  const data=new FormData(form), reset=pending(form,'正在保存…');
  try {
    if (form.id==='account-form') {
      state.profile=await api('/api/profile',{method:'PATCH',body:JSON.stringify({name:data.get('name').trim(),avatar:avatarData,avatar_icon:avatarIcon,color:avatarColor})});
      renderMyProfile(); if (roomData) renderMessages(false); if (form.isConnected) account(); toast('资料已保存。'); return;
    }
    const editing=form.id==='edit-group-form';
    const ids=[...groupSelection];
    if (!ids.length) throw new Error('群里至少留一位朋友吧。');
    if (ids.length>12) throw new Error('一个群可以邀请最多 12 位朋友。');
    const name=editing ? data.get('name') : ids.map(authorName).slice(0,3).join('、');
    const payload={name,avatar:avatarData,member_ids:ids};
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
    case 'unselect-member': groupSelection=groupSelection.filter(id=>id!==target.dataset.id); renderMemberPicker(); $('member-search').focus(); break;
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
  const {value,checked}=event.target;
  if(checked && !groupSelection.includes(value)) {
    if(groupSelection.length>=12) { event.target.checked=false; $('picker-status').textContent='最多可以邀请 12 位朋友。'; return; }
    groupSelection.push(value);
  } else if(!checked) groupSelection=groupSelection.filter(id=>id!==value);
  renderMemberPicker();
  // 搜索过滤不改变已选成员；键盘勾选后把焦点还给原项。
  document.querySelector(`#member-options input[value="${CSS.escape(value)}"]`)?.focus({preventScroll:true});
  if ($('group-avatar-preview')) renderAvatarPreview();
});
document.addEventListener('input',event=>{ if(event.target.id==='member-search') renderMemberPicker(); });
$('my-profile').onclick=account;

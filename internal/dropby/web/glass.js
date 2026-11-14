'use strict';

// 短暂操作贴近对象。资料浮卡和创建菜单使用浏览器 popover 层，
// 不占用聊天布局，也不会被滚动容器裁掉。编辑和裁剪用原生 dialog 管理焦点。
let personAnchor = null, personID = '', cancelCrop = null, startAnchor = null;
function positionFloating(pop, anchor) {
  if (!pop.matches(':popover-open')) return;
  const margin=12, rect=anchor?.getBoundingClientRect();
  const width=pop.offsetWidth, height=pop.offsetHeight;
  let left=rect ? rect.left : (innerWidth-width)/2;
  let top=rect ? rect.bottom+10 : 90;
  if (rect && top+height>innerHeight-margin) top=rect.top-height-10;
  pop.style.left=`${Math.max(margin,Math.min(left,innerWidth-width-margin))}px`;
  pop.style.top=`${Math.max(margin,Math.min(top,innerHeight-height-margin))}px`;
}
function closeTransient(restoreFocus=false) {
  let closed=false;
  const focusTarget=$('message-menu').matches(':popover-open') ? messageMenuTrigger : $('reaction-popover').matches(':popover-open') ? reactionAnchor : $('start-popover').matches(':popover-open') ? startAnchor : $('emoji-popover').matches(':popover-open') ? document.querySelector('[data-action=emoji-menu]') : personAnchor;
  closed = closeMessageMenu();
  for (const id of ['person-popover','start-popover','emoji-popover','reaction-popover']) {
    const pop=$(id); if(pop?.matches(':popover-open')) { pop.hidePopover(); closed=true; }
  }
  if(closed && restoreFocus) (focusTarget?.isConnected ? focusTarget : $('message-input')).focus({preventScroll:true});
  return closed;
}
function profile(id, trigger=document.activeElement) {
  const m=member(id); if(!m) return;
  const pop=$('person-popover');
  if(pop.matches(':popover-open') && personID===id) { closeTransient(true); return; }
  closeWorkspace(false); closeTransient();
  personID=id; personAnchor=trigger;
  const status=memberIntroduction(m);
  pop.innerHTML=`<button class="icon-button floating-close" data-action="close-person" aria-label="关闭朋友资料">${icon('x')}</button><div class="person-card"><div class="person-portrait">${avatar(m)}</div><div class="person-card-copy"><h2>${escapeHTML(m.name)} <small>AI 朋友</small></h2><p class="person-introduction">${escapeHTML(status)}</p><div class="person-card-actions"><button class="primary" data-action="direct" data-id="${m.id}">${icon('message-circle')}发消息</button><button class="secondary" data-action="edit-profile" data-id="${m.id}">${icon('pencil')}编辑资料</button></div><button class="profile-more" data-action="full-profile" data-id="${m.id}">查看完整资料 ${icon('chevron-right')}</button></div></div>`;
  pop.showPopover(); positionFloating(pop,trigger);
  // 键盘触发者可以顺着 Tab 进入卡片；鼠标用户不被强制跳焦点。
  if(trigger?.matches(':focus-visible')) pop.querySelector('[data-action=direct]').focus();
}
// SSE 更新资料时只更新浮卡内容，不重新打开，也不抢走入口或表单的焦点。
function refreshPersonCard() {
  const pop=$('person-popover');
  if(!pop?.matches(':popover-open'))return;
  const m=member(personID); if(!m){closeTransient();return;}
  pop.querySelector('.person-introduction').textContent=memberIntroduction(m);
  pop.querySelector('.person-card h2').innerHTML=`${escapeHTML(m.name)} <small>AI 朋友</small>`;
  pop.querySelector('.person-portrait').innerHTML=avatar(m);
  if(!personAnchor?.isConnected) personAnchor=[...document.querySelectorAll(`[data-action="profile"][data-id="${CSS.escape(personID)}"]`)].find(el=>el.getClientRects().length);
  positionFloating(pop,personAnchor);
}
function cancelAvatarCrop() { if(cancelCrop) cancelCrop(); }
async function cropAvatar(file, request) {
  const bitmap=await createImageBitmap(file);
  if(request!==avatarRequest) { bitmap.close(); return null; }
  const dialog=$('crop-dialog');
  dialog.innerHTML=`<header class="crop-heading"><h2 id="crop-title">换个头像</h2><button type="button" class="icon-button" data-crop="cancel" aria-label="取消裁剪">${icon('x')}</button></header><div class="crop-stage"><canvas id="crop-canvas" width="640" height="640" tabindex="0" aria-label="拖动照片调整位置，也可以用方向键移动"></canvas><div class="crop-ring" aria-hidden="true"></div></div><p class="crop-hint">拖动调整位置 · 方向键也可以移动</p><label class="crop-zoom"><span>缩放</span><input id="crop-zoom" type="range" min="1" max="3" step="0.01" value="1" aria-label="头像缩放"></label><footer><button type="button" class="secondary" data-crop="cancel">取消</button><button type="button" class="primary" data-crop="use">使用头像</button></footer>`;
  const canvas=$('crop-canvas'), ctx=canvas.getContext('2d'), extent=320;
  let zoom=1, x=0, y=0, drag=null;
  const base=Math.max(extent/bitmap.width,extent/bitmap.height);
  function draw() {
    const w=bitmap.width*base*zoom,h=bitmap.height*base*zoom;
    x=Math.max(-(w-extent)/2,Math.min(x,(w-extent)/2));
    y=Math.max(-(h-extent)/2,Math.min(y,(h-extent)/2));
    ctx.setTransform(2,0,0,2,0,0); ctx.clearRect(0,0,extent,extent);
    ctx.drawImage(bitmap,(extent-w)/2+x,(extent-h)/2+y,w,h);
  }
  draw(); dialog.showModal();
  canvas.onpointerdown=e=>{ drag={x:e.clientX,y:e.clientY};canvas.setPointerCapture(e.pointerId); };
  canvas.onpointermove=e=>{ if(!drag)return;const scale=extent/canvas.clientWidth;x+=(e.clientX-drag.x)*scale;y+=(e.clientY-drag.y)*scale;drag={x:e.clientX,y:e.clientY};draw(); };
  canvas.onpointerup=canvas.onpointercancel=()=>{drag=null;};
  canvas.onkeydown=e=>{
    const delta={ArrowLeft:[-8,0],ArrowRight:[8,0],ArrowUp:[0,-8],ArrowDown:[0,8]}[e.key];
    if(delta){e.preventDefault();x+=delta[0];y+=delta[1];draw();}
  };
  $('crop-zoom').oninput=e=>{zoom=Number(e.target.value);draw();};
  return new Promise(resolve=>{
    let finished=false;
    function finish(value) {
      if(finished)return;finished=true;cancelCrop=null;bitmap.close();dialog.close();dialog.replaceChildren();resolve(value);
    }
    cancelCrop=()=>finish(null);
    dialog.oncancel=e=>{e.preventDefault();finish(null);};
    dialog.onclick=e=>{
      if(e.target===dialog) {
        const r=dialog.getBoundingClientRect();
        if(e.clientX<r.left || e.clientX>r.right || e.clientY<r.top || e.clientY>r.bottom) { finish(null); return; }
      }
      const action=e.target.closest('[data-crop]')?.dataset.crop;
      if(action==='cancel')finish(null);
      if(action==='use') {
        // API 只接收最终缩略图。完整图片和照片元数据始终留在本机。
        const output=document.createElement('canvas');output.width=128;output.height=128;
        output.getContext('2d').drawImage(canvas,0,0,128,128);
        let data=output.toDataURL('image/png');if(data.length>88000)data=output.toDataURL('image/jpeg',.85);
        finish(data);
      }
    };
  });
}

// 原生 dialog 的 Escape 与页面 Escape 共用同一关闭路径，防止留下 hidden/open 不一致。
// 详情面外按下再抬起才关闭；从面内拖选文字到外侧不能误关。
// 在 pointerdown 记录旧面板，避免开启面板的同一次点击把新面板关掉。
let outsidePanelPress = null;
document.addEventListener('pointerdown',e=>{
  outsidePanelPress = e.button===0 && !$('inspector').hidden && !$('inspector').contains(e.target)
    && $('workspace-page').hidden && !$('crop-dialog').open
    ? {request:panelRequest,pointer:e.pointerId} : null;
});
document.addEventListener('pointerup',e=>{
  const press=outsidePanelPress; outsidePanelPress=null;
  if(press && press.pointer===e.pointerId && press.request===panelRequest && !$('inspector').contains(e.target)) {
    // 不抢走用户正准备点击的输入框或按钮的焦点。
    const restore=$('inspector').contains(document.activeElement);
    closePanel(restore);
  }
});
document.addEventListener('pointercancel',()=>{outsidePanelPress=null;});
$('workspace-page').addEventListener('cancel',e=>{
  // 文件选择器的取消也会从 input 冒泡；它只结束选文件，编辑内容继续保留。
  if(e.target!==e.currentTarget)return;
  e.preventDefault();closeWorkspace(true);
});
$('workspace-page').addEventListener('click',e=>{
  if(e.target!==$('workspace-page'))return;
  const r=e.target.getBoundingClientRect();
  if(e.clientX<r.left || e.clientX>r.right || e.clientY<r.top || e.clientY>r.bottom)closeWorkspace(true);
});
document.addEventListener('click',e=>{
  const target=e.target.closest('[data-action]');if(!target)return;
  const action=target.dataset.action;
  if(action==='start-chat') startChat(target);
  if(action==='emoji-menu') {
    const pop=$('emoji-popover');if(pop.matches(':popover-open')){pop.hidePopover();return;}
    closeTransient();
    pop.innerHTML=[['😊','微笑'],['🙌','赞'],['❤️','喜欢'],['😂','大笑'],['🤔','思考'],['🎉','庆祝'],['☕','咖啡'],['🌊','海浪']].map(([face,label])=>`<button type="button" data-action="insert-emoji" data-emoji="${face}" aria-label="${label}">${face}</button>`).join('');
    pop.showPopover();positionFloating(pop,target);
  }
  if(action==='insert-emoji') {
    const input=$('message-input'),face=target.dataset.emoji;
    if(input.value.length-(input.selectionEnd-input.selectionStart)+face.length<=input.maxLength)input.setRangeText(face,input.selectionStart,input.selectionEnd,'end');
    closeTransient();input.focus();sizeComposer();rememberDraft();
  }
  if(action==='close-person')closeTransient(true);
  if(action==='full-profile')fullProfile(target.dataset.id);
  if(action==='rail-chats' || action==='rail-people') {
    closeTransient(); sideTab=action==='rail-chats'?'chats':'people';renderSidebar();
    if(innerWidth<=760) {$('sidebar').classList.add('open');$('mobile-nav').setAttribute('aria-expanded','true');}
  }
});

// 宽度改变时重算输入高度和浮卡位置；消息区保留浏览器的滚动锚定。
let resizeFrame=0;
const chatObserver=new ResizeObserver(()=>{
  cancelAnimationFrame(resizeFrame);resizeFrame=requestAnimationFrame(()=>{
    sizeComposer(); updateLatestButton();
    if($('person-popover').matches(':popover-open')) {
      if(!personAnchor?.isConnected) personAnchor=[...document.querySelectorAll(`[data-action="profile"][data-id="${CSS.escape(personID)}"]`)].find(el=>el.getClientRects().length);
      positionFloating($('person-popover'),personAnchor);
    }
    positionFloating($('start-popover'),startAnchor);
    positionFloating($('emoji-popover'),document.querySelector('[data-action=emoji-menu]'));
    positionReactionPopover();
    positionMessageMenu();
  });
});
chatObserver.observe($('main'));
window.addEventListener('resize',()=>{
  if(innerWidth>760){$('sidebar').classList.remove('open');$('mobile-nav').setAttribute('aria-expanded','false');}
});
// 跟随消息头像的浮卡在滚动时继续锚定；头像离开视口就收起，避免悬空。
$('chat-body').addEventListener('scroll',()=>{
  positionMessageMenu();
  positionReactionPopover();
  if(!$('person-popover').matches(':popover-open') || !personAnchor?.closest('#messages')) return;
  const a=personAnchor.getBoundingClientRect(), b=$('chat-body').getBoundingClientRect();
  if(a.bottom<b.top || a.top>b.bottom) closeTransient();
  else positionFloating($('person-popover'),personAnchor);
},{passive:true});
// 所有交互函数就绪后连接 SSE，快响应也不会早于浮层和裁剪初始化。
const stream = new EventSource('/api/events');
stream.addEventListener('ready', () => { $('connection-dot').className = 'ready'; $('connection-label').textContent = '已连接'; scheduleRefresh(); refreshReactions(); });
stream.addEventListener('change',scheduleRefresh);
stream.addEventListener('reactions',event=>refreshReactions(JSON.parse(event.data).conversation_id));
stream.onerror = () => { $('connection-dot').className = ''; $('connection-label').textContent = '正在重连'; };
syncMessages();
setInterval(() => syncMessages(true), messageCheckInterval);

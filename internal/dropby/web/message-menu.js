'use strict';

const messageMenu = $('message-menu');
let messageMenuTrigger = null, messageMenuPoint = null;

function closeMessageMenu() {
  const open = messageMenu.matches(':popover-open');
  if (open) messageMenu.hidePopover();
  messageMenuTrigger?.setAttribute('aria-expanded', 'false');
  messageMenuTrigger = messageMenuPoint = null;
  return open;
}

function positionMessageMenu() {
  if (!messageMenu.matches(':popover-open')) return;
  if (!messageMenuTrigger?.isConnected) { closeMessageMenu(); return; }
  const trigger = messageMenuTrigger.getBoundingClientRect();
  const bubble = messageMenuTrigger.closest('.message-content').querySelector('.message-bubble').getBoundingClientRect();
  const chat = $('chat-body').getBoundingClientRect();
  const anchor = messageMenuPoint ? bubble : trigger;
  if (anchor.bottom <= chat.top || anchor.top >= chat.bottom) { closeMessageMenu(); return; }
  const x = messageMenuPoint ? bubble.left + Math.min(messageMenuPoint.x, bubble.width) : trigger.left;
  const y = messageMenuPoint ? bubble.top + Math.min(messageMenuPoint.y, bubble.height) : trigger.bottom;
  const {offsetWidth: width, offsetHeight: height} = messageMenu;
  // 靠近右侧时让窄菜单也贴着点击点，避免浮条很宽时文字操作跑到另一头。
  const opensLeft = x + width - 8 > innerWidth;
  const left = opensLeft ? x - width + 8 : x - 8;
  const top = y + height > innerHeight && y > innerHeight / 2 ? y - height : y;
  messageMenu.classList.toggle('opens-left', opensLeft);
  messageMenu.style.left = `${Math.max(0, Math.min(left, innerWidth - width))}px`;
  messageMenu.style.top = `${Math.max(0, Math.min(top, innerHeight - height))}px`;
}

function openMessageMenu(trigger, point = null, keyboard = false) {
  const toggle = !point && messageMenu.matches(':popover-open') && messageMenuTrigger === trigger;
  closeTransient(toggle);
  if (toggle) return;
  const message = messages.find(m => m.id === trigger.dataset.id);
  if (!message) return;
  messageMenuTrigger = trigger;
  messageMenuPoint = point;

  const emoji = messageMenu.querySelector('.message-menu-emoji');
  emoji.innerHTML = (state.reaction_palette || []).map(face =>
    `<button type="button" data-action="reaction-add" data-id="${escapeHTML(message.id)}" data-emoji="${escapeHTML(face.emoji)}" aria-label="${escapeHTML(face.label)}" title="${escapeHTML(face.label)}">${escapeHTML(face.emoji)}</button>`
  ).join('');
  emoji.hidden = !emoji.children.length;
  const command = (action, id, glyph, label) => `<button type="button" data-action="${action}" data-id="${escapeHTML(id)}">${icon(glyph)}${label}</button>`;
  messageMenu.querySelector('.message-menu-actions').innerHTML =
    command('reply', message.id, 'reply', '回复') +
    command('copy-message', message.id, 'copy', '复制消息') +
    (message.evidence || []).map((id, i, all) => command('evidence', id, 'external-link', all.length > 1 ? `查看行动 ${i+1}` : '查看行动')).join('');
  // 单一原生浮层负责两块表面，点外部或按 Escape 都会同时收起。
  trigger.setAttribute('aria-expanded', 'true');
  messageMenu.showPopover({source:trigger});
  positionMessageMenu();
  if (!messageMenu.matches(':popover-open')) return;
  const focus = keyboard ? (emoji.querySelector('button') || messageMenu.querySelector('.message-menu-actions button')) : messageMenu;
  focus.focus({preventScroll:true});
}

const messageList = $('messages');
messageList.addEventListener('contextmenu', event => {
  const bubble = event.target.closest('.message-bubble');
  if (!bubble || event.defaultPrevented || event.target.closest('a, button, input, textarea, select, [contenteditable], img')) return;
  const selection = window.getSelection();
  // 已拖选的文字继续走浏览器菜单，包括跨越多个段落的选区。
  if (selection?.rangeCount && !selection.isCollapsed && selection.getRangeAt(0).intersectsNode(bubble)) return;
  const trigger = bubble.closest('.message-content').querySelector('.message-menu-trigger');
  if (!trigger) return;
  event.preventDefault();
  const rect = bubble.getBoundingClientRect();
  openMessageMenu(trigger, {x:event.clientX - rect.left, y:event.clientY - rect.top});
});
messageList.addEventListener('click', event => {
  const trigger = event.target.closest('.message-menu-trigger');
  if (!trigger) return;
  // popovertarget 让浏览器认出 ⋯ 属于这层浮窗，菜单内容和定位仍由这里准备。
  event.preventDefault();
  openMessageMenu(trigger, null, event.detail === 0);
});
messageList.addEventListener('keydown', event => {
  if (event.key !== 'ContextMenu' && !(event.shiftKey && event.key === 'F10')) return;
  const trigger = event.target.closest('.message-menu-trigger');
  if (!trigger) return;
  event.preventDefault();
  if (!messageMenu.matches(':popover-open')) openMessageMenu(trigger, null, true);
});

messageMenu.addEventListener('toggle', () => {
  if (!messageMenu.matches(':popover-open')) closeMessageMenu();
});
messageMenu.addEventListener('click', async event => {
  const action = event.target.closest('button')?.dataset.action;
  if (action === 'reply' || action === 'evidence') closeMessageMenu(); // 继续交给原有的回复和详情入口。
  if (action !== 'copy-message') return;
  const bubble = messageMenuTrigger.closest('.message-content').querySelector('.message-bubble');
  const body = bubble.querySelector('.markdown'), question = bubble.querySelector('.question-text');
  const value = [body?.innerText, question?.innerText].map(text => text?.trim()).filter(Boolean).join('\n\n');
  closeTransient(true);
  if (!value) { toast('这条消息没有可复制的正文。'); return; }
  try {
    await navigator.clipboard.writeText(value);
    toast('已复制消息。');
  } catch {
    if (!body?.isConnected) { toast('无法访问剪贴板，可选中文字后复制。'); return; }
    const selection = window.getSelection(), range = document.createRange();
    range.selectNodeContents(body);
    if (question) range.setEndAfter(question);
    selection.removeAllRanges();
    selection.addRange(range);
    toast('已选中正文，按 ⌘C 或 Ctrl+C 复制。');
  }
});

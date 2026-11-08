'use strict';

let reactionAnchor = null, reactionRoom = '', reactionRange = '', reactionRead = 0;
const reactionsByMessage = new Map();

function syncLoadedReactions() {
  if (reactionRoom !== selected) {
    reactionsByMessage.clear();
    reactionRoom = selected;
    reactionRange = '';
  }
  const range = messages.length ? `${messages[0].seq}:${messages.at(-1).seq}` : '';
  if (range !== reactionRange) {
    reactionRange = range;
    // 分页自带快照，可先显示新加入的历史消息；随后统一补读已加载范围，
    // 防止恰好发生在翻页或重连期间的回应被旧页面快照覆盖。
    for (const message of messages) {
      if (!reactionsByMessage.has(message.id)) reactionsByMessage.set(message.id, message.reactions || []);
    }
    refreshReactions();
  }
  renderReactionRows();
}

async function refreshReactions(room = selected) {
  if (room !== selected || room !== reactionRoom || !reactionRange) return;
  const read = ++reactionRead;
  const [from, through] = reactionRange.split(':');
  try {
    const groups = await api(`/api/conversations/${room}/reactions?from_seq=${from}&through_seq=${through}`);
    if (room !== selected || read !== reactionRead) return;
    for (const message of messages) reactionsByMessage.set(message.id, groups[message.id] || []);
    renderReactionRows();
  } catch {
    // 实时读取失败时保留已知快照，下一次连接就绪或更新事件会补读。
  }
}

function reactionLabel(emoji) {
  return state.reaction_palette?.find(face => face.emoji === emoji)?.label || emoji;
}

const reactionActorName = id => id === 'user' ? '我' : authorName(id);

function renderReactionRows() {
  for (const message of messages) {
    const article = document.getElementById(`message-${message.id}`);
    const row = article?.querySelector('.message-reactions');
    if (!row) continue;
    const markup = (reactionsByMessage.get(message.id) || []).map(({emoji, actor_ids: actors}) => {
      const mine = actors.includes('user');
      const names = actors.map(reactionActorName).join('、');
      return `<button class="reaction-chip${mine ? ' is-mine' : ''}" type="button" data-action="reaction-roster" data-id="${message.id}" data-emoji="${escapeHTML(emoji)}" aria-haspopup="dialog" aria-label="${escapeHTML(reactionLabel(emoji))}，${actors.length} 人回应${mine ? '，含我的回应' : ''}，查看回应者" title="${escapeHTML(names)}"><span aria-hidden="true">${escapeHTML(emoji)}</span><span aria-hidden="true">${actors.length}</span></button>`;
    }).join('');
    if (row.dataset.markup === markup) continue;
    const focus = row.contains(document.activeElement) ? document.activeElement.dataset.emoji : null;
    row.innerHTML = markup;
    row.dataset.markup = markup;
    row.hidden = !markup;
    if (focus) (findReactionChip(row, focus) || article.querySelector('.message-menu-trigger')).focus({preventScroll:true});
  }
  positionReactionPopover();
}

function findReactionChip(article, emoji) {
  return article?.querySelector(`[data-action=reaction-roster][data-emoji="${CSS.escape(emoji)}"]`);
}

function positionReactionPopover() {
  const pop = $('reaction-popover');
  if (!pop.matches(':popover-open')) return;
  const {id, emoji} = reactionAnchor.dataset;
  const article = document.getElementById(`message-${id}`);
  const trigger = article?.querySelector('.message-menu-trigger');
  // 其他浏览器或成员更新计数时，沿用原消息上的锚点；最后一个回应消失才收起名单。
  if (!reactionAnchor.isConnected) reactionAnchor = (emoji ? findReactionChip(article, emoji) : trigger) || null;
  if (!reactionAnchor) {
    pop.hidePopover();
    if (pop.contains(document.activeElement) || document.activeElement === document.body) trigger?.focus({preventScroll:true});
    return;
  }
  const anchor = reactionAnchor.getBoundingClientRect(), chat = $('chat-body').getBoundingClientRect();
  if (anchor.bottom < chat.top || anchor.top > chat.bottom) {
    pop.hidePopover();
    return;
  }
  if (emoji) renderReactionRoster(pop, id, emoji);
  positionFloating(pop, reactionAnchor);
}

function renderReactionRoster(pop, id, emoji) {
  const actors = (reactionsByMessage.get(id) || []).find(group => group.emoji === emoji)?.actor_ids || [];
  const names = actors.map(reactionActorName);
  const markup = `<strong class="reaction-roster-title">${escapeHTML(emoji)} ${actors.length} 人回应</strong><ul>${names.map(name => `<li>${escapeHTML(name)}</li>`).join('')}</ul>${actors.includes('user') ? `<button type="button" class="quiet" data-action="reaction-remove" data-id="${id}" data-emoji="${escapeHTML(emoji)}">撤回我的回应</button>` : ''}`;
  if (pop.dataset.markup === markup) return;
  const focused = pop.contains(document.activeElement);
  pop.innerHTML = markup;
  pop.dataset.markup = markup;
  if (focused) (pop.querySelector('button') || pop).focus({preventScroll:true});
}

function openReactionPopover(anchor) {
  const pop = $('reaction-popover');
  const alreadyOpen = pop.matches(':popover-open') && reactionAnchor === anchor;
  closeTransient(alreadyOpen);
  if (alreadyOpen) return;
  reactionAnchor = anchor;
  const {id, emoji} = anchor.dataset;
  pop.setAttribute('aria-label', '表情回应者');
  delete pop.dataset.markup;
  renderReactionRoster(pop, id, emoji);
  pop.showPopover({source:anchor});
  positionReactionPopover();
  (pop.querySelector('button') || pop).focus({preventScroll:true});
}

async function changeReaction(target, present) {
  const room = selected;
  const {id, emoji} = target.dataset;
  closeTransient(true);
  try {
    await api(`/api/conversations/${room}/messages/${id}/reaction`, {
      method:'PUT', body:JSON.stringify({emoji, present})
    });
    await refreshReactions(room);
  } catch (error) { toast(error.message); }
}

document.addEventListener('click', event => {
  const target = event.target.closest('[data-action^=reaction-]');
  if (!target) return;
  const action = target.dataset.action;
  if (action === 'reaction-roster') openReactionPopover(target);
  if (action === 'reaction-add' || action === 'reaction-remove') changeReaction(target, action === 'reaction-add');
});

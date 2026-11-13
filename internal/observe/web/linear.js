const M = window.Meter;
const ui = { work: "cheng_research", step: "r_compact", pane: "in", open: "", sec: 470 };
const list = document.getElementById("list");
const board = document.getElementById("board");
const detail = document.getElementById("detail");

function secFromBar(event, button) {
  const start = Number(button.dataset.seek);
  if (!button.classList.contains("bar")) return start;
  const bar = M.bars.find(item => item.work === button.dataset.work && item.a === start);
  if (!bar) return start;
  const rect = button.getBoundingClientRect();
  const ratio = rect.width ? Math.min(1, Math.max(0, (event.clientX - rect.left) / rect.width)) : 0;
  return Math.round(bar.a + (bar.b - bar.a) * ratio);
}

function renderList() {
  const items = M.works.map(work => {
    const t = M.totals(work);
    const cost = t.reserve && !t.settled ? "预留 " + M.money(t.reserve) : t.settled ? M.money(t.settled) : "没有费用";
    const bad = work.status === "未结束" || work.status === "等预算";
    const current = ui.work === work.id ? ` aria-current="true"` : "";
    return `<button type="button" class="item" data-work="${work.id}"${current}><span><b>${M.esc(work.who)} · ${M.esc(work.title)}</b><small>${M.esc(work.status)}</small></span><span class="num${bad ? " bad" : ""}">${M.esc(cost)}</span></button>`;
  }).join("");
  list.innerHTML = `<p class="list-label">行动 · ${M.works.length}</p>${items}`;
}

function renderBoard() {
  board.innerHTML = `<p class="kicker">随便聊聊 · 11 月 11 日 · 样例停在 21:28</p><h1>雨，和一块灰</h1><p class="dek">一条消息同时叫醒阿澄和老周。小满要等自己的活动结束才进来。研究到 21:28 还开着。</p><div class="board">${M.boardHTML(ui)}</div>`;
}

function renderDetail() {
  const work = M.workById(ui.work);
  detail.innerHTML = M.detailHTML(work, M.stepById(work, ui.step), ui);
}

function render() { renderList(); renderBoard(); renderDetail(); }

document.body.addEventListener("click", event => {
  const pane = event.target.closest("[data-pane]");
  const open = event.target.closest("[data-open]");
  const step = event.target.closest("[data-step]");
  const frame = event.target.closest("[data-frame]");
  const work = event.target.closest("[data-work]");
  const seek = event.target.closest("[data-seek]");
  if (pane) { ui.pane = pane.dataset.pane; renderDetail(); return; }
  if (open) { ui.open = ui.open === open.dataset.open ? "" : open.dataset.open; renderDetail(); return; }
  if (step) {
    if (step.dataset.work) ui.work = step.dataset.work;
    ui.step = step.dataset.step;
    ui.pane = "in";
    ui.open = "";
    const picked = M.stepById(M.workById(ui.work), ui.step);
    if (picked && picked.sec) ui.sec = picked.sec;
    render();
    return;
  }
  if (frame) { ui.sec = Number(frame.dataset.frame); M.syncFromTime(ui); render(); return; }
  if (seek && !work) { ui.sec = Number(seek.dataset.seek); M.syncFromTime(ui); render(); return; }
  if (work) {
    ui.work = work.dataset.work;
    if (work.dataset.seek) ui.sec = secFromBar(event, work);
    const w = M.workById(ui.work);
    ui.step = w.steps[0].id;
    if (work.dataset.seek) {
      const near = [...w.steps].reverse().find(item => item.sec <= ui.sec);
      if (near) ui.step = near.id;
    }
    ui.pane = "in";
    ui.open = "";
    render();
  }
});

document.body.addEventListener("input", event => {
  if (event.target.id !== "scrub") return;
  ui.sec = Number(event.target.value);
  document.querySelector(".sheet")?.style.setProperty("--at", String(ui.sec / M.HORIZON));
  const out = document.getElementById("readout");
  if (out) out.textContent = M.clock(ui.sec);
});

document.body.addEventListener("change", event => {
  if (event.target.id !== "scrub") return;
  M.syncFromTime(ui);
  render();
});

const pal = M.mountPalette((workId, stepId) => {
  ui.work = workId;
  ui.step = stepId;
  ui.pane = "in";
  ui.open = "";
  const step = M.stepById(M.workById(workId), stepId);
  if (step.sec) ui.sec = step.sec;
  render();
});

window.addEventListener("keydown", event => {
  if (pal.isOpen() || event.target.tagName === "INPUT") return;
  if (event.key !== "j" && event.key !== "k") return;
  const work = M.workById(ui.work);
  const index = work.steps.findIndex(step => step.id === ui.step);
  const next = work.steps[index + (event.key === "j" ? 1 : -1)];
  if (!next) return;
  ui.step = next.id;
  ui.pane = "in";
  ui.sec = next.sec || ui.sec;
  render();
});

render();

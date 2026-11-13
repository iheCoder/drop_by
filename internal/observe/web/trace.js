const M = window.Meter;
const ui = { work: "cheng_research", step: "r_compact", pane: "in", open: "", sec: 470 };
const stage = document.getElementById("stage");
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

function paintDetail() {
  detail.innerHTML = M.detailHTML(M.workById(ui.work), M.stepById(M.workById(ui.work), ui.step), ui);
}

function render() {
  const sum = M.sceneTotals();
  stage.innerHTML = `<div class="matter"><h1>雨，和一块灰</h1><p>已结算 ${M.money(sum.settled)} · 未回执 ${M.money(sum.reserve)}</p></div><p class="range">21:10 - 21:28 · 样例</p><div class="board">${M.boardHTML(ui)}</div>`;
  paintDetail();
}

document.body.addEventListener("click", event => {
  const pane = event.target.closest("[data-pane]");
  const open = event.target.closest("[data-open]");
  const step = event.target.closest("[data-step]");
  const frame = event.target.closest("[data-frame]");
  const work = event.target.closest("[data-work]");
  const seek = event.target.closest("[data-seek]");
  if (pane) { ui.pane = pane.dataset.pane; paintDetail(); return; }
  if (open) { ui.open = ui.open === open.dataset.open ? "" : open.dataset.open; paintDetail(); return; }
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
    const near = [...w.steps].reverse().find(item => item.sec <= ui.sec) || w.steps[0];
    ui.step = near.id;
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

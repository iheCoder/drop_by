const M = window.Meter;
const ui = { work: "cheng_research", step: "r_compact", pane: "in", open: "", sec: 470, filter: "all" };
const sum = M.sceneTotals();
document.getElementById("settled").textContent = M.money(sum.settled);
document.getElementById("hold").textContent = M.money(sum.reserve);
const filters = [["all", "全部"], ["unknown", "未知"], ["sol", "Sol"], ["terra", "Terra"], ["search", "搜索"]];

function match(work, step) {
  if (ui.filter === "unknown") return !!step.reserve;
  if (ui.filter === "sol") return step.model === "gpt-6.1-sol";
  if (ui.filter === "terra") return step.model === "gpt-6.1-terra";
  if (ui.filter === "search") return step.tool === "search" || step.tool === "read_page";
  return true;
}

function renderFilters() {
  document.getElementById("filters").innerHTML = filters.map(([id, label]) => `<button type="button" data-filter="${id}" aria-pressed="${ui.filter === id}">${label}</button>`).join("");
}

function renderSpark() {
  const n = 36;
  const counts = [];
  for (let i = 0; i < n; i++) {
    const t = (i + 0.5) * (M.HORIZON / n);
    counts.push(M.bars.filter(bar => t >= bar.a && t < bar.b && bar.tone !== "wait").length);
  }
  const max = Math.max(...counts, 1);
  document.getElementById("spark").innerHTML = counts.map((count, i) => {
    const t = i * (M.HORIZON / n);
    const on = ui.sec >= t && ui.sec < t + M.HORIZON / n;
    return `<button type="button" data-spark="${Math.round(t)}" class="${on ? "on" : ""}" style="height:${Math.max(8, count / max * 100)}%" aria-label="${M.clock(t)}"></button>`;
  }).join("");
}

function renderLog() {
  const rows = [];
  for (const work of M.works) {
    for (const step of work.steps) {
      if (match(work, step)) rows.push({ work, step });
    }
  }
  rows.sort((a, b) => a.step.sec - b.step.sec);
  const maxMs = Math.max(1, ...rows.map(row => row.step.ms || 0));
  const body = rows.map(({ work, step }) => {
    const on = ui.work === work.id && ui.step === step.id;
    const width = step.ms ? Math.max(6, step.ms / maxMs * 100) : 0;
    const sky = step.model === "gpt-6.1-sol" || step.kind === "compact";
    const current = on ? ` aria-current="true"` : "";
    const open = on ? `<div class="extra">${M.detailHTML(work, step, Object.assign({}, ui, { embed: true }))}</div>` : "";
    return `<div><button type="button" class="rec${sky ? " sky" : ""}" data-work="${work.id}" data-step="${step.id}"${current}><time class="num">${work.when || M.clock(step.sec)}</time><span class="who">${M.esc(work.who)}</span><span><b>${M.esc(step.title)}</b></span><span class="barline">${width ? `<i style="width:${width}%"></i>` : ""}</span><span class="num">${M.priceLabel(step)}</span></button>${open}</div>`;
  }).join("");
  document.getElementById("log").innerHTML = `<div class="cols"><span>时刻</span><span>谁</span><span>这一步</span><span>时长</span><span>金额</span></div>${body}`;
}

function render() { renderFilters(); renderSpark(); renderLog(); }

document.body.addEventListener("click", event => {
  const filter = event.target.closest("[data-filter]");
  const spark = event.target.closest("[data-spark]");
  const pane = event.target.closest("[data-pane]");
  const open = event.target.closest("[data-open]");
  const step = event.target.closest(".steplist [data-step]");
  const row = event.target.closest(".rec");
  if (filter) { ui.filter = filter.dataset.filter; render(); return; }
  if (spark) { ui.sec = Number(spark.dataset.spark); M.syncFromTime(ui); render(); return; }
  if (pane) { ui.pane = pane.dataset.pane; renderLog(); return; }
  if (open) { ui.open = ui.open === open.dataset.open ? "" : open.dataset.open; renderLog(); return; }
  if (step) { ui.step = step.dataset.step; ui.pane = "in"; ui.open = ""; renderLog(); return; }
  if (row) {
    const same = ui.work === row.dataset.work && ui.step === row.dataset.step;
    ui.work = row.dataset.work;
    ui.step = row.dataset.step;
    ui.pane = "in";
    ui.open = "";
    if (same) { ui.work = ""; ui.step = ""; }
    const picked = M.works.flatMap(work => work.steps.map(item => ({ work, item }))).find(row2 => row2.work.id === ui.work && row2.item.id === ui.step);
    if (picked) ui.sec = picked.item.sec || ui.sec;
    render();
  }
});

const pal = M.mountPalette((workId, stepId) => {
  ui.work = workId;
  ui.step = stepId;
  ui.pane = "in";
  ui.filter = "all";
  render();
  document.querySelector(".rec[aria-current='true']")?.scrollIntoView({ block: "center" });
});

window.addEventListener("keydown", event => {
  if (pal.isOpen() || event.target.tagName === "INPUT") return;
  if (event.key !== "j" && event.key !== "k") return;
  const work = M.workById(ui.work);
  if (!work) return;
  const index = work.steps.findIndex(step => step.id === ui.step);
  const next = work.steps[index + (event.key === "j" ? 1 : -1)];
  if (!next) return;
  ui.step = next.id;
  ui.pane = "in";
  render();
});

render();
document.querySelector(".rec[aria-current='true']")?.scrollIntoView({ block: "center" });

"use strict";

const runsEl = document.getElementById("runs");
const stepsEl = document.getElementById("steps");
const gapsEl = document.getElementById("gaps");
const snapEl = document.getElementById("snap");

function money(micros) {
  if (micros == null) return "";
  return "$" + (micros / 1e6).toFixed(6);
}
function costText(step) {
  if (step.cost_status === "settled") return "已结算 " + money(step.cost_micros);
  if (step.cost_status === "unknown") {
    return step.reserved_micros ? "费用未知，预留 " + money(step.reserved_micros) : "费用未知";
  }
  if (step.cost_status === "released") return "已确认没有产生费用";
  if (step.cost_status === "none") return "没有发出";
  return "";
}
function line(step) {
  const link = step.retry_of ? "重试自 " + step.retry_of.slice(0, 12) : (step.parent_id ? "属于 " + step.parent_id.slice(0, 12) : "");
  const call = step.call_id ? "调用 " + step.call_id.slice(0, 12) : "";
  return [step.kind, step.name, step.status, "第 " + step.attempt + " 次", costText(step), call, link, step.summary].filter(Boolean).join(" · ");
}

let stepsToken = 0;
let snapToken = 0;
let selectedRun = "";

async function loadRuns() {
  const res = await fetch("/api/runs");
  const body = await res.json();
  if (body.gaps && body.gaps.length) {
    gapsEl.hidden = false;
    gapsEl.textContent = body.gaps.map(gap => gap.note).join(" ");
  }
  runsEl.replaceChildren();
  if (!body.runs || !body.runs.length) {
    runsEl.textContent = body.source === "none" ? "还没有观测库。主服务写下记录后，这里会显示。" : "还没有行动记录。";
    return;
  }
  for (const run of body.runs) {
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.id = run.id;
    const title = document.createElement("b");
    title.textContent = (run.kind || "行动") + " · " + (run.status || "");
    const meta = document.createElement("small");
    meta.textContent = [run.member_id, run.trigger, "第 " + run.execution + " 次执行", run.work_id, run.parent_work_id ? "来自 " + run.parent_work_id : ""].filter(Boolean).join(" · ");
    button.append(title, meta);
    if (run.id === selectedRun) button.classList.add("on");
    button.addEventListener("click", () => {
      selectedRun = run.id;
      for (const item of runsEl.querySelectorAll("button")) item.classList.remove("on");
      button.classList.add("on");
      loadSteps(run.id);
    });
    runsEl.append(button);
  }
  if (body.runs.length >= 40) {
    const more = document.createElement("p");
    more.className = "muted";
    more.textContent = "这里只列出最近 40 次执行。";
    runsEl.append(more);
  }
}

async function loadSteps(id) {
  const token = ++stepsToken;
  snapToken++;
  snapEl.hidden = true;
  const res = await fetch("/api/runs/" + encodeURIComponent(id));
  if (token !== stepsToken) return;
  const body = await res.json();
  if (token !== stepsToken) return;
  stepsEl.replaceChildren();
  for (const step of body.steps || []) {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = line(step);
    button.addEventListener("click", () => loadSnap(step.id));
    stepsEl.append(button);
  }
  if (!stepsEl.childElementCount) stepsEl.textContent = "这次执行没有步骤。";
}

async function loadSnap(id) {
  const token = ++snapToken;
  const res = await fetch("/api/steps/" + encodeURIComponent(id));
  if (token !== snapToken) return;
  if (!res.ok) {
    snapEl.hidden = false;
    snapEl.textContent = "没有留下快照。";
    return;
  }
  const body = await res.json();
  if (token !== snapToken) return;
  snapEl.hidden = false;
  snapEl.replaceChildren();
  const input = document.createElement("pre");
  input.textContent = body.input || "";
  const output = document.createElement("pre");
  output.textContent = body.output || "";
  snapEl.append(input, output);
}

loadRuns();
setInterval(loadRuns, 5000);

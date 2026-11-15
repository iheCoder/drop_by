import { display } from "./studio-data.js?v=1";

const M = display;
let source;
let people = [];
let rooms = [];
const ui = {
  scope: "room",
  room: "",
  person: "",
  item: "",
  step: "",
  hideCache: false,
  open: "",
  folds: {},
  q: "",
  hit: 0,
  focus: "end",
  local: null,
  shown: 12,
  space: "browse",
  threadAnchor: null,
  threadTop: 0,
  chainTop: 0,
  bodyTop: 0,
  keepBody: false,
  hold: false,
  anchor: null
};

const TONE_NAME = {
  sol: "Sol",
  terra: "Terra",
  personal: "个人",
  tool: "工具",
  compact: "压缩",
  organize: "整理",
  capacity: "回读",
  quiet: "安静",
  wait: "等待",
  hold: "未回执"
};

function toneKey(step) {
  if (!step) return "terra";
  if (step.reserve) return "hold";
  if (step.kind === "compact") return "compact";
  if (step.kind === "tool") return "tool";
  if (step.kind === "organize") return "organize";
  if (step.kind === "capacity") return "capacity";
  if (step.kind === "quiet" || step.kind === "budget") return "quiet";
  if (step.model && step.model.includes("sol")) return "sol";
  if (step.snapshot && !step.model) return "wait";
  return "terra";
}

function roomById(id) { return rooms.find(room => room.id === id) || rooms[0]; }
function currentRoom() { return roomById(ui.room); }
function current() {
  if (ui.local) return ui.local;
  return { work: null, step: null };
}
function parseUsage(text) {
  if (!text || !text.includes("输入")) return null;
  const num = label => {
    const matched = text.match(new RegExp(label + "\\s*([\\d,]+)"));
    return matched ? Number(matched[1].replace(/,/g, "")) : 0;
  };
  return { input: num("输入"), cached: num("缓存读"), write: num("缓存写"), output: num("输出") };
}
function nanoMoney(nano) {
  return "$" + (nano / 1e9).toFixed(6);
}
function billOf(step) {
  const usage = step.usage || parseUsage(step.usageText);
  const rate = source.rates?.[step.model];
  if (!usage || !rate) return null;
  const normal = usage.input - usage.cached - usage.write;
  const rows = [
    ["未命中输入", normal, rate.input],
    ["缓存读", usage.cached, rate.cached],
    ["缓存写", usage.write, rate.write],
    ["输出", usage.output, rate.output]
  ].map(([name, tokens, unit]) => ({ name, tokens, nano: tokens * unit, unit }));
  return { usage, rate, rows, cachedFull: usage.cached * rate.input };
}
function markText(text) {
  const q = ui.q.trim();
  const safe = M.esc(text);
  if (!q) return safe;
  return safe.split(M.esc(q)).join("<mark>" + M.esc(q) + "</mark>");
}
function personById(id) { return people.find(person => person.id === id); }
function personKey(name) {
  if (name === "你") return "you";
  const found = people.find(person => person.name === name);
  return found ? found.id : "you";
}
function faceHTML(id, small) {
  if (id === "you") return `<span class="ava you${small ? " sm" : ""}" aria-hidden="true">你</span>`;
  const person = personById(id);
  const image = person?.avatar;
  const content = image
    ? `<img src="${M.esc(image)}" alt="">`
    : M.esc(person?.name?.slice(0, 1) || "·");
  return `<span class="ava${small ? " sm" : ""}">${content}</span>`;
}
function facesHTML(ids, large) {
  return `<span class="faces${large ? " lg" : ""}">${ids.map(id => faceHTML(id)).join("")}</span>`;
}
function stepOf(item) {
  if (!item) return null;
  if (item.stepRef) return source.workById(item.stepRef.work)?.steps.find(step => step.id === item.stepRef.step);
  return item.call || null;
}
function hasAction(item) {
  const step = stepOf(item);
  return !!(step && step.kind !== "said");
}
function currentItem() {
  return feed().find(entry => entry.id === ui.item) || null;
}
function stepStatus(step) {
  if (!step || step.kind === "said") return "没有单独的调用";
  if (step.statusLabel) return step.statusLabel;
  if (step.reserve) return "未回执";
  if (step.kind === "tool" && /超时|失败/.test((step.title || "") + (step.result || ""))) return "失败";
  if (step.kind === "quiet" || step.kind === "budget") return "未发送";
  const item = currentItem();
  if (item && item.call && String(item.where || "").includes("私聊")) return "已发到私聊";
  if (item && item.kind === "say" && item.call) return String(item.where || "").startsWith("群") ? "已发到群" : "已发到私聊";
  return "未发送";
}
function kickerFor(step) {
  if (step.kicker) return step.kicker;
  if (step.kind === "quiet" || step.kind === "budget") return "看过，没有说话";
  if (step.kind === "compact") return "压缩了窗口";
  if (step.kind === "organize") return "整理了自己的窗口";
  if (step.kind === "capacity") return "窗口放不下，换成回读";
  if (step.kind === "tool") return /超时|失败/.test((step.title || "") + (step.result || "")) ? "工具失败" : "用了工具";
  if (step.reserve) return "请求还没回来";
  if (step.output) return "写了，未发送";
  return "这次调用";
}
function snippet(step) {
  if (!step) return "";
  if (step.snapshot) return step.note;
  if (step.reserve) return "没有用量回执，预留还在。";
  if (step.kind === "tool" && /超时|失败/.test((step.title || "") + (step.result || ""))) return "没有拿到正文。费用仍按这一次记下。";
  if (step.kind === "tool" && step.result) return step.result.split("\n")[0];
  if (step.output) return step.output;
  if (step.body) return step.body;
  if (step.kind === "compact") return "拿回一份新窗口。";
  if (step.kind === "organize") return "便笺换掉了旧窗口，移出去的原文还在。";
  if (step.kind === "capacity") return "旧正文换成了可定位的回读入口。";
  return step.title || "";
}
function feed() {
  return ui.scope === "person" ? source.personFeed(personById(ui.person)) : source.roomFeed(currentRoom());
}
function itemSearch(item) {
  const step = stepOf(item);
  return [item.text || "", item.title || "", item.who || "", item.where || "", step ? snippet(step) : "", step ? step.title : ""].join(" ");
}
function feedHits() {
  const q = ui.q.trim().toLowerCase();
  if (!q) return [];
  return feed().filter(item => itemSearch(item).toLowerCase().includes(q));
}
function ancestorModel(steps, step) {
  const byId = new Map(steps.map(one => [one.id, one]));
  let cursor = step;
  const seen = new Set();
  while (cursor && !seen.has(cursor.id)) {
    if (cursor.kind === "model") return cursor;
    seen.add(cursor.id);
    cursor = byId.get(cursor.parent);
  }
  return null;
}
function roundSteps(steps, step) {
  if (!step || !steps?.length) return steps || [];
  if (!steps.some(one => one.parent || one.call)) return steps;
  const model = step.kind === "model" ? step : ancestorModel(steps, step);
  if (!model) return steps.filter(one => one.id === step.id);
  const ids = new Set([model.id]);
  if (model.call) {
    for (const one of steps) if (one.call && one.call === model.call) ids.add(one.id);
  }
  let grew = true;
  while (grew) {
    grew = false;
    for (const one of steps) {
      if (ids.has(one.id) || !one.parent || !ids.has(one.parent)) continue;
      ids.add(one.id);
      grew = true;
    }
  }
  const index = steps.findIndex(one => one.id === model.id);
  for (let i = index - 1; i >= 0; i--) {
    const one = steps[i];
    if (one.kind === "model" || (one.call && one.call !== model.call)) break;
    if (one.name === "input_tokens" || one.name === "input_tokens_local") ids.add(one.id);
    else break;
  }
  const round = steps.filter(one => ids.has(one.id));
  return round.length ? round : [step];
}
function focusedWork(work, step) {
  if (!work) return { work, step };
  const steps = roundSteps(work.steps, step);
  if (steps.length === (work.steps || []).length) return { work, step };
  return {
    work: {
      ...work, steps, title: "这一轮调用", status: "",
      scope: `同一次${work.kind || "执行"}里还有 ${work.steps.length - steps.length} 步，不属于这一轮。`
    },
    step
  };
}
function viewOf(item) {
  const step = stepOf(item);
  if (step && item.stepRef) {
    const work = source.workById(item.stepRef.work);
    return focusedWork(work, step);
  }
  if (step) {
    return {
      work: { who: item.who, kind: item.where || "私聊", title: item.where || "", status: "已写回", steps: [step] },
      step
    };
  }
  const steer = item.kind === "steer";
  return {
    work: { who: item.who, kind: item.where || "原话", title: item.where || "", status: "原话", steps: [] },
    step: {
      id: item.id,
      kind: "said",
      title: steer ? "真人补充" : (item.who === "你" ? "你写的" : "这条没有单独的模型记录"),
      text: item.text,
      body: steer ? "只进队列，不插进正在进行的调用。" : "没有单独的调用。"
    }
  };
}
function reveal(item) {
  const items = feed();
  const index = items.findIndex(entry => entry.id === item.id);
  if (index < 0) return;
  const need = items.length - index + 2;
  if (need > ui.shown) ui.shown = need;
}
function applyItem(item, focus) {
  ui.item = item.id;
  ui.focus = focus || "end";
  ui.local = viewOf(item);
  ui.step = ui.local.step.id;
  ui.chainTop = 0;
  ui.revealStep = true;
  ui.keepBody = false;
  reveal(item);
}
function sumFeed(items) {
  let settled = 0;
  let reserve = 0;
  const seen = new Set();
  for (const item of items) {
    const step = stepOf(item);
    if (!step || seen.has(step.id) || (step.snapshot && item.kind === "say")) continue;
    seen.add(step.id);
    if (step.reserve) reserve += step.reserve;
    else if (step.price) settled += step.price;
  }
  return { settled, reserve };
}

function prettyText(text) {
  const raw = String(text ?? "");
  const trimmed = raw.trim();
  if (!trimmed || (trimmed[0] !== "{" && trimmed[0] !== "[")) return { text: raw, json: false };
  try {
    return { text: JSON.stringify(JSON.parse(trimmed), null, 2), json: true };
  } catch {
    return { text: raw, json: false };
  }
}
function fold(id, text) {
  const shown = prettyText(text);
  const long = shown.text.length > 72;
  const open = !!ui.folds[id];
  const body = `<p class="${shown.json ? "json " : ""}${long && !open ? "clamp" : ""}">${M.esc(shown.text)}</p>`;
  const button = long ? `<button type="button" class="fold" data-fold="${M.esc(id)}">${open ? "收起" : "展开全文"}</button>` : "";
  return body + button;
}

function billHTML(step) {
  if (step.reserve) return "";
  if (step.kind === "tool") return `<p class="aside">搜索和读网页按次另计，每笔 $0.000750，不进模型的缓存账。</p>`;
  const bill = billOf(step);
  if (!bill) return "";
  const rows = bill.rows.map(row => `<tr class="${row.name === "缓存读" && row.tokens ? "cache" : ""}"><th>${row.name}</th><td>${row.tokens.toLocaleString("zh-CN")}</td><td>${nanoMoney(row.nano)}</td></tr>`).join("");
  const saved = bill.usage.cached
    ? `<p class="aside">这 ${bill.usage.cached.toLocaleString("zh-CN")} 个缓存读，如果按未命中输入计是 ${nanoMoney(bill.cachedFull)}，实际只计 ${nanoMoney(bill.usage.cached * bill.rate.cached)}。</p>`
    : `<p class="aside">这次没有缓存读。输入按未命中计。</p>`;
  return `<table class="bill"><tbody>${rows}<tr class="sum"><th>合计</th><td>${bill.usage.input.toLocaleString("zh-CN")} 进</td><td>${M.priceLabel(step)}</td></tr></tbody></table>${saved}`;
}

function divisionHTML(segments) {
  const list = segments.filter(seg => seg.tokens);
  const total = list.reduce((sum, seg) => sum + seg.tokens, 0) || 1;
  const bar = `<div class="divbar" aria-hidden="true">${list.map(seg => `<i class="${seg.cache ? "cached" : ""}" style="width:${(seg.tokens / total * 100).toFixed(2)}%"></i>`).join("")}</div>`;
  const names = list.map(seg => `<span>${M.esc(seg.k)} ${seg.tokens.toLocaleString("zh-CN")}${seg.cache ? " · 缓存读" : ""}</span>`).join("");
  return `${bar}<div class="divnames">${names}</div>`;
}

function blocksHTML(step, segments) {
  return segments.map((seg, index) => {
    const id = `${step.id}:seg:${index}`;
    return `<article class="block${seg.opaque ? " hatch" : ""} tone-${toneKey(step)}"><header><span class="pill">${M.esc(seg.k)}</span>${seg.cache ? `<span class="pill soft">缓存读</span>` : ""}${seg.tokens ? `<span class="count">约 ${seg.tokens.toLocaleString("zh-CN")}</span>` : ""}</header>${fold(id, seg.text)}</article>`;
  }).join("");
}

function costSummary(step) {
  if (step.reserve) return "预留 " + M.money(step.reserve) + "，还没有回执";
  if (step.costStatus === "unknown") return "金额待确认";
  if (!step.price) return M.priceLabel(step);
  const bill = billOf(step);
  const cached = bill?.usage.cached || step.snapshot?.data?.usage?.cached;
  const cache = cached ? " · 缓存读 " + cached.toLocaleString("zh-CN") : "";
  const time = step.ms ? M.dur(step.ms) + " · " : "";
  return time + M.priceLabel(step) + cache;
}
function usageHTML(usage) {
  const rows = [["输入", usage.input], ["缓存读", usage.cached], ["缓存写", usage.write], ["输出", usage.output]];
  return `<table class="bill"><tbody>${rows.map(([label, tokens]) => `<tr><th>${label}</th><td>${tokens.toLocaleString("zh-CN")} tokens</td></tr>`).join("")}</tbody></table>`;
}

function costBlock(step) {
  const id = "bill:" + step.id;
  const open = !!ui.folds[id];
  const usage = step.snapshot?.data?.usage;
  const can = !!usage || (!step.snapshot && step.kind === "tool") || (!step.reserve && !!billOf(step));
  const button = can ? `<button type="button" class="text-btn" data-fold="${M.esc(id)}" aria-expanded="${open ? "true" : "false"}">${open ? "收起费用明细" : "费用明细"}</button>` : "";
  const detail = open ? (usage ? usageHTML(usage) : billHTML(step)) : "";
  return `<div class="cost-line"><b>${M.esc(costSummary(step))}</b>${button}</div>${detail}`;
}

function modelLead(output) {
  let raw;
  try { raw = JSON.parse(output); } catch { return ""; }
  const texts = [];
  const calls = [];
  for (const cand of raw.candidates || []) {
    for (const part of cand?.content?.parts || []) {
      if (typeof part.text === "string" && part.text.trim()) texts.push(part.text);
      if (part.functionCall?.name) calls.push(part.functionCall.name);
    }
  }
  for (const item of raw.output || []) {
    if (item.type === "function_call" && item.name) calls.push(item.name);
    if (typeof item.content === "string" && item.content.trim()) texts.push(item.content);
    for (const block of Array.isArray(item.content) ? item.content : []) {
      if (typeof block?.text === "string" && block.text.trim()) texts.push(block.text);
    }
  }
  if (!texts.length && !calls.length) return "";
  const callNote = calls.length ? `<p class="aside">这一步还请求了 ${M.esc(calls.join("、"))}</p>` : "";
  const body = texts.map(text => `<article class="block"><p>${M.esc(text)}</p></article>`).join("");
  return `<h3>模型写出的内容</h3>${callNote}${body}`;
}
function inputSummary(input) {
  let raw;
  try { raw = JSON.parse(input); } catch { return ""; }
  const contents = raw.contents || raw.input;
  if (!Array.isArray(contents) || !contents.length) return "";
  const roles = contents.map(item => item.role || item.type || "内容");
  const shown = roles.slice(0, 8).join("、");
  return `<p class="aside">输入有 ${contents.length} 段：${M.esc(shown)}${roles.length > 8 ? "…" : ""}</p>`;
}
function sendHTML(step, data) {
  let args;
  try { args = JSON.parse(data.input || ""); } catch { return ""; }
  if (!args || typeof args !== "object" || Array.isArray(args) || typeof args.message !== "string") return "";
  const fields = Object.entries(args).filter(([key]) => key !== "message").map(([key, value]) => {
    const shown = typeof value === "string" ? value : JSON.stringify(value, null, 2);
    return `<div class="kv"><b>${M.esc(key)}</b><span>${M.esc(shown)}</span></div>`;
  }).join("");
  return `<h3>正文</h3><article class="block tone-${toneKey(step)}"><p>${M.esc(args.message)}</p></article>${fields ? `<h3>其他参数</h3><div class="fields">${fields}</div>` : ""}<h3>结果</h3><article class="block tone-${toneKey(step)}">${fold(step.id + ":result", data.output || "")}</article><h3>原始参数</h3><article class="block tone-${toneKey(step)}">${fold(step.id + ":raw", data.input || "")}</article>`;
}
function snapshotHTML(step) {
  const { data, state } = step.snapshot;
  if (state === "missing") return `<div class="empty-well">这一步没有保存输入或输出。</div>`;
  if (state === "error") return `<div class="empty-well">暂时读不到快照。<button type="button" data-retry-snapshot>重试</button></div>`;
  if (!data) return `<div class="empty-well" role="status">正在读取原始快照…</div>`;
  if (step.name === "send_message") {
    const sent = sendHTML(step, data);
    if (sent) return sent;
  }
  if (step.kind === "model" || step.kind === "compact") {
    const lead = modelLead(data.output || "");
    const summary = inputSummary(data.input || "");
    const output = data.output ? `<h3>原始输出</h3><article class="block tone-${toneKey(step)}">${fold(step.id + ":output", data.output)}</article>` : "";
    const input = data.input ? `<h3>原始输入</h3>${summary}<article class="block tone-${toneKey(step)}">${fold(step.id + ":input", data.input)}</article>` : "";
    return `${lead}${output}${input}` || `<div class="empty-well">这一步没有保存输入或输出。</div>`;
  }

  const entries = step.kind === "tool"
    ? [["结果", "output"], ["参数", "input"]]
    : [["原始输出", "output"], ["原始输入", "input"]];
  return entries.filter(([, field]) => data[field])
    .map(([label, field]) => `<h3>${label}</h3><article class="block tone-${toneKey(step)}">${fold(step.id + ":" + field, data[field])}</article>`)
    .join("") || `<div class="empty-well">这一步没有保存输入或输出。</div>`;
}

function loadSnapshot(step) {
  const snapshot = step?.snapshot;
  if (!snapshot || snapshot.state) return;
  snapshot.state = "loading";
  snapshot.load().then(data => {
    snapshot.data = data;
    snapshot.state = "ready";
  }).catch(error => {
    snapshot.state = error.status === 404 ? "missing" : "error";
  }).finally(() => {
    // A late snapshot only refreshes the open detail. Rebuilding the thread would
    // capture the reset scroll position and leave the conversation on an older page.
    if (ui.space === "focus" && current().step === step) paintDetail();
  });
}

function payloadHTML(work, step) {
  const tone = toneKey(step);
  if (step.snapshot) {
    return `${snapshotHTML(step)}${costBlock(step)}`;
  }
  if (step.kind === "model" || step.kind === "compact") {
    const all = step.segments || [];
    const cached = all.filter(seg => seg.cache).reduce((sum, seg) => sum + (seg.tokens || 0), 0);
    const shown = ui.hideCache ? all.filter(seg => !seg.cache) : all;
    const shrink = step.beforeTokens ? `<div class="shrink" aria-hidden="true"><div><span>送去 ${step.beforeTokens.toLocaleString("zh-CN")}</span><b><i style="width:100%"></i></b></div><div class="back"><span>拿回 ${step.afterTokens.toLocaleString("zh-CN")}</span><b><i style="width:${(step.afterTokens / step.beforeTokens * 100).toFixed(2)}%"></i></b></div></div>` : "";
    const before = step.beforeItems ? `<h3>送去压缩的划分</h3><p class="aside">这 ${step.beforeTokens.toLocaleString("zh-CN")} 是送进去的。供应商没有交回一份「保留了什么、丢掉了什么」的对照。</p><ul class="parts">${step.beforeItems.map(([k, n]) => `<li><span>${M.esc(k)}</span><b>${n.toLocaleString("zh-CN")}</b></li>`).join("")}</ul>${shrink}` : "";
    const inputTitle = step.kind === "compact" ? "压缩后拿回来的窗口" : "送进去的上下文";
    const inputAside = step.kind === "compact"
      ? "下面不是删节对照，是服务商交回的新窗口。每一块都能打开看原文。不可读的那块，这里本来就没有正文。"
      : "模型这次真正读到的内容，按块切开。宽度大致是各块所占的 token。标了缓存读的，没有按输入全价再计一次。";
    const hide = cached ? `<button type="button" class="chip${ui.hideCache ? " on" : ""}" data-hide-cache>${ui.hideCache ? "显示缓存读到的前缀" : "藏起缓存读到的前缀"}</button>` : "";
    const hiddenNote = ui.hideCache && cached ? `<p class="aside">藏起了缓存读的 ${cached.toLocaleString("zh-CN")} tokens。它们仍在实际输入里，也仍算在上面的金额里。</p>` : "";
    const inputBlock = shown.length
      ? `<h3>${inputTitle}</h3><p class="aside">${inputAside}</p>${divisionHTML(shown)}${hide}${hiddenNote}<div class="stack">${blocksHTML(step, shown)}</div>`
      : `<div class="empty-well">${step.reserve ? "请求已经离开本机。送出去的上下文没有留下来。" : "这次没有可展示的输入。"}</div>`;
    const reply = step.output
      ? fold(step.id + ":out", step.output)
      : "";
    const windowNote = step.beforeTokens && billOf(step) ? `<p class="aside">账单里的输出是这次生成的 ${billOf(step).usage.output.toLocaleString("zh-CN")} tokens。拿回来的窗口是 ${step.afterTokens.toLocaleString("zh-CN")}，两边不是同一个数。</p>` : "";
    const cost = costBlock(step);
    if (step.reserve) {
      return `<div class="empty-well">送出去的上下文没有留下来。</div>${cost}`;
    }
    if (step.kind === "compact") {
      return `${inputBlock}${before}${windowNote}${step.note ? `<p class="aside">${M.esc(step.note)}</p>` : ""}${cost}`;
    }
    const wrote = reply
      ? `<article class="block tone-${tone}">${reply}</article>`
      : "";
    return `${wrote}${step.note ? `<p class="aside">${M.esc(step.note)}</p>` : ""}${inputBlock}${cost}`;
  }
  if (step.kind === "said") {
    return `<p class="aside">${M.esc(step.body || "")}</p><article class="block tone-quiet"><p>${M.esc(step.text || "")}</p></article>`;
  }
  if (step.kind === "tool") {
    const failed = /超时|失败/.test((step.title || "") + (step.result || ""));
    const lead = failed
      ? `<p class="lead">${alertIcon()}这一步失败了。${M.esc(step.note || step.result || "没有拿到正文。")}</p>`
      : "";
    const extra = !failed && step.note ? `<p class="aside">${M.esc(step.note)}</p>` : "";
    return `${lead}<h3>结果</h3><article class="block tone-${tone}">${fold(step.id + ":result", step.result || "")}</article><h3>参数</h3><article class="block tone-${tone}">${fold(step.id + ":args", step.args || "")}</article>${extra}${costBlock(step)}`;
  }
  if (step.kind === "organize") {
    const col = items => items.map(([k, text], index) => `<article class="block tone-organize"><header><span class="pill">${M.esc(k)}</span></header>${fold(step.id + ":" + index + ":" + k, text)}</article>`).join("");
    const dropped = step.dropped.map(([id, k]) => `<button type="button" class="chip${ui.open === id ? " on" : ""}" data-open="${M.esc(id)}">${M.esc(k)}</button>`).join("");
    const opened = step.dropped.find(([id]) => id === ui.open);
    return `<p class="aside">她换掉了自己的窗口。便笺和点名留下的原文换掉旧窗口。移出去的内容还在。</p><div class="pair"><div><h3>整理前</h3>${col(step.before)}</div><div><h3>整理后</h3>${col(step.after)}</div></div><h3>移出窗口，点开仍是原文</h3><div class="chips">${dropped}</div>${opened ? `<article class="block tone-organize"><header><span class="pill">${M.esc(opened[1])}</span></header>${fold(opened[0], opened[2])}</article>` : ""}`;
  }
  if (step.kind === "capacity") {
    return `<p class="aside">压缩之后仍然放不下。旧正文换成可定位的回读入口。原文还在工具结果里。</p><div class="pair"><article class="block tone-capacity"><header><span class="pill">换之前</span></header><p>${M.esc(step.beforeText)}</p></article><article class="block tone-capacity"><header><span class="pill">送进下一次请求的</span></header><p>${M.esc(step.afterText)}</p></article></div><button type="button" class="chip" data-open="original">${ui.open === "original" ? "收起仍保存的原文" : "展开仍保存的原文"}</button>${ui.open === "original" ? `<article class="block tone-capacity">${fold("original", step.original)}</article>` : ""}`;
  }
  return `<article class="block tone-quiet">${fold(step.id + ":body", step.body || "")}</article>`;
}

function alertIcon() {
  return `<svg class="ico" aria-hidden="true" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"></circle><path d="M12 8v5"></path><path d="M12 16.5h.01"></path></svg>`;
}
function failedStep(step) {
  if (!step) return false;
  if (step.failed != null) return step.failed;
  if (step.reserve) return true;
  return step.kind === "tool" && /超时|失败/.test((step.title || "") + (step.result || ""));
}
function partsOf(at, raw) {
  const d = new Date(at);
  if (!Number.isFinite(d.getTime())) return null;
  const bag = {};
  for (const part of new Intl.DateTimeFormat("en-US", {
    timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23"
  }).formatToParts(d)) bag[part.type] = part.value;
  if (bag.hour === "24") bag.hour = "00";
  const matched = String(raw || "").match(/\.(\d+)/);
  bag.frac = (matched ? matched[1] : String(d.getMilliseconds()).padStart(3, "0")).padEnd(3, "0");
  return bag;
}
function sameClock(one, two, digits) {
  if (!one || !two) return false;
  const fields = ["year", "month", "day", "hour", "minute", "second"];
  if (fields.some(field => one[field] !== two[field])) return false;
  if (!digits) return true;
  return one.frac.slice(0, digits) === two.frac.slice(0, digits);
}
function clockText(entry, peers) {
  const p = partsOf(entry?.at, entry?.raw);
  if (!p) return "";
  const base = `${p.hour}:${p.minute}:${p.second}`;
  const parsed = (peers || []).map(one => partsOf(one?.at, one?.raw)).filter(Boolean);
  const sameSecond = parsed.filter(one => sameClock(one, p));
  if (sameSecond.length < 2) return base;
  const ms = p.frac.slice(0, 3);
  const sameMs = sameSecond.filter(one => one.frac.slice(0, 3) === ms);
  return sameMs.length < 2 ? `${base}.${ms}` : `${base}.${p.frac.slice(0, 6)}`;
}
function detailStamp(step) {
  const p = partsOf(step?.at, step?.raw);
  if (!p) return "";
  const fraction = /[1-9]/.test(p.frac.slice(3)) ? p.frac.slice(0, 6) : p.frac.slice(0, 3);
  return `${p.year}年${Number(p.month)}月${Number(p.day)}日 ${p.hour}:${p.minute}:${p.second}.${fraction}`;
}
function dayOf(item) {
  if (item?.raw) {
    const p = partsOf(item.at, item.raw);
    if (p) return `${p.year}年${Number(p.month)}月${Number(p.day)}日`;
  }
  return String(item?.time || "").replace(/\s+\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?$/, "");
}
function rowClock(item, peers) {
  if (item?.raw) return clockText(item, peers);
  const matched = String(item?.time || "").match(/\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?/);
  return matched ? matched[0] : "";
}
function stepClock(step, peers) {
  if (step?.at && step?.raw) return clockText(step, peers);
  if (step?.clock) return step.clock;
  if (step && step.sec != null && source.clock) return source.clock(step.sec).slice(0, 5);
  const item = feed().find(entry => entry.id === ui.item);
  return rowClock(item, feed());
}
function stepsHTML(work) {
  const max = Math.max(...work.steps.map(step => step.ms || 0), 1);
  return `<ol class="steps">${work.steps.map(step => {
    const on = step.id === ui.step ? " on" : "";
    const bad = failedStep(step) ? " bad" : "";
    const width = Math.max(4, Math.round((step.ms || 0) / max * 100));
    const mark = failedStep(step) ? alertIcon() : "";
    return `<li><button type="button" class="tone-${toneKey(step)}${on}${bad}" data-step="${M.esc(step.id)}"${on ? ` aria-current="step"` : ""}><i></i><time>${M.esc(stepClock(step, work.steps))}</time><span>${mark}<b>${M.esc(step.title)}</b><span class="meter" aria-hidden="true"><i style="width:${width}%"></i></span></span><em>${step.ms ? M.dur(step.ms) : (step.reserve ? "未回执" : M.dur(step.ms))} · ${M.priceLabel(step)}</em></button></li>`;
  }).join("")}</ol>`;
}
function jumpButtons(work) {
  const steps = work.steps || [];
  const failed = steps.find(failedStep);
  const priced = steps.filter(step => step.price);
  const costliest = priced.reduce((best, step) => step.price > best.price ? step : best, priced[0] || null);
  const slowest = steps.reduce((best, step) => (step.ms || 0) > ((best && best.ms) || 0) ? step : best, null);
  const jumps = [];
  if (failed) jumps.push(`<button type="button" class="bad" data-step="${M.esc(failed.id)}">${alertIcon()}失败的一步</button>`);
  if (priced.length > 1 && costliest) jumps.push(`<button type="button" data-step="${M.esc(costliest.id)}">最贵的一步</button>`);
  if (slowest && slowest.ms && (!failed || slowest.id !== failed.id) && (!costliest || slowest.id !== costliest.id)) jumps.push(`<button type="button" data-step="${M.esc(slowest.id)}">最慢的一步</button>`);
  return jumps;
}
function chainHead(work) {
  const jumps = jumpButtons(work);
  const when = (work.steps || []).find(step => step.raw);
  const date = when ? dayOf(when) : "";
  const note = work.scope ? `<p class="aside">${M.esc(work.scope)}</p>` : "";
  return `<div class="chain-head"><h3>${work.scope ? "这一轮调用" : "这次行动的调用"}</h3>${date ? `<p class="aside">${M.esc(date)}</p>` : ""}${note}${jumps.length ? `<div class="chain-jumps">${jumps.join("")}</div>` : ""}</div>`;
}
function pinHTML(work, step, spoken, tone) {
  const spent = (work.steps || []).reduce((sum, item) => sum + (item.ms || 0), 0);
  const money = spoken ? "没有单独的调用" : `${spent ? "调用合计 " + M.dur(spent) + " · " : ""}${M.costLine(work)}`;
  const fault = !spoken && (work.steps || []).find(item => item.reserve)
    ? "有一步还没有回执。预留还在，不能写成已经花掉。"
    : (!spoken && (work.steps || []).some(failedStep) ? "有一步失败了。" : "");
  const title = spoken ? step.title : work.title;
  const kicker = spoken ? `${work.who} · 原话` : `${work.who} · ${work.kind || ""}`;
  const outcome = !spoken && work.status ? `<p class="pin-outcome">${M.esc(work.status)}</p>` : "";
  return `<header class="pin pin-tight tone-${tone}"><div class="pin-row">${faceHTML(work.person || personKey(work.who), true)}<div class="pin-copy"><p class="kicker">${M.esc(kicker)}</p><h2>${M.esc(title)}</h2></div></div><p class="pin-meta">${M.esc(money)}</p>${outcome}${fault ? `<p class="fault">${alertIcon()}${M.esc(fault)}</p>` : ""}</header>`;
}
function stepHead(step) {
  if (!step || step.kind === "said") return "";
  const when = detailStamp(step);
  return `<header class="step-head"><h3>${M.esc(step.title)}</h3><p>${when ? `<time>${M.esc(when)}</time> · ` : ""}<b>${M.esc(costSummary(step))}</b> · ${M.esc(stepStatus(step))}</p></header>`;
}

function inspectHTML() {
  const { work, step } = current();
  if (!step) return `<div class="inspect-body"><p class="empty-well">选一条记录查看详情。</p></div>`;
  const spoken = step.kind === "said" || !(work.steps && work.steps.length);
  const tone = spoken ? "wait" : toneKey(step);
  const pin = pinHTML(work, step, spoken, tone);
  if (ui.space !== "focus" || spoken) {
    const text = spoken ? (step.text || "") : snippet(step);
    const state = spoken ? (step.body || "") : stepStatus(step);
    return `${pin}<div class="inspect-body"><section class="card summary"><p class="status">${M.esc(state)}</p><p>${M.esc(text)}</p></section></div>`;
  }
  loadSnapshot(step);
  const payload = `<section class="card payload tone-${toneKey(step)}">${stepHead(step)}${payloadHTML(work, step)}</section>`;
  if (work.steps.length > 1) {
    return `${pin}<div class="inspect-body workbench"><section class="card chain-card">${chainHead(work)}${stepsHTML(work)}</section>${payload}</div>`;
  }
  return `${pin}<div class="inspect-body">${payload}</div>`;
}

function sayHTML(item, grouped) {
  const you = item.person === "you";
  const step = stepOf(item);
  const on = item.id === ui.item;
  const hit = ui.q && itemSearch(item).toLowerCase().includes(ui.q.trim().toLowerCase());
  const chip = step ? `<span class="callchip tone-${toneKey(step)}">${M.esc(step.model && step.snapshot ? step.model : TONE_NAME[toneKey(step)])} · ${M.priceLabel(step)}</span>` : "";
  const steer = item.kind === "steer" ? `<em class="steer-note">只进队列，不插进正在进行的调用</em>` : "";
  const clock = rowClock(item, feed());
  const head = grouped ? "" : `<header><b>${M.esc(item.who)}</b><time>${M.esc(clock)}</time></header>`;
  const avatar = you || grouped ? "" : faceHTML(item.person);
  return `<article class="bubble ${you ? "you" : "them"}${grouped ? " grouped" : ""}${on ? " on" : ""}${hit ? " hit" : ""}${ui.focus === "hit" && on ? " focus" : ""}" id="item-${M.esc(item.id)}">${avatar}<div class="bubble-main">${head}<button type="button" class="bubble-hit" data-item="${M.esc(item.id)}"><span class="line">${markText(item.text || "")}</span>${steer}${chip}</button></div></article>`;
}
function eventHTML(item) {
  const step = stepOf(item);
  const on = item.id === ui.item;
  const hit = ui.q && itemSearch(item).toLowerCase().includes(ui.q.trim().toLowerCase());
  const tone = toneKey(step);
  const text = snippet(step);
  const clock = rowClock(item, feed());
  const label = ui.scope === "person"
    ? `<span class="where">${M.esc(item.where || "")}</span>`
    : `${M.esc(item.who)} · ${M.esc(kickerFor(step))}`;
  return `<article class="event tone-${tone}${on ? " on" : ""}${hit ? " hit" : ""}${ui.focus === "hit" && on ? " focus" : ""}" id="item-${M.esc(item.id)}"><button type="button" data-item="${M.esc(item.id)}">${faceHTML(item.person, true)}<span class="event-copy"><b>${label}</b><strong>${M.esc(step.title)}</strong><small>${markText(text)}</small></span><span class="event-side"><em>${M.esc(M.priceLabel(step))}</em><time>${M.esc(clock)}</time></span></button></article>`;
}
function threadBody(items, more) {
  const origin = (ui.scope === "room" && currentRoom()?.origin) || source.origin;
  let html = `<div class="spacer"></div>${more ? `<button type="button" class="earlier" data-earlier>往上还有 ${more} 条</button>` : (origin ? `<p class="origin">${M.esc(origin)}</p>` : "")}`;
  if (!items.length) html += `<p class="origin">${M.esc(source.emptyText || "这里还没有记录。")}</p>`;
  let day = "";
  let who = "";
  for (const item of items) {
    const nextDay = dayOf(item);
    if (nextDay !== day) {
      html += `<div class="day"><span>${M.esc(nextDay)}</span></div>`;
      day = nextDay;
      who = "";
    }
    if (item.kind === "event") {
      html += eventHTML(item);
      who = "";
    } else {
      const grouped = who === item.person && item.kind !== "steer";
      html += sayHTML(item, grouped);
      who = item.person;
    }
  }
  return html;
}
function railButton(selected, attrs, face, name, preview) {
  return `<button type="button" class="nav-row${selected ? " on" : ""}"${selected ? ` aria-current="true"` : ""} ${attrs}>${face}<span><b>${M.esc(name)}</b><small>${M.esc(preview)}</small></span></button>`;
}
function railHTML() {
  const personRows = people.map(person => {
    const last = source.personFeed(person).at(-1);
    const step = stepOf(last);
    const preview = step ? step.title + " · " + M.priceLabel(step) : person.note;
    return railButton(ui.scope === "person" && ui.person === person.id, `data-person="${M.esc(person.id)}"`, faceHTML(person.id), person.name, preview);
  }).join("");
  const roomRows = place => rooms.filter(room => room.place === place).map(room => {
    const last = source.roomFeed(room).at(-1);
    const preview = last ? (last.text || last.title || room.blurb) : room.blurb;
    const face = room.place === "群" ? facesHTML(room.members) : faceHTML(room.members[0]);
    return railButton(ui.scope === "room" && ui.room === room.id, `data-room="${M.esc(room.id)}"`, face, room.name, preview);
  }).join("");
  const sections = [["群", "群聊"], ["私聊", "私聊"], ["个人", "个人活动"], ["会话", "其他会话"]];
  const roomSections = sections.map(([place, label]) => {
    const rows = roomRows(place);
    return rows ? `<p class="rail-label">${label}</p>${rows}` : "";
  }).join("");
  return `<nav class="rail" aria-label="角色和聊天">${personRows ? `<p class="rail-label">角色</p>${personRows}` : ""}${roomSections}</nav>`;
}
function openRoom(room) {
  ui.scope = "room";
  ui.room = room.id;
  ui.q = "";
  ui.hit = 0;
  ui.focus = "end";
  ui.item = "";
  ui.local = null;
  ui.folds = {};
  ui.hideCache = false;
  ui.shown = 12;
  ui.chainTop = 0;
  ui.space = "browse";
}
function openPerson(id) {
  ui.scope = "person";
  ui.person = id;
  ui.q = "";
  ui.hit = 0;
  ui.focus = "end";
  ui.item = "";
  ui.local = null;
  ui.folds = {};
  ui.hideCache = false;
  ui.shown = 12;
  ui.chainTop = 0;
  ui.space = "browse";
}
function jumpHit(delta) {
  const hits = feedHits();
  if (!hits.length) return;
  if (ui.focus !== "hit") ui.hit = delta < 0 ? hits.length - 1 : 0;
  else ui.hit = (ui.hit + delta + hits.length) % hits.length;
  ui.folds = {};
  ui.hideCache = false;
  const item = hits[ui.hit];
  ui.space = hasAction(item) ? "focus" : "browse";
  applyItem(item, "hit");
  render();
}
function rememberInspect() {
  const chain = document.querySelector(".workbench .chain-card");
  if (chain) ui.chainTop = chain.scrollTop;
  const rail = document.querySelector(".step-rail");
  if (rail) ui.railLeft = rail.scrollLeft;
  if (!ui.keepBody) return;
  const detail = document.querySelector(".workbench .payload");
  const scroller = detail || document.querySelector(".inspect-body");
  if (scroller) ui.bodyTop = scroller.scrollTop;
}
function restoreInspect() {
  const workbench = document.querySelector(".workbench");
  if (workbench) {
    const chain = workbench.querySelector(".chain-card");
    const payload = workbench.querySelector(".payload");
    if (chain) chain.scrollTop = ui.chainTop || 0;
    if (payload) payload.scrollTop = ui.keepBody ? (ui.bodyTop || 0) : 0;
    if (ui.revealStep) revealChainStep();
    return;
  }
  const body = document.querySelector(".inspect-body");
  if (body) body.scrollTop = ui.keepBody ? (ui.bodyTop || 0) : 0;
  const rail = document.querySelector(".step-rail");
  if (rail && !ui.revealStep) rail.scrollLeft = ui.railLeft || 0;
}
function paintDetail() {
  const aside = document.querySelector("aside.inspect");
  if (!aside) {
    render();
    return;
  }
  rememberInspect();
  aside.innerHTML = inspectHTML();
  restoreInspect();
  ui.revealStep = false;
  ui.keepBody = false;
}
function updateDetail() {
  if (ui.space === "focus" && document.querySelector("aside.inspect")) paintDetail();
  else {
    if (ui.focus === "end") ui.focus = "stay";
    render();
  }
}
function revealRail() {
  const rail = document.querySelector(".step-rail");
  const current = rail && rail.querySelector('[aria-current="step"]');
  if (!rail || !current) return;
  const left = current.offsetLeft;
  const right = left + current.offsetWidth;
  if (left < rail.scrollLeft) rail.scrollLeft = Math.max(0, left - 8);
  else if (right > rail.scrollLeft + rail.clientWidth) rail.scrollLeft = right - rail.clientWidth + 8;
}
function revealChainStep() {
  const chain = document.querySelector(".chain-card");
  const current = chain && chain.querySelector('[aria-current="step"]');
  if (!chain || !current) return;
  const chainRect = chain.getBoundingClientRect();
  const stepRect = current.getBoundingClientRect();
  if (stepRect.top < chainRect.top) chain.scrollTop += stepRect.top - chainRect.top - 8;
  else if (stepRect.bottom > chainRect.bottom) chain.scrollTop += stepRect.bottom - chainRect.bottom + 8;
}
function render() {
  const oldThread = document.getElementById("thread");
  if (ui.focus === "stay" && oldThread) ui.threadTop = oldThread.scrollTop;
  rememberInspect();
  const items = feed();
  if (items.length && !items.some(item => item.id === ui.item)) applyItem(items[items.length - 1], ui.focus === "hit" ? "hit" : "end");
  const selected = items.find(item => item.id === ui.item);
  if (selected) reveal(selected);
  const visible = items.slice(Math.max(0, items.length - ui.shown));
  const more = items.length - visible.length;
  const sum = sumFeed(items);
  const hits = feedHits();
  const person = ui.scope === "person" ? personById(ui.person) : null;
  const room = currentRoom() || { name: "测光", place: "", members: [], blurb: "" };
  const title = person ? person.name : room.name;
  const place = person ? "角色" : room.place;
  const blurb = person ? "这个人在群、私聊和个人活动里的调用。从最近一次往上翻。" : room.blurb;
  const totals = sum.settled || sum.reserve
    ? `<div class="totals"><p><small>已结算</small><strong>${M.money(sum.settled)}</strong></p>${sum.reserve ? `<p class="hold"><small>未回执</small><strong>${M.money(sum.reserve)}</strong></p>` : ""}</div>`
    : "";
  const face = person ? faceHTML(person.id) : (room.place === "群" ? facesHTML(room.members, true) : faceHTML(room.members[0]));
  const live = room.featured && ui.scope === "room" ? `<div class="live">${people.filter(one => room.members.includes(one.id)).map(one => `<button type="button" data-person="${M.esc(one.id)}">${faceHTML(one.id, true)}<span>${M.esc(one.name)}</span><small>${M.esc(one.note)}</small></button>`).join("")}</div>` : "";
  const hint = person ? "往上翻更早的调用。" : "从最新一条开始。往上翻更早的。";
  const count = ui.q ? (ui.focus === "hit" && hits.length ? `${ui.hit + 1} / ${hits.length}` : `${hits.length} 处`) : "";
  const jumps = ui.q ? `<span class="hit-count">${count}</span><button type="button" data-jump="-1" aria-label="上一个结果">上一个</button><button type="button" data-jump="1" aria-label="下一个结果">下一个</button>` : "";
  document.body.classList.toggle("space-focus", ui.space === "focus");
  document.getElementById("app").innerHTML = `<a class="skip" href="#thread">跳到对话</a><header class="top"><a class="brand" href="/"><small>drop_by</small>测光</a><nav class="switch" aria-label="测光页面"><a href="/"${location.pathname === "/" ? ` aria-current="page"` : ""}>实际记录</a><a href="/demo.html"${location.pathname === "/demo.html" || location.pathname === "/spectrum.html" ? ` aria-current="page"` : ""}>样例</a><a href="/night.html"${document.body.dataset.skin === "night" ? ` aria-current="page"` : ""}>夜航</a><a href="/strata.html"${document.body.dataset.skin === "strata" ? ` aria-current="page"` : ""}>层叠</a></nav><button type="button" class="find" id="open-find"><span>查找</span><kbd>/</kbd></button></header><div class="body">${railHTML()}<main class="stage"><div class="stage-head">${face}<div class="stage-copy"><p class="kicker">${M.esc(place)}</p><h1>${M.esc(title)}</h1><p>${M.esc(blurb)}</p></div>${totals}</div>${source.notice ? `<p class="origin" role="status">${M.esc(source.notice)}</p>` : ""}${live}<div class="finder"><svg class="ico" aria-hidden="true"><use href="/icons.svg#search"/></svg><input id="thread-q" value="${M.esc(ui.q)}" placeholder="${person ? "在这个人的调用里找" : "在这段对话里找"}" aria-label="${person ? "在这个人的调用里找" : "在这段对话里找"}">${jumps}</div><p class="hint">${hint}</p><div id="thread">${threadBody(visible, more)}</div></main><aside class="inspect">${inspectHTML()}</aside></div>`;
  const box = document.getElementById("thread");
  pinThread();
  // Ignore the scroll event caused by restoring the thread, so it does not load older rows.
  ui.hold = true;
  if (box) box.addEventListener("scroll", onThreadScroll);
  requestAnimationFrame(() => { ui.hold = false; });
}
function pinThread() {
  const box = document.getElementById("thread");
  if (!box) return;
  ui.hold = true;
  if (ui.focus === "prepend" && ui.anchor) {
    box.scrollTop = ui.anchor.top + (box.scrollHeight - ui.anchor.before);
    ui.focus = "stay";
    ui.hold = false;
    revealRail();
    ui.revealStep = false;
    return;
  }
  if (ui.focus === "stay") {
    box.scrollTop = ui.threadTop || 0;
    if (ui.threadAnchor != null) {
      const selected = box.querySelector(".on");
      if (selected) {
        const delta = selected.getBoundingClientRect().top - box.getBoundingClientRect().top - ui.threadAnchor;
        box.scrollTop += delta;
      }
      ui.threadAnchor = null;
    }
    restoreInspect();
    ui.keepBody = false;
    ui.hold = false;
    revealRail();
    ui.revealStep = false;
    return;
  }
  if (ui.focus === "hit") {
    const target = box.querySelector(".focus");
    if (target) {
      const boxRect = box.getBoundingClientRect();
      const targetRect = target.getBoundingClientRect();
      if (boxRect.height && targetRect.height) {
        const next = box.scrollTop + (targetRect.top - boxRect.top) - (boxRect.height - targetRect.height) / 2;
        box.scrollTop = Math.max(0, next);
      }
    }
    ui.hold = false;
    revealRail();
    ui.revealStep = false;
    return;
  }
  box.scrollTop = box.scrollHeight;
  revealRail();
  ui.revealStep = false;
  ui.hold = false;
}
function onThreadScroll() {
  const box = document.getElementById("thread");
  if (!box || ui.hold) return;
  if (box.scrollTop < box.scrollHeight - box.clientHeight - 48) ui.focus = "stay";
  if (box.scrollHeight <= box.clientHeight + 8 || box.scrollTop > 28) return;
  if (ui.shown >= feed().length) return;
  ui.hold = true;
  ui.anchor = { before: box.scrollHeight, top: box.scrollTop };
  ui.shown = Math.min(feed().length, ui.shown + 12);
  ui.focus = "prepend";
  render();
}
window.addEventListener("resize", () => {
  if (ui.focus === "end") pinThread();
});

document.body.addEventListener("click", event => {
  const foldBtn = event.target.closest("[data-fold]");
  const hide = event.target.closest("[data-hide-cache]");
  const jump = event.target.closest("[data-jump]");
  const itemBtn = event.target.closest("[data-item]");
  const personBtn = event.target.closest("[data-person]");
  const roomBtn = event.target.closest("[data-room]");
  const open = event.target.closest("[data-open]");
  const step = event.target.closest("[data-step]");
  if (event.target.closest("[data-retry-snapshot]")) {
    current().step.snapshot.state = "";
    updateDetail();
    return;
  }
  if (foldBtn) {
    ui.folds[foldBtn.dataset.fold] = !ui.folds[foldBtn.dataset.fold];
    ui.keepBody = true;
    updateDetail();
    return;
  }
  if (hide) {
    ui.hideCache = !ui.hideCache;
    ui.keepBody = true;
    updateDetail();
    return;
  }
  if (jump) { jumpHit(Number(jump.dataset.jump)); return; }
  if (event.target.closest("[data-earlier]")) {
    const box = document.getElementById("thread");
    ui.hold = true;
    ui.anchor = { before: box ? box.scrollHeight : 0, top: box ? box.scrollTop : 0 };
    ui.shown = Math.min(feed().length, ui.shown + 12);
    ui.focus = "prepend";
    render();
    return;
  }
  if (itemBtn) {
    const item = feed().find(entry => entry.id === itemBtn.dataset.item);
    if (!item) return;
    const next = hasAction(item) ? "focus" : "browse";
    if (ui.space !== next) {
      const thread = document.getElementById("thread");
      const row = itemBtn.closest("article");
      if (thread && row) ui.threadAnchor = row.getBoundingClientRect().top - thread.getBoundingClientRect().top;
      if (ui.focus === "end") ui.focus = "stay";
    }
    ui.space = next;
    ui.folds = {};
    ui.hideCache = false;
    applyItem(item, "stay");
    render();
    return;
  }
  if (personBtn) {
    if (ui.scope === "person" && ui.person === personBtn.dataset.person) {
      if (ui.space !== "browse") { captureThread(); ui.space = "browse"; ui.keepBody = true; render(); }
      return;
    }
    openPerson(personBtn.dataset.person);
    render();
    return;
  }
  if (roomBtn) {
    if (ui.scope === "room" && ui.room === roomBtn.dataset.room) {
      if (ui.space !== "browse") { captureThread(); ui.space = "browse"; ui.keepBody = true; render(); }
      return;
    }
    openRoom(roomById(roomBtn.dataset.room));
    render();
    return;
  }
  if (open) {
    ui.open = ui.open === open.dataset.open ? "" : open.dataset.open;
    ui.keepBody = true;
    updateDetail();
    return;
  }
  if (step && ui.local && ui.local.work && ui.local.work.steps) {
    const picked = ui.local.work.steps.find(entry => entry.id === step.dataset.step);
    if (!picked || picked.id === ui.step) return;
    const opened = ui.space === "focus";
    ui.space = "focus";
    ui.folds = {};
    ui.hideCache = false;
    ui.keepBody = false;
    ui.local = { work: ui.local.work, step: picked };
    ui.step = picked.id;
    ui.revealStep = true;
    if (opened) paintDetail();
    else {
      captureThread();
      if (ui.focus === "end") ui.focus = "stay";
      render();
    }
    return;
  }
  const rail = event.target.closest(".rail");
  if (rail && ui.space === "focus" && !event.target.closest("button")) {
    captureThread();
    ui.space = "browse";
    ui.keepBody = true;
    render();
    return;
  }
  const stage = event.target.closest(".stage");
  if (stage && ui.space === "focus" && !event.target.closest("input, textarea, button, a")) {
    captureThread();
    ui.space = "browse";
    ui.keepBody = true;
    render();
  }
});

document.body.addEventListener("input", event => {
  if (event.target.id === "thread-q") {
    const pos = event.target.selectionStart;
    ui.q = event.target.value;
    ui.hit = 0;
    ui.focus = "stay";
    ui.keepBody = true;
    render();
    const input = document.getElementById("thread-q");
    if (input) {
      input.focus();
      input.setSelectionRange(pos, pos);
    }
    return;
  }
});

document.body.addEventListener("keydown", event => {
  if (event.target.id === "thread-q" && event.key === "Enter") {
    event.preventDefault();
    jumpHit(1);
  }
});

const pal = document.createElement("div");
pal.className = "pal";
pal.hidden = true;
pal.innerHTML = `<div class="pal-box" role="dialog" aria-label="查找"><input id="pal-q" placeholder="找一个角色、群、私聊，或一句话" aria-label="查找"><ul id="pal-list"></ul></div>`;
document.body.appendChild(pal);
const palInput = pal.querySelector("#pal-q");
const palList = pal.querySelector("#pal-list");
let palHi = 0;
let palShown = [];

function palRows() {
  const q = palInput.value.trim().toLowerCase();
  const personRows = people.filter(person => !q || person.name.includes(q)).map(person => ({ type: "person", person }));
  const roomRows = rooms.filter(room => !q || (room.place + room.name + room.blurb).toLowerCase().includes(q)).map(room => ({ type: "room", room }));
  const hitRows = [];
  if (q) {
    for (const room of rooms) {
      for (const item of source.roomFeed(room)) {
        if (itemSearch(item).toLowerCase().includes(q)) hitRows.push({ type: "hit", room, item });
      }
    }
  }
  return [...personRows, ...roomRows, ...hitRows.slice(0, 8)];
}
function pickRow(row) {
  ui.folds = {};
  ui.hideCache = false;
  const q = palInput.value.trim();
  if (row.type === "person") openPerson(row.person.id);
  else if (row.type === "room") openRoom(row.room);
  else {
    openRoom(row.room);
    ui.q = q;
    ui.space = hasAction(row.item) ? "focus" : "browse";
    applyItem(row.item, "hit");
    ui.hit = feedHits().findIndex(item => item.id === row.item.id);
  }
  palClose();
  render();
}
function palPaint() {
  palShown = palRows();
  palHi = Math.max(0, Math.min(palHi, palShown.length - 1));
  let last = "";
  const html = palShown.map((row, i) => {
    const label = row.type === "person" ? "角色" : row.type === "room" ? "聊天" : "对话里";
    const head = row.type === last ? "" : `<li class="label">${label}</li>`;
    last = row.type;
    if (row.type === "person") return `${head}<li><button type="button" class="${i === palHi ? "on" : ""}" data-i="${i}">${faceHTML(row.person.id, true)}<span><b>${M.esc(row.person.name)}</b><small>${M.esc(row.person.note)}</small></span></button></li>`;
    if (row.type === "room") return `${head}<li><button type="button" class="${i === palHi ? "on" : ""}" data-i="${i}">${row.room.place === "群" ? facesHTML(row.room.members) : faceHTML(row.room.members[0], true)}<span><b>${M.esc(row.room.place)} · ${M.esc(row.room.name)}</b><small>${M.esc(row.room.blurb)}</small></span></button></li>`;
    const text = (row.item.text || row.item.title || "").slice(0, 28);
    return `${head}<li><button type="button" class="${i === palHi ? "on" : ""}" data-i="${i}">${faceHTML(row.item.person, true)}<span><b>${M.esc(row.room.name)} · ${M.esc(text)}</b><small>${M.esc(row.item.time)}</small></span></button></li>`;
  }).join("");
  palList.innerHTML = html || `<li class="empty">没有这项</li>`;
}
function palOpen() {
  pal.hidden = false;
  palInput.value = "";
  palHi = 0;
  palPaint();
  palInput.focus();
}
function palClose() { pal.hidden = true; }
palInput.addEventListener("input", () => { palHi = 0; palPaint(); });
palInput.addEventListener("keydown", event => {
  if (event.key === "ArrowDown") { palHi += 1; palPaint(); event.preventDefault(); }
  else if (event.key === "ArrowUp") { palHi -= 1; palPaint(); event.preventDefault(); }
  else if (event.key === "Enter" && palShown[palHi]) pickRow(palShown[palHi]);
  else if (event.key === "Escape") { palClose(); event.stopPropagation(); }
});
palList.addEventListener("click", event => {
  const button = event.target.closest("[data-i]");
  if (!button) return;
  const row = palShown[Number(button.dataset.i)];
  if (row) pickRow(row);
});
pal.addEventListener("mousedown", event => { if (event.target === pal) palClose(); });

function captureThread() {
  const thread = document.getElementById("thread");
  const selected = thread && thread.querySelector(".on");
  if (thread && selected) ui.threadAnchor = selected.getBoundingClientRect().top - thread.getBoundingClientRect().top;
  if (ui.focus === "end") ui.focus = "stay";
}
function leaveOneLevel() {
  if (ui.space !== "focus") return;
  captureThread();
  ui.space = "browse";
  ui.keepBody = true;
  render();
}
function keyTarget(event) {
  return event.target instanceof Element ? event.target : null;
}
window.addEventListener("keydown", event => {
  const target = keyTarget(event);
  if (source && event.key === "/" && document.activeElement.tagName !== "INPUT") {
    event.preventDefault();
    palOpen();
    return;
  }
  if (event.key === "Escape") {
    if (!pal.hidden) { palClose(); return; }
    if (target && target.closest("input, textarea")) { target.blur(); return; }
    leaveOneLevel();
    return;
  }
  if (!source || !pal.hidden || event.target.tagName === "INPUT") return;
  if (event.key !== "j" && event.key !== "k") return;
  const items = feed();
  const index = items.findIndex(item => item.id === ui.item);
  const next = items[index + (event.key === "j" ? 1 : -1)];
  if (!next) return;
  ui.folds = {};
  ui.space = hasAction(next) ? "focus" : "browse";
  applyItem(next, "hit");
  render();
});

document.body.addEventListener("click", event => {
  if (event.target.closest("#open-find")) palOpen();
});

const app = document.getElementById("app");
try {
  source = location.pathname === "/"
    ? await (await import("./studio-observe.js?v=2")).observeSource()
    : (await import("./studio-demo.js?v=1")).demoSource();
  if (source.origin) document.title = "样例 · 测光";
  people = source.people;
  rooms = source.rooms;
  ui.room = source.defaultRoom || rooms[0]?.id || "";
  ui.person = people[0]?.id || "";
  render();
} catch (error) {
  console.error(error);
  app.textContent = "测光暂时读不到记录。请刷新重试。";
}

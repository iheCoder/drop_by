const M = window.Meter;
const ui = {
  scope: "room",
  room: "chat",
  person: "cheng",
  item: "",
  work: "cheng_research",
  step: "r_compact",
  hideCache: false,
  open: "",
  sec: 470,
  note: "",
  folds: {},
  q: "",
  hit: 0,
  focus: "end",
  local: null,
  shown: 12,
  depth: "step",
  space: "browse",
  threadAnchor: null,
  threadTop: 0,
  chainTop: 0,
  bodyTop: 0,
  keepBody: false,
  hold: false,
  anchor: null
};

const RATES = {
  "gpt-6.1-sol": { input: 2000, cached: 100, write: 2500, output: 10000 },
  "gpt-6.1-terra": { input: 1000, cached: 50, write: 1250, output: 5000 }
};

function callStep(partial) {
  const rate = RATES[partial.model];
  const usage = partial.usage;
  const normal = usage.input - usage.cached - usage.write;
  const nano = normal * rate.input + usage.cached * rate.cached + usage.write * rate.write + usage.output * rate.output;
  return Object.assign({
    kind: "model",
    price: Math.ceil(nano / 1000),
    usageText: `输入 ${usage.input.toLocaleString("zh-CN")} · 缓存读 ${usage.cached.toLocaleString("zh-CN")} · 缓存写 ${usage.write.toLocaleString("zh-CN")} · 输出 ${usage.output.toLocaleString("zh-CN")}`
  }, partial);
}

const people = [
  { id: "cheng", name: "阿澄", face: "portrait-sky", note: "研究还开着" },
  { id: "zhou", name: "老周", face: "portrait-traveler", note: "群里已经说完" },
  { id: "man", name: "小满", face: "portrait-cat", note: "有一件在等预算" }
];

const rooms = [
  { id: "chat", place: "群", name: "随便聊聊", blurb: "11 月 11 日这场。话和没说出来的调用在同一条里", members: ["cheng", "zhou", "man"] },
  {
    id: "wood", place: "群", name: "木板和雨", blurb: "更早的一小段，从最新一条往上翻", members: ["cheng", "zhou", "man"],
    messages: [
      { id: "w1", who: "你", time: "11 月 2 日 21:12", text: "木板被雨打得太闷了。" },
      { id: "w2", who: "老周", time: "11 月 2 日 21:18", text: "闷就对了。木头吸了水，声音就发不出去。" },
      { id: "w3", who: "小满", time: "11 月 2 日 21:24", text: "我想听干一点的。这种闷得人发慌。" },
      { id: "w4", who: "你", time: "11 月 2 日 21:31", text: "那得换个屋顶。先记着，别在这儿下结论。" }
    ]
  },
  {
    id: "dm-cheng", place: "私聊", name: "阿澄", blurb: "很长，从最新一条往上翻", members: ["cheng"],
    messages: [
      { id: "d1", who: "你", time: "11 月 3 日 22:04", text: "昨晚那个雨声太闷了，像整块木板捂在耳朵上。" },
      { id: "d2", who: "阿澄", time: "11 月 3 日 22:11", text: "闷多半是木头。铁皮会干一点，声音收得紧。我还不能断定你听到的是哪一种。", call: callStep({
        id: "d2c", title: "私聊里的第一次回复", model: "gpt-6.1-terra", reasoning: "none", purpose: "私聊", ms: 14000,
        usage: { input: 1800, cached: 0, write: 1500, output: 90 },
        output: "闷多半是木头。铁皮会干一点，声音收得紧。我还不能断定你听到的是哪一种。",
        segments: [
          { k: "系统身份", tokens: 860, text: "你是阿澄。我习惯把没弄清的声音先留着，不急着起名字。对人可以很近，对结论要慢一点。" },
          { k: "已有窗口", tokens: 640, text: "私聊里还没有更早的话。这是这段私聊的开头。" },
          { k: "这次来由", tokens: 300, text: "你：昨晚那个雨声太闷了，像整块木板捂在耳朵上。" }
        ]
      }) },
      { id: "d3", who: "你", time: "11 月 3 日 22:16", text: "你怎么会想到铁皮。" },
      { id: "d4", who: "阿澄", time: "11 月 3 日 22:20", text: "还不知道。先把铁皮记成一个可能，不把它当成答案。" },
      { id: "d5", who: "你", time: "11 月 5 日 19:40", text: "我好像玩过一个游戏，雨打在屋顶上，画面几乎是灰的。名字忘了。" },
      { id: "d6", who: "阿澄", time: "11 月 5 日 19:48", text: "名字先别找。你记得的灰，是平的，还是有雨的纹理。", call: callStep({
        id: "d6c", title: "先拦住名字", model: "gpt-6.1-terra", reasoning: "low", purpose: "私聊", ms: 16000,
        usage: { input: 3200, cached: 1800, write: 0, output: 70 },
        output: "名字先别找。你记得的灰，是平的，还是有雨的纹理。",
        note: "开头那一轮已经写进缓存。这次缓存读了 1,800，没有按输入全价再计。",
        segments: [
          { k: "系统身份", tokens: 860, cache: true, text: "你是阿澄。我习惯把没弄清的声音先留着，不急着起名字。" },
          { k: "已有窗口", tokens: 940, cache: true, text: "11 月 3 日：闷是木头，铁皮先只是一个可能。" },
          { k: "这次来由", tokens: 1400, text: "你：我好像玩过一个游戏，雨打在屋顶上，画面几乎是灰的。名字忘了。" }
        ]
      }) },
      { id: "d7", who: "你", time: "11 月 5 日 19:55", text: "忘了。当时觉得它就是灰，没有别的颜色。" },
      { id: "d8", who: "阿澄", time: "11 月 6 日 08:12", text: "那我先不把灰和雨声并成同一个东西。" },
      { id: "d9", who: "你", time: "11 月 7 日 23:02", text: "老周说两块灰并排的时候，左边其实更暖。我当时完全当它们是一样的。" },
      { id: "d10", who: "阿澄", time: "11 月 7 日 23:10", text: "我没听见暖。等我自己看，不借用他的眼睛。" },
      { id: "d11", who: "你", time: "11 月 8 日 12:20", text: "你要是去查，查完跟我说一声就行，不用写很长。" },
      { id: "d12", who: "阿澄", time: "11 月 8 日 12:36", text: "有一页超时了，没有正文。我换了一个地址，还没有读完。", call: {
        id: "d12c", kind: "tool", title: "读网页失败", tool: "read_page", price: 750, ms: 500,
        note: "超时，没有正文。费用仍按次记 $0.000750。下一笔换了地址，所以是另一次调用，不是把这一次重放成功。",
        args: '{\n  "url": "https://example.invalid/dry-rain-full"\n}',
        result: "超时。没有正文。"
      } },
      { id: "d13", who: "你", time: "11 月 8 日 12:40", text: "超时就超时。别为了有个结果硬凑一个名字。" },
      { id: "d14", who: "阿澄", time: "11 月 9 日 18:05", text: "换地址之后读到了。铁皮屋顶把雨收成很密的声音，房间里只有一种灰。" },
      { id: "d15", who: "你", time: "11 月 9 日 18:22", text: "这句我有印象。名字呢。" },
      { id: "d16", who: "阿澄", time: "11 月 9 日 18:28", text: "文末没有名字。颜色是平的，而且不动。" },
      { id: "d17", who: "你", time: "11 月 10 日 21:15", text: "群里你如果要说，就说你自己听见的。别把老周的暖也说成你的。" },
      { id: "d18", who: "阿澄", time: "11 月 10 日 21:24", text: "知道。平灰先别并成同一个游戏。" },
      { id: "d19", who: "你", time: "11 月 11 日 21:36", text: "刚才群里你说名字先不下。私聊里也可以先留着。" },
      { id: "d20", who: "阿澄", time: "11 月 11 日 21:41", text: "留着。铁皮对得上声音，对不上那块灰。明天再听一次，不在这里补一个名字。", call: callStep({
        id: "d20c", title: "最新一条回复", model: "gpt-6.1-sol", reasoning: "low", purpose: "私聊", ms: 18000,
        usage: { input: 6400, cached: 4000, write: 0, output: 180 },
        output: "留着。铁皮对得上声音，对不上那块灰。明天再听一次，不在这里补一个名字。",
        note: "私聊已经很长。前缀里有 4,000 是缓存读，剩下的新话才按未命中计。",
        segments: [
          { k: "系统身份", tokens: 860, cache: true, text: "你是阿澄。我习惯把没弄清的声音先留着，不急着起名字。对人可以很近，对结论要慢一点。" },
          { k: "已有窗口", tokens: 3140, cache: true, text: "从 11 月 3 日的闷，到铁皮只是可能，再到名字先别找、页超时、换地址后读到平灰。" },
          { k: "这次来由", tokens: 2400, text: "你：刚才群里你说名字先不下。私聊里也可以先留着。" }
        ]
      }) }
    ]
  },
  {
    id: "dm-zhou", place: "私聊", name: "老周", blurb: "几句，都围绕那块灰", members: ["zhou"],
    messages: [
      { id: "z1", who: "你", time: "11 月 7 日 20:02", text: "你说的那块灰，是平的吗？" },
      { id: "z2", who: "老周", time: "11 月 7 日 20:08", text: "平的。没有雨的纹理。" },
      { id: "z3", who: "你", time: "11 月 7 日 20:11", text: "和声音对得上吗？" },
      { id: "z4", who: "老周", time: "11 月 7 日 20:16", text: "铁皮对得上声音，对不上这块灰。" },
      { id: "z5", who: "你", time: "11 月 7 日 20:20", text: "行。我让阿澄自己看，不把你的话塞给她。" }
    ]
  },
  {
    id: "dm-man", place: "私聊", name: "小满", blurb: "还没真正接上", members: ["man"],
    messages: [
      { id: "m1", who: "小满", time: "11 月 11 日 18:02", text: "我还在听屋顶，先不发到群里。" },
      { id: "m2", who: "你", time: "11 月 11 日 18:10", text: "群里先不用接。" },
      { id: "m3", who: "小满", time: "11 月 11 日 21:02", text: "到点了，可是这次预留放不进本周额度。还没有模型调用，也没有在这里催你。" }
    ]
  }
];

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
  return "terra";
}

function barKey(bar) {
  if (bar.tone === "wait") return "wait";
  if (bar.tone === "quiet") return "quiet";
  if (bar.open || bar.tone === "sky") return "sol";
  if (bar.work === "man_personal") return "personal";
  return "terra";
}

function roomById(id) { return rooms.find(room => room.id === id) || rooms[0]; }
function currentRoom() { return roomById(ui.room); }
function current() {
  if (ui.local) return ui.local;
  const work = M.workById(ui.work);
  return { work, step: M.stepById(work, ui.step) };
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
  const rate = RATES[step.model];
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
function messageView(room, msg) {
  const call = msg.call;
  if (call) {
    const step = Object.assign({ sec: 0 }, call);
    return {
      work: { who: msg.who, kind: room.place, title: room.name, status: room.place === "私聊" ? "已写回私聊" : "已写回这段群聊", steps: [step] },
      step
    };
  }
  return {
    work: { who: msg.who, kind: room.place, title: room.name, status: "原话", steps: [] },
    step: { id: msg.id, kind: "said", title: msg.who === "你" ? "你写的" : "这条没有单独的模型记录", body: msg.who === "你" ? "这是你写的。没有模型调用，没有上下文，也没有费用。" : "样例里这条没有挂上单独的调用记录。话在对话里，费用和上下文要到挂了调用的那几条才看得到。", who: msg.who, text: msg.text }
  };
}
function hitsIn(room) {
  const q = ui.q.trim().toLowerCase();
  if (!q || !room.messages) return [];
  return room.messages.filter(msg => msg.text.toLowerCase().includes(q));
}
function markText(text) {
  const q = ui.q.trim();
  const safe = M.esc(text);
  if (!q) return safe;
  return safe.split(M.esc(q)).join("<mark>" + M.esc(q) + "</mark>");
}
function personById(id) { return people.find(person => person.id === id) || people[0]; }
function personKey(name) {
  if (name === "你") return "you";
  const found = people.find(person => person.name === name);
  return found ? found.id : "you";
}
function atOf(time) {
  const matched = String(time).match(/(\d+)\s*月\s*(\d+)\s*日\s*(\d+):(\d+)/);
  if (!matched) return 0;
  const month = matched[1].padStart(2, "0");
  const day = matched[2].padStart(2, "0");
  return Date.parse(`2026-${month}-${day}T${matched[3]}:${matched[4]}:00+08:00`);
}
function sceneAt(sec) { return Date.parse("2026-11-11T21:10:00+08:00") + sec * 1000; }
function faceHTML(id, small) {
  if (id === "you") return `<span class="ava you${small ? " sm" : ""}" aria-hidden="true">你</span>`;
  const person = personById(id);
  return `<span class="ava${small ? " sm" : ""}"><img src="/assets/${person.face}.webp" alt=""></span>`;
}
function facesHTML(ids, large) {
  return `<span class="faces${large ? " lg" : ""}">${ids.map(id => faceHTML(id)).join("")}</span>`;
}
function stepOf(item) {
  if (!item) return null;
  if (item.stepRef) return M.stepById(M.workById(item.stepRef.work), item.stepRef.step);
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
  if (step.reserve) return "未回执";
  if (step.kind === "tool" && /超时|失败/.test((step.title || "") + (step.result || ""))) return "失败";
  if (step.kind === "quiet" || step.kind === "budget") return "未发送";
  if (step.id === "z1" || step.id === "c2" || step.id === "z2") return "已发到群";
  const item = currentItem();
  if (item && item.call && String(item.where || "").includes("私聊")) return "已发到私聊";
  if (item && item.kind === "say" && item.call) return String(item.where || "").startsWith("群") ? "已发到群" : "已发到私聊";
  return "未发送";
}
function kickerFor(step, where) {
  if (where && where !== "群 · 随便聊聊") return where;
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
function sceneFeed() {
  const names = { you: "你", cheng: "阿澄", zhou: "老周", man: "小满" };
  const spoken = {
    290: { work: "zhou_1", step: "z1" },
    345: { work: "cheng_chat", step: "c2" },
    500: { work: "zhou_2", step: "z2" }
  };
  const attached = new Set(["z1", "c2", "z2"]);
  const items = M.ticks.map(tick => ({
    id: "say-" + tick.lane + "-" + tick.sec,
    kind: tick.kind === "steer" ? "steer" : "say",
    who: names[tick.lane],
    person: tick.lane,
    sec: tick.sec,
    at: sceneAt(tick.sec),
    time: "11 月 11 日 " + M.clock(tick.sec).slice(0, 5),
    text: tick.text,
    stepRef: spoken[tick.sec] || null,
    where: "群 · 随便聊聊"
  }));
  for (const work of M.works) {
    if (work.id === "man_personal" || work.id === "man_budget") continue;
    for (const step of work.steps) {
      if (attached.has(step.id)) continue;
      items.push({
        id: "ev-" + work.id + "-" + step.id,
        kind: "event",
        who: work.who,
        person: personKey(work.who),
        sec: step.sec,
        at: sceneAt(step.sec),
        time: "11 月 11 日 " + M.clock(step.sec).slice(0, 5),
        title: step.title,
        stepRef: { work: work.id, step: step.id },
        where: "群 · 随便聊聊"
      });
    }
  }
  items.sort((a, b) => a.at - b.at || (a.kind === "event" ? 1 : -1));
  return items;
}
function roomFeed(room) {
  if (room.id === "chat") return sceneFeed();
  return (room.messages || []).map(msg => ({
    id: msg.id,
    kind: "say",
    who: msg.who,
    person: personKey(msg.who),
    at: atOf(msg.time),
    time: msg.time,
    text: msg.text,
    call: msg.call || null,
    where: room.place === "私聊" ? "私聊" : room.place + " · " + room.name
  }));
}
function whereOf(work) {
  if (work.kind === "个人") return "个人活动";
  if (work.kind === "后续") return "还没进任何聊天";
  return "群 · 随便聊聊";
}
function personFeed(person) {
  const items = [];
  for (const work of M.works) {
    if (work.who !== person.name) continue;
    for (const step of work.steps) {
      const at = work.id === "man_budget" ? Date.parse("2026-11-11T21:00:00+08:00") : sceneAt(step.sec || 0);
      items.push({
        id: "pf-" + work.id + "-" + step.id,
        kind: "event",
        who: person.name,
        person: person.id,
        at,
        time: work.id === "man_budget" ? "11 月 11 日 21:00" : "11 月 11 日 " + M.clock(step.sec || 0).slice(0, 5),
        title: step.title,
        stepRef: { work: work.id, step: step.id },
        where: whereOf(work)
      });
    }
  }
  for (const room of rooms) {
    if (room.place !== "私聊" || !room.members.includes(person.id)) continue;
    for (const msg of room.messages || []) {
      if (msg.who !== person.name || !msg.call) continue;
      items.push({
        id: "pf-" + msg.id,
        kind: "event",
        who: person.name,
        person: person.id,
        at: atOf(msg.time),
        time: msg.time,
        title: msg.call.title,
        call: msg.call,
        where: "私聊"
      });
    }
  }
  items.sort((a, b) => a.at - b.at);
  return items;
}
function feed() {
  return ui.scope === "person" ? personFeed(personById(ui.person)) : roomFeed(currentRoom());
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
function viewOf(item) {
  const step = stepOf(item);
  if (step && item.stepRef) {
    const work = M.workById(item.stepRef.work);
    return { work, step };
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
  ui.depth = "step";
  ui.chainTop = 0;
  ui.revealStep = true;
  ui.keepBody = false;
  if (item.stepRef) ui.work = item.stepRef.work;
  if (item.sec) ui.sec = item.sec;
  reveal(item);
}
function sumFeed(items) {
  let settled = 0;
  let reserve = 0;
  const seen = new Set();
  for (const item of items) {
    const step = stepOf(item);
    if (!step || seen.has(step.id)) continue;
    seen.add(step.id);
    if (step.reserve) reserve += step.reserve;
    else if (step.price) settled += step.price;
  }
  return { settled, reserve };
}

function fold(id, text) {
  const long = String(text || "").length > 72;
  const open = !!ui.folds[id];
  const body = `<p class="${long && !open ? "clamp" : ""}">${M.esc(text)}</p>`;
  const button = long ? `<button type="button" class="fold" data-fold="${M.esc(id)}">${open ? "收起" : "展开全文"}</button>` : "";
  return body + button;
}

function terrain() {
  const n = 64;
  const counts = [];
  for (let i = 0; i < n; i++) {
    const t = (i + 0.5) * (M.HORIZON / n);
    counts.push(M.bars.filter(bar => t >= bar.a && t < bar.b && bar.tone !== "wait").length);
  }
  const max = Math.max(...counts, 1);
  const width = 1000;
  const height = 72;
  const pts = counts.map((count, i) => {
    const x = (i / (n - 1)) * width;
    const y = height - 6 - (count / max) * 52;
    return [x.toFixed(1), y.toFixed(1)];
  });
  const line = pts.map(point => point.join(",")).join(" ");
  const area = `M0,${height} L${pts.map(point => point.join(",")).join(" L")} L${width},${height} Z`;
  return `<svg class="terrain" viewBox="0 0 ${width} ${height}" preserveAspectRatio="none" aria-hidden="true"><defs><linearGradient id="terrain-fill" x1="0" y1="0" x2="1" y2="0"><stop class="s1" offset="0"/><stop class="s2" offset="0.55"/><stop class="s3" offset="1"/></linearGradient></defs><path d="${area}" fill="url(#terrain-fill)"/><polyline points="${line}" fill="none" stroke="url(#terrain-fill)" stroke-width="3" stroke-linejoin="round" vector-effect="non-scaling-stroke"/></svg>`;
}

function mapHTML() {
  const marks = [0, 360, 720, 1080].map((sec, i) => {
    const cls = i === 0 ? "start" : i === 3 ? "end" : "";
    return `<span class="${cls}" style="left:${sec / M.HORIZON * 100}%">${M.clock(sec).slice(0, 5)}</span>`;
  }).join("");
  const rows = M.lanes.map(([id, name]) => {
    const spans = M.bars.filter(bar => bar.lane === id).map(bar => {
      const width = Math.max((bar.b - bar.a) / M.HORIZON * 100, 0.9);
      const show = width >= 11 ? M.esc(bar.label) : "";
      const key = barKey(bar);
      const on = ui.work === bar.work ? " on" : "";
      const open = bar.open ? " open" : "";
      return `<button type="button" class="span tone-${key}${on}${open}" style="left:${bar.a / M.HORIZON * 100}%;width:${width}%" data-span="${bar.work}" data-seek="${bar.a}" aria-label="${M.esc(bar.label)}，${TONE_NAME[key]}"><i></i><span>${show}</span></button>`;
    }).join("");
    const pips = M.bars.filter(bar => bar.lane === id && bar.work === ui.work).flatMap(bar => {
      const work = M.workById(bar.work);
      return work.steps.filter(step => step.sec >= bar.a && step.sec < bar.b).map(step => {
        const on = ui.step === step.id ? " on" : "";
        return `<button type="button" class="pip tone-${toneKey(step)}${on}" style="left:${step.sec / M.HORIZON * 100}%" data-step="${step.id}" data-work="${work.id}" aria-label="${M.esc(step.title)}，${M.dur(step.ms)}，${M.priceLabel(step)}"></button>`;
      });
    }).join("");
    const ticks = M.ticks.filter(tick => tick.lane === id).map(tick => `<button type="button" class="tick ${tick.kind}" style="left:${tick.sec / M.HORIZON * 100}%" data-tick="${tick.sec}" data-text="${M.esc(tick.text)}" aria-label="${tick.kind === "steer" ? "真人补充" : "消息"}"></button>`).join("");
    return `<div class="lane"><div class="lane-name">${M.esc(name)}</div><div class="track">${ticks}${spans}${pips}</div></div>`;
  }).join("");
  const near = M.frames.reduce((best, frame) => Math.abs(frame.sec - ui.sec) < Math.abs(best.sec - ui.sec) ? frame : best);
  const lede = ui.note ? ui.note : (Math.abs(near.sec - ui.sec) <= 12 ? near.label : "正在看这一刻");
  const who = { you: "你", cheng: "阿澄", zhou: "老周", man: "小满" };
  const talk = [...M.ticks].sort((a, b) => a.sec - b.sec).map(tick => `<li><button type="button" data-tick="${tick.sec}" data-text="${M.esc(tick.text)}"><b>${who[tick.lane]}</b><time>${M.clock(tick.sec).slice(0, 5)}</time><span>${M.esc(tick.text)}</span></button></li>`).join("");
  return `<div class="problems"><button type="button" data-step="r_fail" data-work="cheng_research">读网页超时</button><button type="button" data-step="r_drop" data-work="cheng_research">连接断了，预留还在</button></div><div class="sheet" style="--at:${ui.sec / M.HORIZON}"><div class="terrain-wrap">${terrain()}</div><div class="ruler"><div class="marks">${marks}</div><input id="scrub" type="range" min="0" max="${M.HORIZON}" step="1" value="${ui.sec}" aria-label="移动时刻"></div><div class="lanes">${rows}</div><div class="play" id="play"></div></div><p class="readout"><time>${M.esc(M.clock(ui.sec))}</time><span>${M.esc(lede)}</span></p><section class="talk"><h3>这场说出来的话</h3><p class="aside">上面的菱形是这些话的位置。这里按说话顺序读，点一句就回到那一刻。</p><ol>${talk}</ol></section>`;
}

function transportHTML() {
  const frames = M.frames.map(frame => {
    const on = Math.abs(frame.sec - ui.sec) <= 8;
    return `<button type="button" class="moment${on ? " on" : ""}" data-frame="${frame.sec}" aria-pressed="${on}"><time>${M.clock(frame.sec).slice(0, 5)}</time><span>${M.esc(frame.label)}</span></button>`;
  }).join("");
  const budgetOn = ui.work === "man_budget" ? " on" : "";
  return `${frames}<button type="button" class="moment ghost${budgetOn}" data-span="man_budget"><time>21:00</time><span>等预算</span></button>`;
}

function metricsHTML(step) {
  const bill = billOf(step);
  const cached = bill ? bill.usage.cached : 0;
  const failed = step.kind === "tool" && /超时|失败/.test((step.title || "") + (step.result || ""));
  const cells = step.kind === "tool"
    ? [["时长", M.dur(step.ms)], ["按次", M.priceLabel(step)], ["结果", failed ? "失败" : "拿到了"]]
    : [
      ["时长", step.reserve ? "未知" : M.dur(step.ms)],
      ["这次金额", step.reserve ? "预留 " + M.money(step.reserve) : M.priceLabel(step)],
      ["缓存读", cached ? cached.toLocaleString("zh-CN") + " tokens" : "没有"]
    ];
  if (step.beforeTokens) cells.push(["拿回来", step.afterTokens.toLocaleString("zh-CN")]);
  return `<div class="metrics">${cells.map(([k, v]) => `<div><small>${k}</small><b>${M.esc(v)}</b></div>`).join("")}</div>`;
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
  if (!step.price) return "不另计";
  const bill = billOf(step);
  const cache = bill && bill.usage.cached ? " · 缓存读 " + bill.usage.cached.toLocaleString("zh-CN") : "";
  const time = step.ms ? M.dur(step.ms) + " · " : "";
  return time + M.priceLabel(step) + cache;
}
function costBlock(step) {
  const id = "bill:" + step.id;
  const open = !!ui.folds[id];
  const can = step.kind === "tool" || (!step.reserve && !!billOf(step));
  const button = can ? `<button type="button" class="text-btn" data-fold="${id}" aria-expanded="${open ? "true" : "false"}">${open ? "收起费用明细" : "费用明细"}</button>` : "";
  const detail = open
    ? (step.kind === "tool"
      ? `<p class="aside">搜索和读网页按次另计，每笔 $0.000750，不进模型的缓存账。</p>`
      : billHTML(step))
    : "";
  return `<div class="cost-line"><b>${M.esc(costSummary(step))}</b>${button}</div>${detail}`;
}

function payloadHTML(work, step) {
  const tone = toneKey(step);
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
  if (step.reserve) return true;
  return step.kind === "tool" && /超时|失败/.test((step.title || "") + (step.result || ""));
}
function sourceQuote() {
  const item = feed().find(entry => entry.id === ui.item);
  if (!item) return "";
  const text = (item.text || snippet(stepOf(item)) || item.title || "").replace(/\s+/g, " ");
  return text.length > 42 ? text.slice(0, 42) + "…" : text;
}
function stepClock(step) {
  if (step && step.sec != null) return M.clock(step.sec).slice(0, 5);
  const item = feed().find(entry => entry.id === ui.item);
  const matched = item && String(item.time || "").match(/\d+:\d+/);
  return matched ? matched[0] : "";
}
function stepsHTML(work) {
  const max = Math.max(...work.steps.map(step => step.ms || 0), 1);
  return `<ol class="steps">${work.steps.map(step => {
    const on = step.id === ui.step ? " on" : "";
    const bad = failedStep(step) ? " bad" : "";
    const width = Math.max(4, Math.round((step.ms || 0) / max * 100));
    const mark = failedStep(step) ? alertIcon() : "";
    return `<li><button type="button" class="tone-${toneKey(step)}${on}${bad}" data-step="${step.id}"${on ? ` aria-current="step"` : ""}><i></i><time>${M.esc(stepClock(step))}</time><span>${mark}<b>${M.esc(step.title)}</b><span class="meter" aria-hidden="true"><i style="width:${width}%"></i></span></span><em>${step.ms ? M.dur(step.ms) : (step.reserve ? "未回执" : M.dur(step.ms))} · ${M.priceLabel(step)}</em></button></li>`;
  }).join("")}</ol>`;
}
function jumpButtons(work) {
  const steps = work.steps || [];
  const failed = steps.find(failedStep);
  const priced = steps.filter(step => step.price);
  const costliest = priced.reduce((best, step) => step.price > best.price ? step : best, priced[0] || null);
  const slowest = steps.reduce((best, step) => (step.ms || 0) > ((best && best.ms) || 0) ? step : best, null);
  const jumps = [];
  if (failed) jumps.push(`<button type="button" class="bad" data-step="${failed.id}">${alertIcon()}失败的一步</button>`);
  if (priced.length > 1 && costliest) jumps.push(`<button type="button" data-step="${costliest.id}">最贵的一步</button>`);
  if (slowest && slowest.ms && (!failed || slowest.id !== failed.id) && (!costliest || slowest.id !== costliest.id)) jumps.push(`<button type="button" data-step="${slowest.id}">最慢的一步</button>`);
  return jumps;
}
function chainHead(work) {
  const jumps = jumpButtons(work);
  return `<div class="chain-head"><h3>这次行动的调用</h3>${jumps.length ? `<div class="chain-jumps">${jumps.join("")}</div>` : ""}</div>`;
}
function stepSwitch(work) {
  if (!work.steps || work.steps.length < 2) return "";
  const jumps = jumpButtons(work);
  const rail = work.steps.map(step => {
    const on = step.id === ui.step ? " on" : "";
    const bad = failedStep(step) ? " bad" : "";
    const mark = failedStep(step) ? alertIcon() : "";
    return `<button type="button" class="tone-${toneKey(step)}${on}${bad}" data-step="${step.id}"${on ? ` aria-current="step"` : ""}>${mark}<b>${M.esc(step.title)}</b></button>`;
  }).join("");
  return `<div class="step-switch">${jumps.length ? `<div class="chain-jumps">${jumps.join("")}</div>` : ""}<div class="step-rail">${rail}</div></div>`;
}
function pinHTML(work, step, spoken, tone) {
  const spent = (work.steps || []).reduce((sum, item) => sum + (item.ms || 0), 0);
  const money = spoken ? "没有单独的调用" : `${spent ? "调用合计 " + M.dur(spent) + " · " : ""}${M.costLine(work)}`;
  const fault = !spoken && (work.steps || []).find(item => item.reserve)
    ? "有一步还没有回执。预留还在，不能写成已经花掉。"
    : (!spoken && (work.steps || []).some(item => item.kind === "tool" && /超时|失败/.test((item.title || "") + (item.result || ""))) ? "有一步失败了。费用仍按那一次记下。" : "");
  const title = spoken ? step.title : work.title;
  const kicker = spoken ? `${work.who} · 原话` : `${work.who} · ${work.kind || ""}`;
  const outcome = !spoken && work.status ? `<p class="pin-outcome">${M.esc(work.status)}</p>` : "";
  return `<header class="pin pin-tight tone-${tone}"><div class="pin-row">${faceHTML(personKey(work.who), true)}<div class="pin-copy"><p class="kicker">${M.esc(kicker)}</p><h2>${M.esc(title)}</h2></div></div><p class="pin-meta">${M.esc(money)}</p>${outcome}${fault ? `<p class="fault">${alertIcon()}${M.esc(fault)}</p>` : ""}</header>`;
}
function stepHead(step) {
  if (!step || step.kind === "said") return "";
  return `<header class="step-head"><h3>${M.esc(step.title)}</h3><p><b>${M.esc(costSummary(step))}</b> · ${M.esc(stepStatus(step))}</p></header>`;
}

function faultLine(step) {
  if (step.reserve) return "系统没有拿到用量回执。预留还在，不能写成已经花掉，也不能写成免费。";
  if (step.kind === "tool" && /超时|失败/.test((step.title || "") + (step.result || ""))) return "工具失败。没有拿到正文。费用仍按这一次记下，没有抹成没发生。";
  return "";
}

function inspectHTML() {
  const { work, step } = current();
  const spoken = step.kind === "said" || !(work.steps && work.steps.length);
  const tone = spoken ? "wait" : toneKey(ui.depth === "step" ? step : (work.steps.find(item => item.id === ui.step) || step));
  const pin = pinHTML(work, step, spoken, tone);
  if (ui.space !== "focus" || spoken) {
    const text = spoken ? (step.text || "") : snippet(step);
    const state = spoken ? (step.body || "") : stepStatus(step);
    return `${pin}<div class="inspect-body"><section class="card summary"><p class="status">${M.esc(state)}</p><p>${M.esc(text)}</p></section></div>`;
  }
  const payload = `<section class="card payload tone-${toneKey(step)}">${stepHead(step)}${payloadHTML(work, step)}</section>`;
  if (work.steps.length > 1) {
    return `${pin}<div class="inspect-body workbench"><section class="card chain-card">${chainHead(work)}${stepsHTML(work)}</section>${payload}</div>`;
  }
  return `${pin}<div class="inspect-body">${payload}</div>`;
}

function legendHTML() {
  return ["terra", "sol", "personal", "tool", "compact", "quiet", "hold"].map(key => `<span><i class="tone-${key}"></i>${TONE_NAME[key]}</span>`).join("");
}

function sayHTML(item, grouped) {
  const you = item.person === "you";
  const step = stepOf(item);
  const on = item.id === ui.item;
  const hit = ui.q && itemSearch(item).toLowerCase().includes(ui.q.trim().toLowerCase());
  const chip = step ? `<span class="callchip tone-${toneKey(step)}">${TONE_NAME[toneKey(step)]} · ${M.priceLabel(step)}</span>` : "";
  const steer = item.kind === "steer" ? `<em class="steer-note">只进队列，不插进正在进行的调用</em>` : "";
  const clock = (item.time.match(/\d+:\d+/) || [""])[0];
  const head = grouped ? "" : `<header><b>${M.esc(item.who)}</b><time>${M.esc(clock)}</time></header>`;
  const avatar = you || grouped ? "" : faceHTML(item.person);
  return `<article class="bubble ${you ? "you" : "them"}${grouped ? " grouped" : ""}${on ? " on" : ""}${hit ? " hit" : ""}${ui.focus === "hit" && on ? " focus" : ""}" id="item-${item.id}">${avatar}<div class="bubble-main">${head}<button type="button" class="bubble-hit" data-item="${item.id}"><span class="line">${markText(item.text || "")}</span>${steer}${chip}</button></div></article>`;
}
function eventHTML(item) {
  const step = stepOf(item);
  const on = item.id === ui.item;
  const hit = ui.q && itemSearch(item).toLowerCase().includes(ui.q.trim().toLowerCase());
  const tone = toneKey(step);
  const text = snippet(step);
  const clock = (item.time.match(/\d+:\d+/) || [""])[0];
  const label = ui.scope === "person"
    ? `<span class="where">${M.esc(item.where || "")}</span>`
    : `${M.esc(item.who)} · ${M.esc(kickerFor(step, ""))}`;
  return `<article class="event tone-${tone}${on ? " on" : ""}${hit ? " hit" : ""}${ui.focus === "hit" && on ? " focus" : ""}" id="item-${item.id}"><button type="button" data-item="${item.id}">${faceHTML(item.person, true)}<span class="event-copy"><b>${label}</b><strong>${M.esc(step.title)}</strong><small>${markText(text)}</small></span><span class="event-side"><em>${M.esc(M.priceLabel(step))}</em><time>${M.esc(clock)}</time></span></button></article>`;
}
function threadBody(items, more) {
  let html = `<div class="spacer"></div>${more ? `<button type="button" class="earlier" data-earlier>往上还有 ${more} 条</button>` : `<p class="origin">样例停在 11 月 11 日 21:28，不是正在发生的记录。</p>`}`;
  let day = "";
  let who = "";
  for (const item of items) {
    const nextDay = item.time.replace(/\s+\d+:\d+$/, "");
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
    const last = personFeed(person).at(-1);
    const step = stepOf(last);
    const preview = step ? step.title + " · " + M.priceLabel(step) : person.note;
    return railButton(ui.scope === "person" && ui.person === person.id, `data-person="${person.id}"`, faceHTML(person.id), person.name, preview);
  }).join("");
  const roomRows = place => rooms.filter(room => room.place === place).map(room => {
    const last = roomFeed(room).at(-1);
    const preview = last ? (last.text || last.title || room.blurb) : room.blurb;
    const face = room.place === "群" ? facesHTML(room.members) : faceHTML(room.members[0]);
    return railButton(ui.scope === "room" && ui.room === room.id, `data-room="${room.id}"`, face, room.name, preview);
  }).join("");
  return `<nav class="rail" aria-label="角色和聊天"><p class="rail-label">角色</p>${personRows}<p class="rail-label">群聊</p>${roomRows("群")}<p class="rail-label">私聊</p>${roomRows("私聊")}</nav>`;
}
function openRoom(room) {
  ui.scope = "room";
  ui.room = room.id;
  ui.q = "";
  ui.hit = 0;
  ui.focus = "end";
  ui.item = "";
  ui.local = null;
  ui.note = "";
  ui.folds = {};
  ui.hideCache = false;
  ui.shown = 12;
  ui.depth = "step";
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
  ui.note = "";
  ui.folds = {};
  ui.hideCache = false;
  ui.shown = 12;
  ui.depth = "step";
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
function savedChatWidth() {
  try {
    const width = Number(localStorage.getItem("dropby.spectrum.chatWidth"));
    if (width >= 220 && width <= 480) return width;
  } catch (error) { /* 样例页没有存储时用默认宽度 */ }
  return 300;
}
function setChatWidth(px) {
  ui.chatWidth = Math.min(480, Math.max(220, Math.round(px)));
  document.body.style.setProperty("--chat-w", ui.chatWidth + "px");
  try { localStorage.setItem("dropby.spectrum.chatWidth", String(ui.chatWidth)); } catch (error) { /* 记不住就只留在这一次 */ }
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
  const room = currentRoom();
  const title = person ? person.name : room.name;
  const place = person ? "角色" : room.place;
  const blurb = person ? "这个人在群、私聊和个人活动里的全部调用。从最近一次往上翻。" : (room.id === "chat" ? "话、补充，还有没发出来的调用，按时间排在同一条里。" : room.blurb);
  const totals = sum.settled || sum.reserve
    ? `<div class="totals"><p><small>已结算</small><strong>${M.money(sum.settled)}</strong></p>${sum.reserve ? `<p class="hold"><small>未回执</small><strong>${M.money(sum.reserve)}</strong></p>` : ""}</div>`
    : "";
  const face = person ? faceHTML(person.id) : (room.place === "群" ? facesHTML(room.members, true) : faceHTML(room.members[0]));
  const live = room.id === "chat" && ui.scope === "room" ? `<div class="live">${people.map(one => `<button type="button" data-person="${one.id}">${faceHTML(one.id, true)}<span>${M.esc(one.name)}</span><small>${M.esc(one.note)}</small></button>`).join("")}</div>` : "";
  const hint = person ? "往上翻更早的调用。" : "从最新一条开始。往上翻更早的。";
  const count = ui.q ? (ui.focus === "hit" && hits.length ? `${ui.hit + 1} / ${hits.length}` : `${hits.length} 处`) : "";
  const jumps = ui.q ? `<span class="hit-count">${count}</span><button type="button" data-jump="-1" aria-label="上一个结果">上一个</button><button type="button" data-jump="1" aria-label="下一个结果">下一个</button>` : "";
  document.body.classList.toggle("space-focus", ui.space === "focus");
  document.getElementById("app").innerHTML = `<a class="skip" href="#thread">跳到对话</a><header class="top"><a class="brand" href="/"><small>drop_by</small>测光</a><nav class="switch" aria-label="原型"><a href="/spectrum.html"${document.body.dataset.skin === "spectrum" ? ` aria-current="page"` : ""}>色谱</a><a href="/night.html"${document.body.dataset.skin === "night" ? ` aria-current="page"` : ""}>夜航</a><a href="/strata.html"${document.body.dataset.skin === "strata" ? ` aria-current="page"` : ""}>层叠</a></nav><button type="button" class="find" id="open-find"><span>查找</span><kbd>/</kbd></button></header><div class="body">${railHTML()}<main class="stage"><div class="stage-head">${face}<div class="stage-copy"><p class="kicker">${M.esc(place)}</p><h1>${M.esc(title)}</h1><p>${M.esc(blurb)}</p></div>${totals}</div>${live}<div class="finder"><svg class="ico" aria-hidden="true"><use href="/icons.svg#search"/></svg><input id="thread-q" value="${M.esc(ui.q)}" placeholder="${person ? "在这个人的调用里找" : "在这段对话里找"}" aria-label="${person ? "在这个人的调用里找" : "在这段对话里找"}">${jumps}</div><p class="hint">${hint}</p><div id="thread">${threadBody(visible, more)}</div></main><aside class="inspect">${inspectHTML()}</aside></div>`;
  const box = document.getElementById("thread");
  requestAnimationFrame(() => {
    pinThread();
    box.addEventListener("scroll", onThreadScroll);
  });
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

function secFromBar(event, button) {
  const start = Number(button.dataset.seek);
  const bar = M.bars.find(item => item.work === button.dataset.span && item.a === start);
  if (!bar) return start;
  const rect = button.getBoundingClientRect();
  const ratio = rect.width ? Math.min(1, Math.max(0, (event.clientX - rect.left) / rect.width)) : 0;
  return Math.round(bar.a + (bar.b - bar.a) * ratio);
}

document.body.addEventListener("click", event => {
  const foldBtn = event.target.closest("[data-fold]");
  const hide = event.target.closest("[data-hide-cache]");
  const jump = event.target.closest("[data-jump]");
  const itemBtn = event.target.closest("[data-item]");
  const personBtn = event.target.closest("[data-person]");
  const roomBtn = event.target.closest("[data-room]");
  const open = event.target.closest("[data-open]");
  const step = event.target.closest("[data-step]");
  if (foldBtn) {
    ui.folds[foldBtn.dataset.fold] = !ui.folds[foldBtn.dataset.fold];
    ui.keepBody = true;
    if (ui.focus === "end") ui.focus = "stay";
    render();
    return;
  }
  if (hide) {
    ui.hideCache = !ui.hideCache;
    ui.keepBody = true;
    if (ui.focus === "end") ui.focus = "stay";
    render();
    return;
  }
  if (jump) { jumpHit(Number(jump.dataset.jump)); return; }
  if (event.target.closest("[data-back]")) return;
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
    if (ui.space !== next) captureThread();
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
    if (ui.focus === "end") ui.focus = "stay";
    render();
    return;
  }
  if (step && ui.local && ui.local.work && ui.local.work.steps) {
    const picked = ui.local.work.steps.find(entry => entry.id === step.dataset.step);
    if (!picked) return;
    const chain = document.querySelector(".workbench .chain-card");
    if (chain) ui.chainTop = chain.scrollTop;
    ui.space = "focus";
    ui.folds = {};
    ui.hideCache = false;
    ui.keepBody = false;
    ui.local = { work: ui.local.work, step: picked };
    ui.step = picked.id;
    ui.depth = "step";
    ui.revealStep = true;
    if (ui.focus === "end") ui.focus = "stay";
    render();
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
  if (event.target.id !== "scrub") return;
  ui.sec = Number(event.target.value);
  ui.note = "";
  document.querySelector(".sheet")?.style.setProperty("--at", String(ui.sec / M.HORIZON));
  const out = document.querySelector(".readout time");
  if (out) out.textContent = M.clock(ui.sec);
});

document.body.addEventListener("keydown", event => {
  if (event.target.id === "thread-q" && event.key === "Enter") {
    event.preventDefault();
    jumpHit(1);
  }
});

document.body.addEventListener("change", event => {
  if (event.target.id !== "scrub") return;
  M.syncFromTime(ui);
  ui.folds = {};
  render();
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
      for (const item of roomFeed(room)) {
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
  if (event.key === "/" && document.activeElement.tagName !== "INPUT") {
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
  if (!pal.hidden || event.target.tagName === "INPUT") return;
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

render();

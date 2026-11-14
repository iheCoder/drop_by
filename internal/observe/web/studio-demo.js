import "./meter.js?v=5";

const M = window.Meter;

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

function personKey(name) {
  if (name === "你") return "you";
  return people.find(person => person.name === name)?.id || "you";
}

function atOf(time) {
  const matched = String(time).match(/(\d+)\s*月\s*(\d+)\s*日\s*(\d+):(\d+)/);
  if (!matched) return 0;
  const month = matched[1].padStart(2, "0");
  const day = matched[2].padStart(2, "0");
  return Date.parse(`2026-${month}-${day}T${matched[3]}:${matched[4]}:00+08:00`);
}
function sceneAt(sec) { return Date.parse("2026-11-11T21:10:00+08:00") + sec * 1000; }
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
// Keep the original scene and its hand-authored associations in the sample source.
export function demoSource() {
  for (const id of ["z1", "c2", "z2"]) {
    const step = M.works.flatMap(work => work.steps).find(step => step.id === id);
    if (step) step.statusLabel = "已发到群";
  }
  for (const person of people) person.avatar = "/assets/" + person.face + ".webp";
  rooms[0].featured = true;
  rooms[0].blurb = "话、补充，还有没发出来的调用，按时间排在同一条里。";
  return {
    people, rooms, rates: RATES, workById: M.workById, clock: M.clock,
    roomFeed, personFeed,
    origin: "样例停在 11 月 11 日 21:28",
    defaultRoom: "chat",
  };
}

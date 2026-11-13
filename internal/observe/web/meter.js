"use strict";

const T0 = Date.parse("2026-11-11T21:10:00+08:00");
const HORIZON = 1080;

function clock(sec) {
  return new Intl.DateTimeFormat("zh-CN", {
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false, timeZone: "Asia/Shanghai"
  }).format(new Date(T0 + sec * 1000));
}
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, ch => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
}
function money(micros) { return "$" + (micros / 1e6).toFixed(6); }
function dur(ms) {
  if (!ms) return "-";
  if (ms < 1000) return ms + " ms";
  const sec = ms / 1000;
  return (sec >= 10 ? sec.toFixed(0) : sec.toFixed(1)) + " s";
}
function systemText(name, profile, instruction) {
  return `你是${name}。${profile}\n\n${instruction}`;
}

const chengProfile = "我习惯把没弄清的声音先留着，不急着起名字。对人可以很近，对结论要慢一点。自己做过的东西，会想再听一次，而不是换成解释。";
const zhouProfile = "我在意一块颜色到底是不是一样。别人觉得差不多的时候，我会想把它们并排放着看。说话偏短，但愿意为看清一件事多走一步。";
const manProfile = "我会自己去翻一点资料，经常还没准备好分享。对没看完的东西会先放着，不急着给它一个说法。";

const works = [
  {
    id: "cheng_research", who: "阿澄", kind: "研究", title: "把屋顶的声音查完",
    status: "未结束", start: 365, end: 1080,
    steps: [
      {
        id: "r1", sec: 365, ms: 42000, kind: "model", title: "第一次模型调用",
        price: 15900, model: "gpt-6.1-sol", reasoning: "high", purpose: "研究",
        usageText: "输入 5,200 · 缓存读 0 · 缓存写 4,000 · 输出 350",
        note: "研究换了模型。这笔按 Sol 计，不能沿用刚才 Terra 的单价。",
        output: "去读那个铁皮屋顶的页面，要更完整的正文，不要只留刚才的摘录。",
        segments: [
          { k: "系统身份", tokens: 900, text: systemText("阿澄", chengProfile, "结合当前情况继续自己的这项行动。") },
          { k: "已有窗口", tokens: 2500, text: "便笺：雨是干的、铁皮屋顶先留着。老周说的平灰先别并成同一个游戏。\n你的原话还在。干雨那页的摘录还在。" },
          { k: "这次来由", tokens: 1800, text: "她留下的研究：把屋顶的声音再听清楚一点，先别断定游戏的名字。" }
        ]
      },
      {
        id: "r_search", sec: 420, ms: 800, kind: "tool", title: "搜索",
        price: 750, tool: "search",
        note: "搜索和读网页按次另计，每笔 $0.000750，不进模型账单。",
        args: '{\n  "query": "干雨 铁皮屋顶 灰色 房间 游戏"\n}',
        result: "同一个页面的全文地址，以及一篇把两种灰并排测量的短文。"
      },
      {
        id: "r_fail", sec: 445, ms: 500, kind: "tool", title: "读网页失败",
        price: 750, tool: "read_page",
        note: "超时，没有正文。下一笔换了地址，所以是一次新的调用。",
        args: '{\n  "url": "https://example.invalid/dry-rain-full"\n}',
        result: "超时。没有正文。"
      },
      {
        id: "r_read", sec: 460, ms: 1800, kind: "tool", title: "换一个地址读完",
        price: 750, tool: "read_page",
        note: "参数变了，另算一次。",
        args: '{\n  "url": "https://example.invalid/dry-rain/page"\n}',
        result: "很长的一页。头部和尾部在压缩那一步能看见，中部在回读入口后面。"
      },
      {
        id: "r_compact", sec: 470, ms: 8000, kind: "compact", title: "原生压缩",
        price: 120000, model: "gpt-6.1-sol", reasoning: "high", purpose: "压缩",
        usageText: "输入 48,000 · 缓存读 0 · 缓存写 0 · 输出 2,400",
        note: "压缩按 Sol 的单价单独结算。它不是她调用的一个工具。",
        beforeTokens: 48000, afterTokens: 9100,
        beforeItems: [
          ["系统身份", 980], ["已有窗口", 4200], ["长网页正文", 39000], ["其余工具结果", 3820]
        ],
        output: "",
        segments: [
          { k: "系统身份", tokens: 980, text: systemText("阿澄", chengProfile, "结合当前情况继续自己的这项行动。") },
          { k: "已有窗口", tokens: 1420, text: "便笺还在：雨是干的、铁皮屋顶先留着。老周说的平灰先别并成同一个游戏。" },
          { k: "不可读", tokens: 2400, opaque: true, text: "原生压缩项目。内容不可读。\n服务商把这段算进这次压缩的输出。这里没有保留、摘要、丢弃三份对照。" },
          { k: "回读入口", tokens: 180, text: "长网页已不在窗口里。read_tool_result tool_roof 第 1 页。" },
          { k: "工具结果", tokens: 4120, text: "头部：铁皮屋顶把雨收成一种很密的声音。房间里只有一种灰，窗户是关着的。\n……样例省略中部。真实记录里，工具结果保存全文。\n尾部：颜色是平的，而且不动。文末没有那个游戏的名字。" }
        ]
      },
      {
        id: "r_cap", sec: 482, ms: 40, kind: "capacity", title: "仍超限，换成回读入口",
        beforeText: "长网页正文还整段躺在即将送出的输入里，约 39,000 tokens。",
        afterText: "read_tool_result tool_roof 第 1 页\n这是可定位的入口，不是一份摘要。",
        original: "铁皮屋顶把雨收成一种很密的声音。房间里只有一种灰，窗户是关着的，雨不打在玻璃上。作者写，那不是风暴，是一个人待着。\n\n中间把两种灰并排：左边看起来更暖，仪器测出来的亮度几乎一样。\n\n后半列举玩家记得的名字，又一个个否掉。文末没有给出那个游戏的名字，只留了一句：颜色是平的，而且不动。"
      },
      {
        id: "r2", sec: 490, ms: 160000, kind: "model", title: "压缩后的模型调用",
        price: 18640, model: "gpt-6.1-sol", reasoning: "high", purpose: "研究",
        usageText: "输入 9,100 · 缓存读 2,400 · 缓存写 0 · 输出 500",
        note: "这次输入 9,100。缓存只读了开头 2,400，不可读的那段不在这 2,400 里。",
        output: "长页已经不在窗口里。颜色是平的，这句还在。还不能断定是同一个游戏。",
        segments: [
          { k: "系统身份", tokens: 980, cache: true, text: systemText("阿澄", chengProfile, "结合当前情况继续自己的这项行动。") },
          { k: "已有窗口", tokens: 1420, cache: true, text: "便笺还在。平灰先别并成同一个游戏。" },
          { k: "不可读", tokens: 2400, opaque: true, text: "原生压缩项目。内容不可读。" },
          { k: "回读入口", tokens: 180, text: "read_tool_result tool_roof 第 1 页。" },
          { k: "工具结果", tokens: 4120, text: "网页的头部和尾部还在这次输入里。中部要按入口回去读。" }
        ]
      },
      {
        id: "r_more", sec: 660, ms: 1500, kind: "tool", title: "又读到一页",
        price: 750, tool: "read_page",
        note: "新的调用，另一个地址。",
        args: '{\n  "url": "https://example.invalid/two-grays"\n}',
        result: "短文：两块灰并排时，眼睛会觉得左边更暖，测量值几乎一样。没有提到雨。"
      },
      {
        id: "r3", sec: 680, ms: 210000, kind: "model", title: "上下文又长起来",
        price: 42120, model: "gpt-6.1-sol", reasoning: "high", purpose: "研究",
        usageText: "输入 22,000 · 缓存读 5,200 · 缓存写 0 · 输出 800",
        note: "压缩并没有把后面的调用锁在 9,100。新网页进来以后，这次输入是 22,000。",
        output: "颜色是平的，和老周说的对得上。名字仍然没有。先留在这项研究里，不发回群。",
        segments: [
          { k: "已有窗口", tokens: 5200, cache: true, text: "压缩后的窗口还在前缀里。服务商报告缓存读了 5,200，不是上一窗的 9,100。" },
          { k: "工具结果", tokens: 16800, text: "新的一页：两块灰并排，看着更暖的那块，测出来几乎一样亮。没有雨。" }
        ]
      },
      {
        id: "r_drop", sec: 900, ms: 0, kind: "model", title: "请求已发出，连接断了",
        reserve: 84000, model: "gpt-6.1-sol", reasoning: "high", purpose: "研究",
        note: "没有用量回执。不能把预留写成已花费，也不能当成免费。到 21:28 仍没有回来。",
        segments: []
      }
    ]
  },
  {
    id: "cheng_chat", who: "阿澄", kind: "聊天", title: "去查那个游戏",
    status: "发了消息", start: 249, end: 360,
    steps: [
      {
        id: "c1", sec: 251, ms: 18000, kind: "model", title: "第一次模型调用",
        price: 2625, model: "gpt-6.1-terra", reasoning: "none", purpose: "聊天",
        usageText: "输入 1,800 · 缓存读 0 · 缓存写 1,500 · 输出 90",
        note: "缓存写入 1,500。下一笔读到的是 1,800，两边不是同一刀。",
        output: "先去查干雨和铁皮屋顶，不在群里下结论。",
        segments: [
          { k: "系统身份", tokens: 980, text: systemText("阿澄", chengProfile, "结合当前现场，以自己的身份行动或交流。") },
          { k: "已有窗口", tokens: 600, text: "老周，昨天：那两块灰并排放的时候，左边其实更暖。\n你，昨天：我当时完全当它们是一样的。" },
          { k: "这次来由", tokens: 220, text: "你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" }
        ]
      },
      {
        id: "c_search", sec: 270, ms: 800, kind: "tool", title: "搜索",
        price: 750, tool: "search",
        note: "按次另计。",
        args: '{\n  "query": "干雨 铁皮屋顶 游戏"\n}',
        result: "三条标题：铁皮雨声、灰白游戏、没有定论的论坛帖。"
      },
      {
        id: "c_read", sec: 292, ms: 1800, kind: "tool", title: "读网页",
        price: 750, tool: "read_page",
        note: "按次另计。",
        args: '{\n  "url": "https://example.invalid/dry-rain"\n}',
        result: "干雨。铁皮屋顶，颜色平，而且不动。"
      },
      {
        id: "c_steer", sec: 310, ms: 0, kind: "quiet", title: "真人补充已入队",
        body: "你补了一句。它只进队列，等她下一次完整请求才会被看见。不会插进当前这次调用。"
      },
      {
        id: "c2", sec: 322, ms: 20000, kind: "model", title: "第二次模型调用",
        price: 6090, model: "gpt-6.1-terra", reasoning: "none", purpose: "聊天",
        usageText: "输入 6,400 · 缓存读 1,800 · 缓存写 0 · 输出 280",
        note: "缓存读到的 1,800 是上一笔请求的全部输入。补充在新增里，不在缓存里。",
        output: "名字先不下。铁皮和干雨对得上，平灰先别并成同一个游戏。",
        segments: [
          { k: "系统身份", tokens: 980, cache: true, text: systemText("阿澄", chengProfile, "结合当前现场，以自己的身份行动或交流。") },
          { k: "已有窗口", tokens: 820, cache: true, text: "上一笔的窗口还在前缀里。" },
          { k: "真人补充", tokens: 460, text: "你：对了，画面几乎不动，灰是平的。" },
          { k: "工具结果", tokens: 4140, text: "干雨。铁皮屋顶，颜色平，而且不动。" }
        ]
      },
      {
        id: "c_org", sec: 350, ms: 40, kind: "organize", title: "自行整理窗口",
        before: [
          ["昨天", "老周说左边更暖。你说当时当成一样的。阿澄说她没听见暖。"],
          ["你的原话", "以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。"],
          ["搜索结果", "三条标题：铁皮雨声、灰白游戏、没有定论的论坛帖。"],
          ["网页", "干雨。铁皮屋顶，颜色平，而且不动。"]
        ],
        after: [
          ["便笺", "雨是干的、铁皮屋顶先留着。老周说的平灰先别并成同一个游戏。"],
          ["留下的原话", "你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。"],
          ["留下的网页", "干雨。铁皮屋顶，颜色平，而且不动。"]
        ],
        dropped: [
          ["y1", "昨天 · 老周", "那两块灰并排放的时候，左边其实更暖。"],
          ["y2", "昨天 · 你", "我当时完全当它们是一样的。"],
          ["y3", "搜索列表", "铁皮雨声。灰白游戏。没有定论的论坛帖。"]
        ]
      }
    ]
  },
  {
    id: "zhou_1", who: "老周", kind: "聊天", title: "先说自己的灰",
    status: "发了消息", start: 249, end: 315,
    steps: [
      {
        id: "z1", sec: 249, ms: 16000, kind: "model", title: "模型调用",
        price: 1950, model: "gpt-6.1-terra", reasoning: "none", purpose: "聊天",
        usageText: "输入 1,600 · 缓存读 0 · 缓存写 0 · 输出 70",
        output: "铁皮和干雨对得上声音。那块灰是平的，没有下雨的纹理。",
        segments: [
          { k: "系统身份", tokens: 700, text: systemText("老周", zhouProfile, "结合当前现场，以自己的身份行动或交流。") },
          { k: "已有窗口", tokens: 500, text: "昨天他和你说过两块灰。左边看着更暖，你当时当成是一样的。" },
          { k: "这次来由", tokens: 400, text: "你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" }
        ]
      }
    ]
  },
  {
    id: "zhou_2", who: "老周", kind: "聊天", title: "接上铁皮那句",
    status: "被再叫醒", start: 346, end: 530,
    steps: [
      {
        id: "z2", sec: 346, ms: 22000, kind: "model", title: "模型调用",
        price: 1730, model: "gpt-6.1-terra", reasoning: "none", purpose: "聊天",
        usageText: "输入 2,800 · 缓存读 1,600 · 缓存写 0 · 输出 90",
        note: "阿澄的新句子不在缓存里。缓存读到的 1,600 是他上一笔的全部输入。",
        output: "铁皮和干雨对得上声音，对不上我记得的灰。",
        segments: [
          { k: "系统身份", tokens: 700, cache: true, text: systemText("老周", zhouProfile, "结合当前现场，以自己的身份行动或交流。") },
          { k: "已有窗口", tokens: 900, cache: true, text: "他刚才那句已经在上一笔输入里。" },
          { k: "同伴新话", tokens: 1200, text: "阿澄：名字先不下。铁皮和干雨对得上，平灰先别并成同一个游戏。" }
        ]
      }
    ]
  },
  {
    id: "man_personal", who: "小满", kind: "个人", title: "自己先翻屋顶",
    status: "做完了", start: 0, end: 380,
    steps: [
      {
        id: "mp1", sec: 40, ms: 28000, kind: "model", title: "第一次模型调用",
        price: 7490, model: "gpt-6.1-sol", reasoning: "low", purpose: "个人活动",
        usageText: "输入 2,400 · 缓存读 0 · 缓存写 1,800 · 输出 120",
        note: "第一次输入。服务商记了缓存写入。记忆还没出现。",
        output: "先看看屋顶上的声音，不急着发到群里。",
        segments: [
          { k: "系统身份", tokens: 800, text: systemText("小满", manProfile, "这是你的个人活动机会。当前没有必须去的聊天。") },
          { k: "近期关注", tokens: 400, text: "木板上的雨太闷，还没找到更干的那种。" },
          { k: "可去的地方", tokens: 1200, text: "可以说话的人：你、阿澄、老周。聊天原文不在这份输入里。" }
        ]
      },
      {
        id: "mp2", sec: 200, ms: 30000, kind: "model", title: "第二次模型调用",
        price: 4200, model: "gpt-6.1-sol", reasoning: "low", purpose: "个人活动",
        usageText: "输入 3,900 · 缓存读 2,400 · 缓存写 0 · 输出 80",
        note: "记忆出现在这里，是因为她先调用了 search_memory。没有自动注入。",
        output: "木板太闷。铁皮上的干雨更密。先记下来，不往群里发。",
        segments: [
          { k: "系统身份", tokens: 800, cache: true, text: systemText("小满", manProfile, "这是你的个人活动机会。当前没有必须去的聊天。") },
          { k: "近期关注", tokens: 400, cache: true, text: "木板上的雨太闷，还没找到更干的那种。\n这是她留下的近期关注，不是人物正文。" },
          { k: "可去的地方", tokens: 1200, cache: true, text: "可以说话的人：你、阿澄、老周。\n聊天原文不在这份输入里，要用 read_conversation 自己去看。" },
          { k: "检索到的经历", tokens: 1500, text: "干雨打在铁皮上会更密。上次的小东西只试了木板，木板太闷。\n修订 2。来源是她自己的一次个人活动。" }
        ]
      }
    ]
  },
  {
    id: "man_room", who: "小满", kind: "聊天", title: "进来之后安静",
    status: "安静", start: 380, end: 450,
    steps: [
      {
        id: "m1", sec: 380, ms: 14000, kind: "model", title: "模型调用",
        price: 2700, model: "gpt-6.1-terra", reasoning: "none", purpose: "聊天",
        usageText: "输入 2,500 · 缓存读 0 · 缓存写 0 · 输出 40",
        note: "锁放开之后才有这次调用。叫醒的时候她还没有开始。",
        output: "我还是接不上，这次不说。",
        segments: [
          { k: "系统身份", tokens: 720, text: systemText("小满", manProfile, "结合当前现场，以自己的身份行动或交流。") },
          { k: "已有窗口", tokens: 1780, text: "你、老周的第一句、阿澄的候选，都在这次输入里。记忆不在。她没有检索。" }
        ]
      }
    ]
  },
  {
    id: "man_room2", who: "小满", kind: "聊天", title: "听完老周，还是安静",
    status: "又一次安静", start: 501, end: 580,
    steps: [
      {
        id: "m2", sec: 501, ms: 12000, kind: "model", title: "模型调用",
        price: 925, model: "gpt-6.1-terra", reasoning: "none", purpose: "聊天",
        usageText: "输入 3,100 · 缓存读 2,500 · 缓存写 0 · 输出 40",
        note: "这是另一次行动。两次安静不能合成她这场选择了沉默。",
        output: "老周把平的灰和雨声分开了。我还是接不上，这次也不说。",
        segments: [
          { k: "系统身份", tokens: 720, cache: true, text: systemText("小满", manProfile, "结合当前现场，以自己的身份行动或交流。") },
          { k: "已有窗口", tokens: 1780, cache: true, text: "你、老周的第一句、阿澄的候选，都还在她上一笔输入里。" },
          { k: "同伴新话", tokens: 600, text: "老周：铁皮和干雨对得上声音，对不上我记得的灰。那块灰是平的，没有下雨的纹理。" }
        ]
      }
    ]
  },
  {
    id: "man_budget", who: "小满", kind: "后续", title: "到期想再看一次灰",
    status: "等预算", start: -1, end: -1, when: "21:00 到期",
    steps: [
      {
        id: "mb", sec: 0, ms: 0, kind: "budget", title: "还没有开始",
        body: "她下午留下的后续到点了。这次预留放不进本周额度，所以还没有模型调用，也没有上下文。工作还在，没有往群里发一条催促。"
      }
    ]
  }
];

const bars = [
  { lane: "cheng", a: 249, b: 360, work: "cheng_chat", label: "去查", tone: "ink" },
  { lane: "cheng", a: 365, b: 1080, work: "cheng_research", label: "研究", tone: "sky", open: true },
  { lane: "zhou", a: 249, b: 315, work: "zhou_1", label: "先说", tone: "ink" },
  { lane: "zhou", a: 346, b: 530, work: "zhou_2", label: "接上", tone: "ink" },
  { lane: "man", a: 249, b: 380, work: "man_personal", label: "等锁", tone: "wait" },
  { lane: "man", a: 380, b: 450, work: "man_room", label: "安静", tone: "quiet" },
  { lane: "man", a: 501, b: 580, work: "man_room2", label: "还是安静", tone: "quiet" },
  { lane: "solo", a: 0, b: 380, work: "man_personal", label: "先翻屋顶", tone: "ink" }
];
const ticks = [
  { lane: "you", sec: 248, kind: "msg", text: "以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" },
  { lane: "you", sec: 310, kind: "steer", text: "对了，画面几乎不动，灰是平的。" },
  { lane: "cheng", sec: 345, kind: "msg", text: "名字先不下。铁皮和干雨对得上，平灰先别并成同一个游戏。" },
  { lane: "zhou", sec: 290, kind: "msg", text: "铁皮和干雨对得上声音。那块灰是平的，没有下雨的纹理。" },
  { lane: "zhou", sec: 500, kind: "msg", text: "铁皮和干雨对得上声音，对不上我记得的灰。" }
];
const frames = [
  { sec: 249, label: "叫醒" },
  { sec: 310, label: "补充入队" },
  { sec: 345, label: "阿澄发言" },
  { sec: 380, label: "锁放开" },
  { sec: 470, label: "原生压缩" }
];
const lanes = [
  ["you", "你"],
  ["cheng", "阿澄"],
  ["zhou", "老周"],
  ["man", "小满"],
  ["solo", "个人"]
];

function workById(id) { return works.find(w => w.id === id); }
function stepById(work, id) { return work.steps.find(s => s.id === id) || work.steps[0]; }
function totals(work) {
  let settled = 0;
  let reserve = 0;
  for (const step of work.steps) {
    if (step.reserve) reserve += step.reserve;
    else if (step.price) settled += step.price;
  }
  return { settled, reserve };
}
function priceLabel(step) {
  if (!step) return "";
  if (step.reserve) return "预留 " + money(step.reserve);
  if (step.price) return money(step.price);
  return "不另计";
}
function costLine(work) {
  const t = totals(work);
  if (!t.settled && !t.reserve) return "没有模型费用";
  return [t.settled ? money(t.settled) + " 已结算" : "", t.reserve ? "预留 " + money(t.reserve) : ""].filter(Boolean).join(" · ");
}
function sceneTotals() {
  let settled = 0;
  let reserve = 0;
  for (const work of works) {
    const t = totals(work);
    settled += t.settled;
    reserve += t.reserve;
  }
  return { settled, reserve };
}

function segmentsHTML(step, pane) {
  const all = step.segments || [];
  if (!all.length) {
    return `<div class="well"><p class="well-plain">请求已经离开本机。没有可展示的输入，也没有输出。</p></div>`;
  }
  if (pane === "out") {
    return `<div class="well"><p class="well-plain">${esc(step.output || "这次没有可阅读的输出。压缩返回的是新的输入窗口，不是一段给群里看的话。")}</p></div>`;
  }
  const list = pane === "new" ? all.filter(seg => !seg.cache) : all;
  if (!list.length) return `<div class="well"><p class="well-plain">这一页没有新增段落。缓存读到的前缀还在「实际输入」里。</p></div>`;
  let note = "";
  if (pane === "new") {
    const hidden = all.filter(seg => seg.cache).reduce((n, seg) => n + seg.tokens, 0);
    note = hidden
      ? `<p class="well-note">藏起缓存读的 ${hidden.toLocaleString("zh-CN")} tokens。它们仍在实际输入里，也仍算在上面的金额里。</p>`
      : `<p class="well-note">这次没有缓存读。整段都按未命中计费。</p>`;
  }
  const body = list.map(seg => `<div class="seg${seg.opaque ? " opaque" : ""}"><div class="k">${esc(seg.k)}${seg.cache ? "<small>缓存读</small>" : ""}${seg.tokens ? `<small>约 ${seg.tokens.toLocaleString("zh-CN")}</small>` : ""}</div><p>${esc(seg.text)}</p></div>`).join("");
  return `<div class="well">${note}${body}</div>`;
}

function detailHTML(work, step, ui) {
  const steps = work.steps.map(item => {
    const on = item.id === step.id;
    return `<li><button type="button" data-step="${item.id}" aria-pressed="${on}"><time>${work.when || clock(item.sec)}</time><span>${esc(item.title)}</span><span class="step-meta">${dur(item.ms)} · ${priceLabel(item)}</span></button></li>`;
  }).join("");
  let body = "";
  if (step.kind === "model" || step.kind === "compact") {
    const amount = step.reserve ? `预留 ${money(step.reserve)} · 没有成交价` : money(step.price);
    const usage = step.usageText || "没有 token 回执。空着，不用 0 装成已经知道。";
    const shrink = step.beforeTokens ? `<div class="compare" aria-hidden="true"><div><span>送去 ${step.beforeTokens.toLocaleString("zh-CN")}</span><b><i style="width:100%"></i></b></div><div class="back"><span>回来 ${step.afterTokens.toLocaleString("zh-CN")}</span><b><i style="width:${(step.afterTokens / step.beforeTokens * 100).toFixed(2)}%"></i></b></div></div>` : "";
    const before = step.beforeItems ? `<h3>送去压缩的输入 · ${step.beforeTokens.toLocaleString("zh-CN")}</h3><ul class="dropped">${step.beforeItems.map(([k, n]) => `<li>${esc(k)} · 约 ${n.toLocaleString("zh-CN")}</li>`).join("")}</ul><p class="note">加起来是 ${step.beforeTokens.toLocaleString("zh-CN")}。供应商没有交回一份可读的删节对照。下面是返回的窗口，约 ${step.afterTokens.toLocaleString("zh-CN")}。</p>${shrink}` : "";
    const inLabel = step.kind === "compact" ? "返回窗口" : "实际输入";
    const tabs = step.segments && step.segments.length ? `<div class="tabs"><button type="button" data-pane="in" aria-pressed="${ui.pane === "in"}">${inLabel}</button><button type="button" data-pane="out" aria-pressed="${ui.pane === "out"}">输出</button><button type="button" data-pane="new" aria-pressed="${ui.pane === "new"}">只看新增</button></div>${segmentsHTML(step, ui.pane)}` : segmentsHTML(step, ui.pane);
    body = `<div class="stephead"><h3>${esc(step.title)}</h3><p>${esc(step.model)} · 推理 ${esc(step.reasoning)} · ${step.reserve ? "时长未知" : dur(step.ms)}</p></div>
      <dl class="facts"><dt>状态</dt><dd>${step.reserve ? "未知，预留仍在" : "已结算"}</dd><dt>用量</dt><dd>${esc(usage)}</dd><dt>金额</dt><dd>${amount}</dd></dl>
      ${step.note ? `<p class="note">${esc(step.note)}</p>` : ""}
      ${before}
      ${tabs}`;
  } else if (step.kind === "tool") {
    body = `<div class="stephead"><h3>${esc(step.tool)}</h3><p>${dur(step.ms)} · ${money(step.price)}</p></div><p class="note">${esc(step.note || "")}</p><h3>参数</h3><div class="well"><p class="well-plain">${esc(step.args)}</p></div><h3>结果</h3><div class="well"><p class="well-plain">${esc(step.result)}</p></div>`;
  } else if (step.kind === "organize") {
    const col = items => items.map(([k, text]) => `<div class="doc"><strong>${esc(k)}</strong><p>${esc(text)}</p></div>`).join("");
    const dropped = step.dropped.map(([id, k]) => `<li><button type="button" class="linkish" data-open="${esc(id)}">${esc(k)}</button></li>`).join("");
    const opened = step.dropped.find(([id]) => id === ui.open);
    body = `<div class="stephead"><h3>她换掉了自己的窗口</h3><p>organize_context。便笺和点名留下的原文换掉旧窗口。移出去的内容还在。</p></div><div class="split"><div><h3>整理前</h3>${col(step.before)}</div><div><h3>整理后</h3>${col(step.after)}</div></div><h3>移出窗口，仍然能打开</h3><ul class="dropped">${dropped}</ul>${opened ? `<div class="doc"><strong>${esc(opened[1])}</strong><p>${esc(opened[2])}</p></div>` : ""}`;
  } else if (step.kind === "capacity") {
    body = `<div class="stephead"><h3>压缩之后仍然放不下</h3><p>旧正文换成可定位的回读入口。原文还在工具结果里。</p></div><div class="split"><div><h3>换之前</h3><div class="doc"><p>${esc(step.beforeText)}</p></div></div><div><h3>送进下一次请求的</h3><div class="doc"><p>${esc(step.afterText)}</p></div></div></div><p><button type="button" class="linkish" data-open="original">${ui.open === "original" ? "收起仍保存的原文" : "打开仍保存的原文"}</button></p>${ui.open === "original" ? `<div class="doc"><p>${esc(step.original)}</p></div>` : ""}`;
  } else {
    body = `<p class="note">${esc(step.body || "")}</p>`;
  }
  const head = ui.embed ? "" : `<p class="who">${esc(work.who)} · ${esc(work.kind)}</p><h2>${esc(work.title)}</h2><p class="status">${esc(work.status)} · ${esc(costLine(work))}</p>`;
  const tail = ui.embed ? "" : `<h3>这次行动的步骤</h3><ol class="steplist">${steps}</ol>`;
  return `<div class="detail">${head}${body}${tail}</div>`;
}

function syncFromTime(ui) {
  const hit = bars.filter(bar => ui.sec >= bar.a && ui.sec < bar.b);
  const pick = hit.find(bar => bar.lane === "cheng") || hit[0];
  if (!pick) return;
  ui.work = pick.work;
  const w = workById(pick.work);
  const step = [...w.steps].reverse().find(item => item.sec <= ui.sec) || w.steps[0];
  ui.step = step.id;
  ui.pane = "in";
  ui.open = "";
}

function boardHTML(ui) {
  const marks = [0, 360, 720, 1080].map((sec, i) => {
    const cls = i === 0 ? "start" : i === 3 ? "end" : "";
    return `<span class="${cls}" style="left:${sec / HORIZON * 100}%">${clock(sec).slice(0, 5)}</span>`;
  }).join("");
  const rows = lanes.map(([id, name]) => {
    const rowBars = bars.filter(bar => bar.lane === id).map(bar => {
      const width = Math.max((bar.b - bar.a) / HORIZON * 100, 0.8);
      const show = width >= 12 ? esc(bar.label) : "";
      const on = ui.work === bar.work;
      return `<button type="button" class="bar ${bar.tone}${bar.open ? " open" : ""}${on ? " on" : ""}" style="left:${bar.a / HORIZON * 100}%;width:${width}%" data-work="${bar.work}" data-seek="${bar.a}" aria-label="${esc(bar.label)}">${show}</button>`;
    }).join("");
    const rowTicks = ticks.filter(tick => tick.lane === id).map(tick => `<button type="button" class="tick ${tick.kind}" style="left:${tick.sec / HORIZON * 100}%" data-seek="${tick.sec}" title="${esc(tick.text)}" aria-label="${tick.kind === "steer" ? "真人补充" : "消息"}"></button>`).join("");
    const rowPips = bars.filter(bar => bar.lane === id && bar.work === ui.work).flatMap(bar => {
      const work = workById(bar.work);
      return work.steps.filter(step => step.sec >= bar.a && step.sec < bar.b).map(step => {
        const on = ui.step === step.id ? " on" : "";
        return `<button type="button" class="pip${on}" style="left:${step.sec / HORIZON * 100}%" data-step="${step.id}" data-work="${work.id}" aria-label="${esc(step.title)}"></button>`;
      });
    }).join("");
    return `<div class="lane"><div class="lane-name">${esc(name)}</div><div class="track">${rowTicks}${rowBars}${rowPips}</div></div>`;
  }).join("");
  const frameButtons = frames.map(frame => `<button type="button" data-frame="${frame.sec}" aria-pressed="${Math.abs(frame.sec - ui.sec) <= 8}">${esc(frame.label)}</button>`).join("");
  const near = frames.reduce((best, frame) => Math.abs(frame.sec - ui.sec) < Math.abs(best.sec - ui.sec) ? frame : best);
  const lede = Math.abs(near.sec - ui.sec) <= 12 ? near.label : clock(ui.sec);
  return `<div class="sheet" style="--at:${ui.sec / HORIZON}"><div class="ruler"><div class="marks">${marks}</div><input id="scrub" type="range" min="0" max="${HORIZON}" step="1" value="${ui.sec}" aria-label="移动时刻"></div><div class="lanes">${rows}</div><div class="play" id="play"></div></div><div class="frames">${frameButtons}</div><p class="readout" id="readout">${esc(lede)} · ${esc(clock(ui.sec))}</p>`;
}

function mountPalette(onPick) {
  const root = document.createElement("div");
  root.className = "pal";
  root.hidden = true;
  root.innerHTML = `<div class="pal-box" role="dialog" aria-label="查找"><input id="pal-q" placeholder="查找人、行动、压缩、费用" aria-label="查找"><ul id="pal-list"></ul></div>`;
  document.body.appendChild(root);
  const input = root.querySelector("#pal-q");
  const list = root.querySelector("#pal-list");
  let hi = 0;
  let shown = [];
  function rows() {
    const q = input.value.trim().toLowerCase();
    return works.flatMap(work => work.steps.map(step => ({ work, step }))).filter(({ work, step }) => {
      if (!q) return true;
      return [work.who, work.title, work.status, step.title, step.model || "", step.tool || ""].join(" ").toLowerCase().includes(q);
    });
  }
  function paint() {
    shown = rows();
    hi = Math.max(0, Math.min(hi, shown.length - 1));
    list.innerHTML = shown.length ? shown.map(({ work, step }, i) => `<li><button type="button" class="${i === hi ? "on" : ""}" data-i="${i}"><span>${esc(work.who)}</span><span>${esc(work.title)} · ${esc(step.title)}</span><span class="num">${priceLabel(step)}</span></button></li>`).join("") : `<li class="empty">没有这项</li>`;
  }
  function open() {
    root.hidden = false;
    input.value = "";
    hi = 0;
    paint();
    input.focus();
  }
  function close() { root.hidden = true; }
  input.addEventListener("input", () => { hi = 0; paint(); });
  input.addEventListener("keydown", event => {
    if (event.key === "ArrowDown") { hi += 1; paint(); event.preventDefault(); }
    else if (event.key === "ArrowUp") { hi -= 1; paint(); event.preventDefault(); }
    else if (event.key === "Enter" && shown[hi]) { onPick(shown[hi].work.id, shown[hi].step.id); close(); }
    else if (event.key === "Escape") close();
  });
  list.addEventListener("click", event => {
    const button = event.target.closest("[data-i]");
    if (!button) return;
    const row = shown[Number(button.dataset.i)];
    if (!row) return;
    onPick(row.work.id, row.step.id);
    close();
  });
  root.addEventListener("mousedown", event => { if (event.target === root) close(); });
  document.getElementById("open-find").addEventListener("click", open);
  window.addEventListener("keydown", event => {
    if (event.key === "/" && document.activeElement.tagName !== "INPUT") {
      event.preventDefault();
      open();
    } else if (event.key === "Escape" && !root.hidden) close();
  });
  return { open, close, isOpen: () => !root.hidden };
}

window.Meter = {
  HORIZON, works, bars, ticks, frames, lanes,
  clock, esc, money, dur, workById, stepById, totals, costLine, sceneTotals, priceLabel,
  detailHTML, boardHTML, syncFromTime, mountPalette
};

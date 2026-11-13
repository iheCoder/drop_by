"use strict";

const T0 = Date.parse("2026-11-11T21:10:00+08:00");
const HORIZON = 1080;
const AXIS = 52;
const LANE = 72;
const LANES = ["you", "cheng", "zhou", "man"];

const RATE = {
  "gpt-6.1-terra": { input: 1000, cached: 50, write: 1250, output: 5000 },
  "gpt-6.1-sol": { input: 2000, cached: 100, write: 2500, output: 10000 }
};

const PEOPLE = {
  you: { name: "你" },
  cheng: {
    name: "阿澄", rev: 3, revised: "11 月 2 日", how: "你写定「说话可以再慢一点」",
    profile: "我习惯把没弄清的声音先留着，不急着起名字。对人可以很近，对结论要慢一点。自己做过的东西，会想再听一次，而不是换成解释。",
    focus: "干雨打在不同屋顶上，听起来差多少。"
  },
  zhou: {
    name: "老周", rev: 2, revised: "10 月 28 日", how: "创建时生成，之后没有改过",
    profile: "我在意一块颜色到底是不是一样。别人觉得差不多的时候，我会想把它们并排放着看。说话偏短，但愿意为看清一件事多走一步。",
    focus: "两块灰是不是真的一样亮。"
  },
  man: {
    name: "小满", rev: 5, revised: "11 月 8 日", how: "她在对话里明确改过一句，来源是那次原话",
    profile: "我会自己去翻一点资料，经常还没准备好分享。对没看完的东西会先放着，不急着给它一个说法。",
    focus: "木板上的雨太闷，还没找到更干的那种。"
  }
};

const CHAT_TOOLS = ["send_message", "search", "read_page", "organize_context", "search_memory", "read_memory", "read_history", "read_tool_result", "schedule_followup", "cancel_work", "save_artifact"];
const SOLO_TOOLS = ["send_message", "search", "read_page", "search_memory", "read_memory", "read_conversation", "save_artifact", "schedule_followup", "cancel_work"];

const MEMORY = {
  k: "检索到的经历",
  text: "干雨打在铁皮上会更密。上次的小东西只试了木板，木板太闷。\n修订 2。来源是她自己的一次个人活动。正文是她的理解，不是群里的定论。"
};

function systemText(id, instruction) {
  const p = PEOPLE[id];
  return `你是${p.name}。${p.profile}\n\n${instruction}`;
}

function quoteUsage(usage) {
  if (!usage) return null;
  if (usage.provider === "deepseek") return { kind: "settled", micros: 750, reserved: 0 };
  if (usage.status !== "settled") return { kind: usage.status, micros: 0, reserved: usage.reserved || 0 };
  const rate = RATE[usage.model];
  const normal = usage.input - usage.cached - usage.write;
  const nano = normal * rate.input + usage.cached * rate.cached + usage.write * rate.write + usage.output * rate.output;
  return { kind: "settled", micros: Math.ceil(nano / 1000), reserved: 0 };
}

const WORKS = [
  {
    id: "man_personal", who: "man", title: "自己先翻屋顶", kind: "个人活动",
    status: "done", statusLabel: "做完了，没分享", start: 0, end: 380, scene: false,
    when: "21:10 前就开始",
    trigger: "她自己的活动时钟。没有群消息，也没有聊天对象。",
    room: "没有聊天对象",
    also: [{ id: "man_room", label: "锁放开后的群里那段" }],
    runtime: "个人活动用 gpt-6.1-sol，推理 high。记忆不会自动灌进第一次输入。",
    steps: [
      {
        id: "mp1", sec: 40, ms: 90000, kind: "model", title: "第一次模型调用",
        call: {
          model: "gpt-6.1-sol", reasoning: "high", maxOutput: 25000, ms: 90000,
          tools: SOLO_TOOLS,
          cacheNote: "第一次输入。服务商记了缓存写入，不是从群聊里带过来的。",
          usage: { provider: "openai", model: "gpt-6.1-sol", status: "settled", purpose: "个人活动", input: 2400, cached: 0, write: 1800, output: 120 },
          segments: [
            { k: "系统身份", tokens: 800, text: systemText("man", "这是你的个人活动机会。可以从自己的兴趣、经历或新接触的东西出发。当前没有必须去的聊天。") },
            { k: "近期关注", tokens: 400, text: "木板上的雨太闷，还没找到更干的那种。\n这是她留下的近期关注，不是人物正文。" },
            { k: "可去的地方", tokens: 1200, text: "可以说话的人：你、阿澄、老周。\n可以打开的房间：随便聊聊、和小满的私聊。\n聊天原文不在这份输入里，要用 read_conversation 自己去看。" }
          ],
          output: "先找自己记过的屋顶，不急着发给谁。"
        }
      },
      {
        id: "mp_search", sec: 160, ms: 700, kind: "tool", title: "搜索",
        tool: {
          name: "search", status: "succeeded", ms: 700,
          retry: "新的调用。",
          args: "{\n  \"query\": \"干雨 铁皮屋顶 木板 声音\"\n}",
          result: "三条标题：铁皮雨声的采样说明、木板吸音的短文、一个没有名字的灰房间。没有把其中一条直接当成她要的那个游戏。",
          usage: { provider: "deepseek", purpose: "搜索", status: "settled" }
        }
      },
      {
        id: "mp_mem", sec: 200, ms: 30, kind: "tool", title: "检索自己的经历",
        tool: {
          name: "search_memory", status: "succeeded", ms: 30,
          retry: "本地检索，没有另外的模型调用。",
          args: "{\n  \"query\": \"屋顶 雨 木板\"\n}",
          result: MEMORY.text,
          usage: null
        }
      },
      {
        id: "mp2", sec: 220, ms: 80000, kind: "model", title: "第二次模型调用",
        call: {
          model: "gpt-6.1-sol", reasoning: "high", maxOutput: 25000, ms: 80000,
          tools: SOLO_TOOLS,
          cacheNote: "缓存读到的 2,400 是上一笔输入的全部。检索结果是新增的。",
          usage: { provider: "openai", model: "gpt-6.1-sol", status: "settled", purpose: "个人活动", input: 3900, cached: 2400, write: 0, output: 80 },
          segments: [
            { k: "系统身份", tokens: 800, cache: "read", text: systemText("man", "这是你的个人活动机会。可以从自己的兴趣、经历或新接触的东西出发。当前没有必须去的聊天。") },
            { k: "近期关注", tokens: 400, cache: "read", text: "木板上的雨太闷，还没找到更干的那种。\n这是她留下的近期关注，不是人物正文。" },
            { k: "可去的地方", tokens: 1200, cache: "read", text: "可以说话的人：你、阿澄、老周。\n可以打开的房间：随便聊聊、和小满的私聊。\n聊天原文不在这份输入里，要用 read_conversation 自己去看。" },
            { k: "检索到的经历", tokens: 1500, text: MEMORY.text + "\n\n这是工具结果。没有另一份「实际使用了哪些记忆」的名单；它出现在这次请求里，才算被这次调用看见。" }
          ],
          output: "木板那次太闷。这次先留在自己这里，不往群里发。"
        }
      },
      { id: "mp_end", sec: 370, ms: 0, kind: "end", title: "个人活动结束", quiet: true, body: "没有 send_message。参与记录会写成 quiet。锁在这一刻放开，群里那场才开始叫到她的模型。" }
    ]
  },
  {
    id: "cheng_chat", who: "cheng", title: "去查那个游戏", kind: "聊天",
    status: "done", statusLabel: "发了消息，并留下研究", start: 249, end: 360,
    trigger: "你 21:14:08 的消息。同一次叫醒还有老周；小满被叫到了，但要等锁。",
    room: "随便聊聊",
    also: [{ id: "zhou_1", label: "老周同时开始的那段" }, { id: "cheng_research", label: "她随后留下的研究" }],
    runtime: "聊天用 gpt-6.1-terra，推理 none，store false。steering 开着，但只接真人的补充。",
    steps: [
      {
        id: "c1", sec: 251, ms: 18000, kind: "model", title: "第一次模型调用",
        call: {
          model: "gpt-6.1-terra", reasoning: "none", maxOutput: 8000, ms: 18000,
          tools: CHAT_TOOLS,
          cacheNote: "这笔输入里有 1,500 记成缓存写入。下一次缓存读到的是 1,800，两边不是同一个数。",
          usage: { provider: "openai", model: "gpt-6.1-terra", status: "settled", purpose: "聊天", input: 1800, cached: 0, write: 1500, output: 90 },
          segments: [
            { k: "系统身份", tokens: 980, text: systemText("cheng", "结合当前现场，以自己的身份行动或交流。") },
            { k: "已有窗口", tokens: 600, text: "老周，昨天：那两块灰并排放的时候，左边其实更暖。\n你，昨天：我当时完全当它们是一样的。\n阿澄，昨天：暖这一点，我倒是没听见。下次想看你怎么并排。" },
            { k: "这次来由", tokens: 220, text: "新的消息。这条来由只交付一次。\n你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" }
          ],
          output: "先去查这种屋顶上的雨。还不确定是哪一个，先不在群里说死。"
        }
      },
      {
        id: "c_search", sec: 270, ms: 800, kind: "tool", title: "搜索",
        tool: {
          name: "search", status: "succeeded", ms: 800, retry: "新的调用。",
          args: "{\n  \"query\": \"屋顶 雨声 灰色 安静 游戏\"\n}",
          result: "1. 铁皮屋顶的雨声采样，提到雨点干、密。\n2. 一个灰白画面的独立游戏介绍，主色是两块看起来相近的灰。\n3. 论坛帖：有人在找「下雨但屋里很静」的游戏，没有定论。",
          usage: { provider: "deepseek", purpose: "搜索", status: "settled" }
        }
      },
      {
        id: "c_read", sec: 292, ms: 18000, kind: "tool", title: "读网页",
        tool: {
          name: "read_page", status: "succeeded", ms: 18000, retry: "新的调用。",
          args: "{\n  \"url\": \"https://example.invalid/dry-rain\"\n}",
          result: "标题：干雨。\n铁皮屋顶把雨收成一种很密的声音，画面故意只留灰。作者写，那不是风暴，是一个人待在屋里。文中没有给出那个名字，只说颜色「平，而且不动」。",
          usage: { provider: "deepseek", purpose: "读网页", status: "settled" }
        }
      },
      {
        id: "c_steer", sec: 310, ms: 0, kind: "note", title: "真人补充已入队",
        body: "你的这句已经 accepted，只表示进了队列。阿澄当时的连接还在读网页，这句话不在那一次请求里。下一次完整的模型调用才会看见它。老周发出的消息不会走这条 steering。"
      },
      {
        id: "c2", sec: 322, ms: 20000, kind: "model", title: "第二次模型调用",
        call: {
          model: "gpt-6.1-terra", reasoning: "none", maxOutput: 8000, ms: 20000,
          tools: CHAT_TOOLS,
          cacheNote: "缓存读到的 1,800 是上一笔请求的全部输入。上一笔的缓存写入是 1,500，两笔账不能拼成同一刀。",
          usage: { provider: "openai", model: "gpt-6.1-terra", status: "settled", purpose: "聊天", input: 6400, cached: 1800, write: 0, output: 280 },
          segments: [
            { k: "系统身份", tokens: 980, cache: "read", text: systemText("cheng", "结合当前现场，以自己的身份行动或交流。") },
            { k: "已有窗口", tokens: 600, cache: "read", text: "老周，昨天：那两块灰并排放的时候，左边其实更暖。\n你，昨天：我当时完全当它们是一样的。\n阿澄，昨天：暖这一点，我倒是没听见。下次想看你怎么并排。" },
            { k: "这次来由", tokens: 220, cache: "read", text: "新的消息。这条来由只交付一次。\n你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" },
            { k: "真人补充", tokens: 200, text: "啊，不是恐怖的。比较安静，像一个人在屋里。\n\n这句从队列进入这次完整请求。它不是系统替她做的判断。" },
            { k: "工具结果", tokens: 1900, text: "search 的三条结果还在。铁皮雨声、灰白游戏介绍、一个没有定论的论坛帖。" },
            { k: "工具结果", tokens: 2500, text: "read_page：干雨。铁皮屋顶，雨是干的，画面几乎没有颜色。作者写那不是风暴，是一个人待在屋里。颜色「平，而且不动」。" }
          ],
          output: "我听到的可能不是同一个。有个页面写屋顶是铁皮，雨是干的，画面确实几乎没有颜色。先放在这里，我再往下听。不像恐怖，更像屋里只有一个人。"
        }
      },
      {
        id: "c_send", sec: 345, ms: 40, kind: "tool", title: "发出消息",
        tool: {
          name: "send_message", status: "succeeded", ms: 40,
          retry: "同一次 call id 如果恢复，会取回这条回执，不会再发一遍。这次是新的调用。",
          args: "{\n  \"conversation_id\": \"随便聊聊\",\n  \"text\": \"我听到的可能不是同一个。有个页面写屋顶是铁皮，雨是干的……\"\n}",
          result: "已发布 msg_cheng。老周会被这句再叫醒。阿澄自己不会被自己的消息叫醒。",
          usage: null
        }
      },
      {
        id: "c_org", sec: 350, ms: 40, kind: "organize", title: "自行整理窗口",
        before: [
          { k: "昨天的三句", text: "老周说左边更暖。你说当时当成一样的。阿澄说她没听见暖。" },
          { k: "你的原话", text: "以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" },
          { k: "搜索结果", text: "三条标题：铁皮雨声、灰白游戏、没有定论的论坛帖。" },
          { k: "网页", text: "干雨。铁皮屋顶，颜色平，而且不动。" }
        ],
        after: [
          { k: "便笺", text: "雨是干的、铁皮屋顶先留着。老周说的平灰先别并成同一个游戏。" },
          { k: "留下的原话", text: "你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" },
          { k: "留下的网页", text: "干雨。铁皮屋顶，颜色平，而且不动。" }
        ],
        dropped: [
          { id: "y1", k: "昨天 · 老周", text: "那两块灰并排放的时候，左边其实更暖。" },
          { id: "y2", k: "昨天 · 你", text: "我当时完全当它们是一样的。" },
          { id: "y3", k: "搜索列表", text: "铁皮雨声、灰白游戏介绍、论坛帖。完整结果仍可由 read_tool_result 打开。" }
        ]
      },
      { id: "c_end", sec: 358, ms: 0, kind: "end", title: "这次行动结束", body: "参与记录会写成 act：发过消息，也用过工具。她同时留下了一项研究。研究是下一项工作，不是这段行动的尾声。" }
    ]
  },
  {
    id: "zhou_1", who: "zhou", title: "先说自己的灰", kind: "聊天",
    status: "done", statusLabel: "发了消息", start: 249, end: 315,
    trigger: "和阿澄同一次被你的消息叫醒。他没有等阿澄查完。",
    room: "随便聊聊",
    also: [{ id: "cheng_chat", label: "同时在查的阿澄" }],
    runtime: "gpt-6.1-terra，推理 none。这次没有搜索。",
    steps: [
      {
        id: "z1", sec: 251, ms: 35000, kind: "model", title: "模型调用",
        call: {
          model: "gpt-6.1-terra", reasoning: "none", maxOutput: 8000, ms: 35000,
          tools: CHAT_TOOLS,
          cacheNote: "",
          usage: { provider: "openai", model: "gpt-6.1-terra", status: "settled", purpose: "聊天", input: 1600, cached: 0, write: 0, output: 70 },
          segments: [
            { k: "系统身份", tokens: 700, text: systemText("zhou", "结合当前现场，以自己的身份行动或交流。") },
            { k: "已有窗口", tokens: 500, text: "昨天他和你说过两块灰。左边看着更暖，你当时当成是一样的。" },
            { k: "这次来由", tokens: 400, text: "你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" }
          ],
          output: "如果是灰，而且几乎不动，我会先怀疑那是两块亮度不一样的灰，不是天气。名字我还没想到。"
        }
      },
      {
        id: "z1_send", sec: 290, ms: 30, kind: "tool", title: "发出消息",
        tool: {
          name: "send_message", status: "succeeded", ms: 30, retry: "新的调用。",
          args: "{\n  \"text\": \"如果是灰，而且几乎不动，我会先怀疑那是两块亮度不一样的灰，不是天气。\"\n}",
          result: "已发布。阿澄还在自己的行动里，这句会在她下一次请求前作为同伴的新话交付，不会变成 steering。",
          usage: null
        }
      },
      { id: "z1_end", sec: 308, ms: 0, kind: "end", title: "这次行动结束", body: "参与记录写成 act。他先说完了，不等阿澄的网页。" }
    ]
  },
  {
    id: "zhou_2", who: "zhou", title: "接上铁皮那句", kind: "聊天",
    status: "done", statusLabel: "被阿澄的消息再叫醒", start: 346, end: 530,
    trigger: "阿澄 21:15:45 发出的消息。不是上一场行动的续集，是一次新的行动。",
    room: "随便聊聊",
    also: [{ id: "cheng_chat", label: "叫醒他的那次发送" }],
    runtime: "gpt-6.1-terra。缓存读的是他自己上一笔输入，不是阿澄的上下文。",
    steps: [
      { id: "z2_wake", sec: 346, ms: 0, kind: "note", title: "被阿澄的消息叫醒", body: "他的上一段已经结束。新消息是别人说的，所以再开一次。他不用等阿澄的研究。" },
      {
        id: "z2", sec: 360, ms: 120000, kind: "model", title: "模型调用",
        call: {
          model: "gpt-6.1-terra", reasoning: "none", maxOutput: 8000, ms: 120000,
          tools: CHAT_TOOLS,
          cacheNote: "缓存读到的 1,600 是他上一笔调用的全部输入。阿澄的新句子不在缓存里。",
          usage: { provider: "openai", model: "gpt-6.1-terra", status: "settled", purpose: "聊天", input: 2800, cached: 1600, write: 0, output: 90 },
          segments: [
            { k: "系统身份", tokens: 700, cache: "read", text: systemText("zhou", "结合当前现场，以自己的身份行动或交流。") },
            { k: "已有窗口", tokens: 500, cache: "read", text: "昨天他和你说过两块灰。左边看着更暖，你当时当成是一样的。" },
            { k: "这次来由", tokens: 400, cache: "read", text: "你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" },
            { k: "同伴新话", tokens: 1200, text: "阿澄：我听到的可能不是同一个。有个页面写屋顶是铁皮，雨是干的，画面确实几乎没有颜色。先放在这里，我再往下听。不像恐怖，更像屋里只有一个人。" }
          ],
          output: "铁皮和干雨对得上声音，对不上我记得的灰。那块灰是平的，没有下雨的纹理。"
        }
      },
      {
        id: "z2_send", sec: 500, ms: 30, kind: "tool", title: "发出消息",
        tool: {
          name: "send_message", status: "succeeded", ms: 30, retry: "新的调用。",
          args: "{\n  \"text\": \"铁皮和干雨对得上声音，对不上我记得的灰。那块灰是平的，没有下雨的纹理。\"\n}",
          result: "已发布。小满的第一段已经结束，这句会再叫醒她。",
          usage: null
        }
      },
      { id: "z2_end", sec: 520, ms: 0, kind: "end", title: "这次行动结束", body: "又是一次独立的行动。费用记在老周名下，不并进阿澄的研究。" }
    ]
  },
  {
    id: "man_room", who: "man", title: "进来之后安静", kind: "聊天",
    status: "done", statusLabel: "安静", start: 380, end: 450,
    trigger: "21:14:09 就被叫到。锁在个人活动上，直到 21:16:20 才开始这次模型调用。",
    room: "随便聊聊",
    also: [{ id: "man_personal", label: "占着锁的个人活动" }],
    runtime: "gpt-6.1-terra。开始时，你的话、老周的第一句、阿澄的候选都已经在房间里。",
    steps: [
      {
        id: "mr1", sec: 382, ms: 60000, kind: "model", title: "模型调用",
        call: {
          model: "gpt-6.1-terra", reasoning: "none", maxOutput: 8000, ms: 60000,
          tools: CHAT_TOOLS,
          cacheNote: "",
          usage: { provider: "openai", model: "gpt-6.1-terra", status: "settled", purpose: "聊天", input: 2500, cached: 0, write: 0, output: 40 },
          segments: [
            { k: "系统身份", tokens: 720, text: systemText("man", "结合当前现场，以自己的身份行动或交流。") },
            { k: "已有窗口", tokens: 1400, text: "你：以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。\n老周：如果是灰，而且几乎不动，我会先怀疑那是两块亮度不一样的灰，不是天气。\n阿澄：有个页面写屋顶是铁皮，雨是干的，画面几乎没有颜色。不像恐怖，更像屋里只有一个人。" },
            { k: "这次来由", tokens: 380, text: "锁放开后的第一次交付。来由仍然是你那句忘了名字的游戏，不是一条新的系统指令。" }
          ],
          output: "阿澄已经去查声音了，老周在说两块灰。我这边没有要补的，先不说话。"
        }
      },
      { id: "mr1_end", sec: 448, ms: 0, kind: "end", title: "没有发消息", quiet: true, body: "参与记录写成 quiet。这是她进来之后的选择。21:14 到 21:16 之间的那段斜线不是这个选择，那时模型还没开始。" }
    ]
  },
  {
    id: "man_room_2", who: "man", title: "听完老周，还是安静", kind: "聊天",
    status: "done", statusLabel: "又被叫醒，仍安静", start: 501, end: 580,
    trigger: "老周 21:18:20 的新消息。上一段安静已经结束，所以这是另一次行动。",
    room: "随便聊聊",
    also: [{ id: "man_room", label: "前一段安静" }, { id: "zhou_2", label: "叫醒她的老周" }],
    runtime: "gpt-6.1-terra。缓存读到她上一笔 2,500，老周的新句子是新增。",
    steps: [
      { id: "mr2_wake", sec: 501, ms: 0, kind: "note", title: "被老周的新话叫醒", body: "安静结束并不关上这个人。房间里出现她还没看见的消息，就会再开始一次。" },
      {
        id: "mr2", sec: 510, ms: 50000, kind: "model", title: "模型调用",
        call: {
          model: "gpt-6.1-terra", reasoning: "none", maxOutput: 8000, ms: 50000,
          tools: CHAT_TOOLS,
          cacheNote: "缓存读到的 2,500 等于她上一笔的全部输入。",
          usage: { provider: "openai", model: "gpt-6.1-terra", status: "settled", purpose: "聊天", input: 3100, cached: 2500, write: 0, output: 40 },
          segments: [
            { k: "系统身份", tokens: 720, cache: "read", text: systemText("man", "结合当前现场，以自己的身份行动或交流。") },
            { k: "已有窗口", tokens: 1400, cache: "read", text: "你、老周的第一句、阿澄的候选，都还在她上一笔输入里。" },
            { k: "这次来由", tokens: 380, cache: "read", text: "上一笔的来由仍在缓存前缀里。" },
            { k: "同伴新话", tokens: 600, text: "老周：铁皮和干雨对得上声音，对不上我记得的灰。那块灰是平的，没有下雨的纹理。" }
          ],
          output: "老周把平的灰和雨声分开了。我还是接不上，这次也不说。"
        }
      },
      { id: "mr2_end", sec: 570, ms: 0, kind: "end", title: "又一次没有发消息", quiet: true, body: "两次 quiet 是两次行动。不能合成「小满这场选择了沉默」。" }
    ]
  },
  {
    id: "cheng_research", who: "cheng", title: "把屋顶的声音查完", kind: "研究",
    status: "unknown", statusLabel: "未结束 · 费用未知", start: 365, end: 1080,
    trigger: "上一段行动里她自己预约的。研究归她，不占住整群。",
    room: "随便聊聊",
    also: [{ id: "cheng_chat", label: "预约它的那次聊天" }],
    runtime: "研究用 gpt-6.1-sol，推理 high，最大输出 25000。压缩发生在下一次请求之前。",
    steps: [
      {
        id: "r1", sec: 365, ms: 42000, kind: "model", title: "第一次模型调用",
        call: {
          model: "gpt-6.1-sol", reasoning: "high", maxOutput: 25000, ms: 42000,
          tools: CHAT_TOOLS,
          cacheNote: "研究换了模型。不能按刚才 Terra 的单价理解这笔。",
          usage: { provider: "openai", model: "gpt-6.1-sol", status: "settled", purpose: "研究", input: 5200, cached: 0, write: 4000, output: 350 },
          segments: [
            { k: "系统身份", tokens: 900, text: systemText("cheng", "结合当前情况继续自己的这项行动。") },
            { k: "已有窗口", tokens: 2500, text: "便笺：雨是干的、铁皮屋顶先留着。老周说的平灰先别并成同一个游戏。\n你的原话还在。干雨那页的摘录还在。" },
            { k: "这次来由", tokens: 1800, text: "她留下的研究：把屋顶的声音再听清楚一点，先别断定游戏的名字。" }
          ],
          output: "去读那个铁皮屋顶的页面，要更完整的正文，不要只留刚才的摘录。"
        }
      },
      {
        id: "r_search", sec: 420, ms: 800, kind: "tool", title: "搜索",
        tool: {
          name: "search", status: "succeeded", ms: 800, retry: "新的调用。",
          args: "{\n  \"query\": \"干雨 铁皮屋顶 灰色 房间 游戏\"\n}",
          result: "同一个页面的全文地址，以及一篇把两种灰并排测量的短文。",
          usage: { provider: "deepseek", purpose: "搜索", status: "settled" }
        }
      },
      {
        id: "r_fail", sec: 445, ms: 500, kind: "tool", title: "读网页失败",
        tool: {
          name: "read_page", status: "failed", ms: 500,
          retry: "请求发出去了，超时，没有正文。这是一次新的调用。接下来换了地址，所以不是同一个 tool_call 取回原结果，也不算「相同参数连续失败」。",
          args: "{\n  \"url\": \"https://example.invalid/dry-rain-full\"\n}",
          result: "超时。没有正文。",
          usage: { provider: "deepseek", purpose: "读网页", status: "settled" }
        }
      },
      {
        id: "r_read", sec: 460, ms: 1800, kind: "tool", title: "换一个地址读完",
        tool: {
          name: "read_page", status: "succeeded", ms: 1800, retry: "参数变了，另算一次。",
          args: "{\n  \"url\": \"https://example.invalid/dry-rain/page\"\n}",
          result: "很长的一页。头部和尾部在压缩那一步里能看见，中部在回读入口后面。",
          usage: { provider: "deepseek", purpose: "读网页", status: "settled" }
        }
      },
      {
        id: "r_compact", sec: 470, ms: 8000, kind: "compact", title: "原生压缩",
        beforeTokens: 48000, afterTokens: 9100,
        beforeItems: [
          { k: "系统身份", tokens: 980 },
          { k: "已有窗口", tokens: 4200 },
          { k: "长网页正文", tokens: 39000 },
          { k: "其余工具结果", tokens: 3820 }
        ],
        call: {
          model: "gpt-6.1-sol", reasoning: "high", maxOutput: 25000, ms: 8000,
          tools: CHAT_TOOLS,
          cacheNote: "压缩按 Sol 的单价单独结算。它不是她调用的一个工具。",
          usage: { provider: "openai", model: "gpt-6.1-sol", status: "settled", purpose: "压缩", input: 48000, cached: 0, write: 0, output: 2400 },
          segments: [
            { k: "系统身份", tokens: 980, text: systemText("cheng", "结合当前情况继续自己的这项行动。") },
            { k: "已有窗口", tokens: 1420, text: "便笺还在：雨是干的、铁皮屋顶先留着。老周说的平灰先别并成同一个游戏。" },
            { k: "不可读", tokens: 2400, opaque: true, text: "原生压缩项目。内容不可读。\n服务商把这段算进这次压缩的输出。这里没有「保留 / 摘要 / 丢弃」三份对照。" },
            { k: "回读入口", tokens: 180, text: "长网页已不在窗口里。read_tool_result tool_roof 第 1 页。" },
            { k: "工具结果", tokens: 4120, text: "头部：铁皮屋顶把雨收成一种很密的声音。房间里只有一种灰，窗户是关着的。\n……样例省略中部。真实记录里，工具结果保存全文。\n尾部：颜色是平的，而且不动。文末没有那个游戏的名字。" }
          ],
          output: ""
        }
      },
      {
        id: "r_cap", sec: 482, ms: 40, kind: "capacity", title: "仍超限，换成回读入口",
        beforeText: "长网页正文还整段躺在即将送出的输入里，约 39,000 tokens。",
        afterText: "read_tool_result tool_roof 第 1 页\n这是可定位的入口，不是一份摘要。",
        original: "铁皮屋顶把雨收成一种很密的声音。房间里只有一种灰，窗户是关着的，雨不打在玻璃上。作者写，那不是风暴，是一个人待着。\n\n中间把两种灰并排：左边看起来更暖，仪器测出来的亮度几乎一样。\n\n后半列举玩家记得的名字，又一个个否掉。文末没有给出那个游戏的名字，只留了一句：颜色是平的，而且不动。\n\n样例只保留这一小段原文。真实的那一页会整段留在工具结果里，不因为移出窗口就删掉。"
      },
      {
        id: "r2", sec: 490, ms: 160000, kind: "model", title: "压缩后的模型调用",
        call: {
          model: "gpt-6.1-sol", reasoning: "high", maxOutput: 25000, ms: 160000,
          tools: CHAT_TOOLS,
          cacheNote: "这次输入 9,100。缓存只读了开头 2,400，不可读的那段不在这 2,400 里。",
          usage: { provider: "openai", model: "gpt-6.1-sol", status: "settled", purpose: "研究", input: 9100, cached: 2400, write: 0, output: 500 },
          segments: [
            { k: "系统身份", tokens: 980, cache: "read", text: systemText("cheng", "结合当前情况继续自己的这项行动。") },
            { k: "已有窗口", tokens: 1420, cache: "read", text: "便笺还在。平灰先别并成同一个游戏。" },
            { k: "不可读", tokens: 2400, opaque: true, text: "原生压缩项目。内容不可读。" },
            { k: "回读入口", tokens: 180, text: "read_tool_result tool_roof 第 1 页。" },
            { k: "工具结果", tokens: 4120, text: "网页的头部和尾部还在这次输入里。中部要按入口回去读。" }
          ],
          output: "长页已经不在窗口里。颜色是平的，这句还在。还不能断定是同一个游戏。"
        }
      },
      {
        id: "r_more", sec: 660, ms: 1500, kind: "tool", title: "又读到一页",
        tool: {
          name: "read_page", status: "succeeded", ms: 1500, retry: "新的调用，另一个地址。",
          args: "{\n  \"url\": \"https://example.invalid/two-grays\"\n}",
          result: "短文：两块灰并排时，眼睛会觉得左边更暖，测量值几乎一样。没有提到雨。",
          usage: { provider: "deepseek", purpose: "读网页", status: "settled" }
        }
      },
      {
        id: "r3", sec: 680, ms: 210000, kind: "model", title: "上下文又长起来",
        call: {
          model: "gpt-6.1-sol", reasoning: "high", maxOutput: 25000, ms: 210000,
          tools: CHAT_TOOLS,
          cacheNote: "压缩并没有把后面的调用锁在 9,100。新网页进来以后，这次输入是 22,000。缓存读了 5,200，不是上一窗的全部。",
          usage: { provider: "openai", model: "gpt-6.1-sol", status: "settled", purpose: "研究", input: 22000, cached: 5200, write: 0, output: 800 },
          segments: [
            { k: "已有窗口", tokens: 5200, cache: "read", text: "压缩后的窗口还在前缀里。服务商报告缓存读了 5,200，不是上一窗的 9,100。" },
            { k: "工具结果", tokens: 16800, text: "新的一页：两块灰并排，看着更暖的那块，测出来几乎一样亮。没有雨。" }
          ],
          output: "颜色是平的，和老周说的对得上；名字仍然没有。先留在这项研究里，不发回群。"
        }
      },
      {
        id: "r_drop", sec: 900, ms: 0, kind: "model", title: "请求已发出，连接断了",
        call: {
          model: "gpt-6.1-sol", reasoning: "high", maxOutput: 25000, ms: 0,
          tools: CHAT_TOOLS,
          cacheNote: "没有用量回执。不能把预留写成已花费，也不能当成免费。",
          usage: { provider: "openai", model: "gpt-6.1-sol", status: "unknown", purpose: "研究", reserved: 84000 },
          segments: [],
          output: ""
        }
      }
    ]
  },
  {
    id: "man_budget", who: "man", title: "到期想再看一次灰", kind: "后续",
    status: "budget", statusLabel: "等预算", scene: false, start: -1,
    when: "21:00 到期",
    trigger: "她下午留下的后续。到点时，这次预留放不进本周额度。",
    room: "随便聊聊",
    also: [],
    runtime: "还没有模型调用，所以没有上下文可看。",
    note: "工作还在。状态是等预算，不是失败，也没有往群里发一条催促。额度恢复之后才会真正开始。",
    steps: []
  }
];

const BARS = [
  { lane: "cheng", work: "cheng_chat", start: 249, end: 360, label: "去查", tone: "" },
  { lane: "zhou", work: "zhou_1", start: 249, end: 315, label: "先说灰", tone: "" },
  { lane: "man", start: 249, end: 380, label: "等锁", tone: "wait", seek: 249 },
  { lane: "zhou", work: "zhou_2", start: 346, end: 530, label: "接上那句", tone: "" },
  { lane: "man", work: "man_room", start: 380, end: 450, label: "安静", tone: "quiet" },
  { lane: "man", work: "man_room_2", start: 501, end: 580, label: "还是安静", tone: "quiet" },
  { lane: "cheng", work: "cheng_research", start: 365, end: 1080, label: "研究还开着", tone: "open" }
];

const TICKS = [
  { lane: "you", sec: 248, kind: "message", text: "以前玩过一个游戏，下雨打在屋顶上，画面几乎是灰的。名字忘了。" },
  { lane: "zhou", sec: 290, kind: "message", text: "如果是灰，而且几乎不动，我会先怀疑那是两块亮度不一样的灰，不是天气。" },
  { lane: "you", sec: 310, kind: "steer", text: "啊，不是恐怖的。比较安静，像一个人在屋里。" },
  { lane: "cheng", sec: 345, kind: "message", text: "我听到的可能不是同一个。有个页面写屋顶是铁皮，雨是干的……不像恐怖，更像屋里只有一个人。" },
  { lane: "zhou", sec: 500, kind: "message", text: "铁皮和干雨对得上声音，对不上我记得的灰。那块灰是平的，没有下雨的纹理。" }
];

const LINKS = [
  { from: { lane: "you", sec: 248 }, to: { lane: "cheng", sec: 249 } },
  { from: { lane: "you", sec: 248 }, to: { lane: "zhou", sec: 249 } },
  { from: { lane: "you", sec: 248 }, to: { lane: "man", sec: 249 } },
  { from: { lane: "cheng", sec: 345 }, to: { lane: "zhou", sec: 346 } },
  { from: { lane: "zhou", sec: 500 }, to: { lane: "man", sec: 501 } }
];

const FRAMES = [
  { sec: 249, label: "叫醒", lede: "老周和阿澄同时开始。小满也被叫到了，但她的锁还在个人活动上，所以这场还没有她的模型调用。" },
  { sec: 310, label: "补充入队", lede: "你的补充只是入队。阿澄正在读网页，这句话要等下一次完整请求才进上下文。老周的第一句已经发出去了。" },
  { sec: 345, label: "阿澄发言", lede: "阿澄把候选发进群里。老周会因此再开一次行动，不用等她的研究结束。小满还在等锁，这句会留到她真正开始的时候。" },
  { sec: 380, label: "锁放开", lede: "小满的模型调用从这里才开始。上下文里已经有你的话、老周的第一句和阿澄的候选。斜线的那一段不是安静。" },
  { sec: 470, label: "原生压缩", lede: "这一跳来自 Sol 的原生压缩。返回的窗口里有不可读的项目，没有保留、摘要、丢弃三份对照。" }
];

const state = { sec: 249, query: "", pane: "in", opened: "" };
let seenStep = "";

function clamp(n) { return Math.max(0, Math.min(HORIZON, n)); }
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
  if (!ms) return "—";
  if (ms < 1000) return ms + " ms";
  const sec = ms / 1000;
  return (sec >= 10 ? sec.toFixed(0) : sec.toFixed(1)) + " s";
}
function workById(id) { return WORKS.find(w => w.id === id); }
function usageOf(step) {
  if (step.call) return step.call.usage;
  if (step.tool) return step.tool.usage || null;
  return null;
}
function workTotals(work) {
  let settled = 0;
  let reserved = 0;
  for (const step of work.steps) {
    const priced = quoteUsage(usageOf(step));
    if (!priced) continue;
    if (priced.kind === "settled") settled += priced.micros;
    else reserved += priced.reserved;
  }
  return { settled, reserved };
}
function charges() {
  const rows = [];
  for (const work of WORKS) {
    for (const step of work.steps) {
      const usage = usageOf(step);
      if (!usage) continue;
      rows.push({ work, step, usage, priced: quoteUsage(usage) });
    }
  }
  rows.sort((a, b) => a.step.sec - b.step.sec);
  return rows;
}
function parseRoute() {
  const raw = (location.hash || "#scene").replace(/^#/, "");
  const [path, query] = raw.split("?");
  const params = new URLSearchParams(query || "");
  const [name, a, b] = path.split("/");
  return { name: name || "scene", a, b, t: params.get("t"), f: params.get("f") || "all" };
}
function visibleWorks() {
  const q = state.query.trim().toLowerCase();
  return WORKS.filter(work => {
    if (!q) return true;
    const who = PEOPLE[work.who].name;
    return [who, work.title, work.statusLabel, work.kind, work.room].join(" ").toLowerCase().includes(q);
  }).slice().sort((a, b) => {
    const rank = w => w.status === "unknown" ? 0 : w.status === "budget" ? 1 : 2;
    return rank(a) - rank(b) || (b.start ?? -1) - (a.start ?? -1);
  });
}

function renderRail(route) {
  const items = visibleWorks();
  const html = items.length ? items.map(work => {
    const totals = workTotals(work);
    const cost = [
      totals.settled ? money(totals.settled) : "",
      totals.reserved ? `预留 ${money(totals.reserved)}` : ""
    ].filter(Boolean).join(" · ") || "没有模型费用";
    const when = work.when || clock(work.start);
    const current = route.name === "work" && route.a === work.id;
    return `<a class="rail-item" href="#work/${work.id}"${current ? ` aria-current="true"` : ""}><span class="rail-title">${esc(PEOPLE[work.who].name)} · ${esc(work.title)}</span><span class="rail-meta">${esc(when)} · ${esc(work.statusLabel)} · ${esc(cost)}</span></a>`;
  }).join("") : `<p class="rail-empty">没有这项行动。</p>`;
  document.getElementById("rail").innerHTML = `<p class="rail-label">行动 · ${items.length}</p>${html}`;
}

function render() {
  const route = parseRoute();
  if (route.name === "scene" && route.t != null && route.t !== "") state.sec = clamp(Number(route.t));
  const stepKey = `${route.name}/${route.a || ""}/${route.b || ""}`;
  if (stepKey !== seenStep) {
    state.pane = "in";
    state.opened = "";
    seenStep = stepKey;
  }
  document.querySelectorAll("[data-nav]").forEach(link => {
    const section = route.name === "ledger" ? "ledger" : route.name === "member" ? "" : "scene";
    const on = link.dataset.nav === section;
    if (on) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  });
  renderRail(route);
  const stage = document.getElementById("stage");
  if (route.name === "ledger") stage.innerHTML = renderLedger(route.f);
  else if (route.name === "member") stage.innerHTML = renderMember(route.a);
  else if (route.name === "work") stage.innerHTML = renderWork(route.a, route.b);
  else stage.innerHTML = renderScene();
  document.title = route.name === "ledger" ? "费用 · 测光" : "测光 · drop_by";
  if (route.name === "scene") {
    paint();
    requestAnimationFrame(drawLinks);
  }
}

function renderScene() {
  const names = LANES.map(id => {
    if (id === "you") return `<div class="who"><strong>你</strong><small>消息和补充</small></div>`;
    const p = PEOPLE[id];
    return `<div class="who"><a href="#member/${id}">${esc(p.name)}</a><small>人物 ${p.rev}</small></div>`;
  }).join("");
  const lanes = LANES.map(id => {
    const bars = BARS.filter(bar => bar.lane === id).map(barHTML).join("");
    const ticks = TICKS.filter(tick => tick.lane === id).map(tickHTML).join("");
    return `<div class="lane-track">${bars}${ticks}</div>`;
  }).join("");
  const marks = [
    ["start", 0, "21:10"],
    ["", 240, "21:14"],
    ["", 480, "21:18"],
    ["", 720, "21:22"],
    ["end", 1080, "21:28"]
  ].map(([edge, sec, label]) => `<span class="${edge}" style="left:${sec / HORIZON * 100}%">${label}</span>`).join("");
  const frames = FRAMES.map(frame => `<button type="button" data-frame="${frame.sec}">${esc(frame.label)}</button>`).join("");
  return `<header class="scene-head"><p class="kicker">随便聊聊 · 11 月 11 日 · 链看到 21:28</p><h1>雨，和一块灰</h1><p class="dek">一条消息叫醒了三个人。老周先开口，阿澄去查，小满要等自己的活动结束才进来。研究到样例截止还开着。这里没有一次「互动结束」。</p></header>
  <div class="attention">
    <a href="#work/cheng_research/r_drop"><span class="tag danger">未知</span><span><span class="attention-title">有一笔预留没有用量</span><span class="attention-text">阿澄的研究在后面又发出一次请求，连接断了。预留还在，不记成已花费。</span></span></a>
    <a href="#scene?t=249"><span class="tag">等锁</span><span><span class="attention-title">小满一开始不是安静</span><span class="attention-text">叫醒的时候她在个人活动里。锁放开之后她才进来，然后没有发言。</span></span></a>
    <a href="#work/cheng_research/r_compact"><span class="tag">压缩</span><span><span class="attention-title">贵的是这一次压缩</span><span class="attention-text">Sol 的原生压缩单独结算。打开可以看到不可读的项目，以及压缩前后的 token。</span></span></a>
  </div>
  <div class="board-scroll"><div class="board"><div class="names"><div class="axis-space"></div>${names}</div><div class="tracks" id="tracks"><div class="playhead" id="playhead"></div><svg id="links" aria-hidden="true"></svg><div class="axis"><div class="axis-labels">${marks}</div><input id="scrub" class="scrub" type="range" min="0" max="${HORIZON}" step="1" value="${state.sec}" aria-label="移动测光时刻"></div>${lanes}</div></div></div>
  <div class="frames">${frames}</div>
  <div class="readout" id="readout" aria-live="polite"></div>
  <section class="lock-strip"><h2>同时占着锁的个人活动</h2><div class="board"><div class="who"><a href="#member/man">小满</a><small>没有聊天对象</small></div><div class="tracks"><div class="lane-track"><a class="bar" style="left:0;width:${380 / HORIZON * 100}%" href="#work/man_personal"><span class="bar-label">自己先翻屋顶 · 21:16:20 结束</span></a></div></div></div><p class="lock-note">上面斜线的那一段，等的就是这一条。个人活动结束、锁放开，群里的调用才开始。</p></section>
  <p class="legend">实线框是一段行动。斜线是还在等锁。空心圆是这段结束时没有发言。红点是还没结束，而且费用未知。黄线是你正在看的时刻。实心菱形是已经发出的消息，空心菱形是真人补充，只入队。</p>
  ${footer()}`;
}

function barHTML(bar) {
  const width = Math.max((bar.end - bar.start) / HORIZON * 100, 1.1);
  const cls = `bar ${bar.tone}`.trim();
  const style = `left:${bar.start / HORIZON * 100}%;width:${width}%`;
  const label = width >= 8 ? `<span class="bar-label">${esc(bar.label)}</span>` : "";
  if (bar.work) return `<a class="${cls}" style="${style}" href="#work/${bar.work}" aria-label="${esc(bar.label)}">${label}</a>`;
  return `<button type="button" class="${cls}" style="${style}" data-seek="${bar.seek}" aria-label="${esc(bar.label)}">${label}</button>`;
}
function tickHTML(tick) {
  const label = tick.kind === "steer" ? "真人补充" : "消息";
  return `<button type="button" class="tick ${tick.kind === "steer" ? "steer" : ""}" style="left:${tick.sec / HORIZON * 100}%" data-seek="${tick.sec}" aria-label="${label}" title="${esc(tick.text)}"></button>`;
}

function activeLines(sec) {
  const lines = [];
  for (const bar of BARS) {
    if (sec < bar.start || sec >= bar.end) continue;
    if (!bar.work) {
      lines.push("小满 · 等个人活动放开锁，模型还没开始");
      continue;
    }
    const work = workById(bar.work);
    const step = [...work.steps].reverse().find(item => item.sec <= sec);
    lines.push(`${PEOPLE[work.who].name} · ${step ? step.title : "刚被叫醒"}`);
  }
  if (sec < 380) lines.push("小满的个人活动还占着这把锁");
  return lines;
}
function readoutHTML(sec) {
  const frame = FRAMES.reduce((best, item) => Math.abs(item.sec - sec) < Math.abs(best.sec - sec) ? item : best);
  const near = Math.abs(frame.sec - sec) <= 15;
  const tick = TICKS.filter(item => Math.abs(item.sec - sec) <= 6).sort((a, b) => Math.abs(a.sec - sec) - Math.abs(b.sec - sec))[0];
  const lines = activeLines(sec).map(line => `<li>${esc(line)}</li>`).join("");
  const quote = tick ? `<p class="quote-line">${tick.kind === "steer" ? "补充" : "消息"} · ${esc(tick.text)}</p>` : "";
  return `<time>${clock(sec)}</time>${near ? `<p class="lede">${esc(frame.lede)}</p>` : ""}<ul class="read-lines">${lines}</ul>${quote}`;
}
function paint() {
  const line = document.getElementById("playhead");
  const scrub = document.getElementById("scrub");
  const out = document.getElementById("readout");
  if (line) line.style.left = `${state.sec / HORIZON * 100}%`;
  if (scrub) {
    scrub.value = String(state.sec);
    scrub.setAttribute("aria-valuetext", clock(state.sec));
  }
  if (out) out.innerHTML = readoutHTML(state.sec);
  document.querySelectorAll("[data-frame]").forEach(button => {
    const on = Math.abs(Number(button.dataset.frame) - state.sec) <= 15 && FRAMES.reduce((best, item) => Math.abs(item.sec - state.sec) < Math.abs(best.sec - state.sec) ? item : best).sec === Number(button.dataset.frame);
    button.setAttribute("aria-pressed", on ? "true" : "false");
  });
}
function drawLinks() {
  const tracks = document.getElementById("tracks");
  const svg = document.getElementById("links");
  if (!tracks || !svg) return;
  const w = tracks.clientWidth;
  const h = tracks.clientHeight;
  if (!w || !h) return;
  svg.setAttribute("viewBox", `0 0 ${w} ${h}`);
  const x = sec => sec / HORIZON * w;
  const y = lane => AXIS + LANES.indexOf(lane) * LANE + 12;
  svg.innerHTML = LINKS.map(link => `<path d="M ${x(link.from.sec).toFixed(1)} ${y(link.from.lane).toFixed(1)} L ${x(link.to.sec).toFixed(1)} ${y(link.to.lane).toFixed(1)}"></path>`).join("");
}

function renderWork(id, stepId) {
  const work = workById(id);
  if (!work) return `<p>没有这项行动。</p>${footer()}`;
  const step = work.steps.find(item => item.id === stepId) || work.steps[0];
  const totals = workTotals(work);
  const person = PEOPLE[work.who];
  const span = work.status === "unknown" ? `${clock(work.start)} 开始，到 21:28 还没结束` : work.steps.length ? `${clock(work.start)} – ${clock(work.end)}` : work.when;
  const cost = work.status === "budget" ? "还没有调用" : `${money(totals.settled)} 已结算${totals.reserved ? ` · 另有预留 ${money(totals.reserved)} 未结算` : ""}`;
  const facts = [
    ["状态", work.statusLabel],
    ["触发", work.trigger],
    ["时间", span],
    ["费用", cost],
    ["人物", `版本 ${person.rev} · ${person.revised}${person.how}。这次没有新版本。`]
  ];
  const also = work.also.length ? `<div class="siblings">${work.also.map(item => `<a href="#work/${item.id}">${esc(item.label)}</a>`).join("")}</div>` : "";
  const maxMs = Math.max(1, ...work.steps.map(item => item.ms || 0));
  const steps = work.steps.map(item => {
    const priced = quoteUsage(usageOf(item));
    const price = priced && priced.kind === "settled" ? money(priced.micros) : priced ? `预留 ${money(priced.reserved)}` : "不单独计价";
    const width = item.ms ? Math.max(4, item.ms / maxMs * 100) : 0;
    return `<li><a class="step" href="#work/${work.id}/${item.id}" aria-pressed="${step && step.id === item.id ? "true" : "false"}"><time>${clock(item.sec)}</time><span><span class="step-title">${esc(item.title)}</span><span class="step-meta">${dur(item.ms)} · ${price}</span>${width ? `<span class="step-track"><i style="width:${width}%"></i></span>` : ""}</span></a></li>`;
  }).join("");
  const body = work.steps.length ? `<div class="work-split"><ol class="steps">${steps}</ol>${renderInspector(work, step)}</div>` : `<p class="dek">${esc(work.note)}</p>`;
  return `<header class="work-head"><p class="crumb"><a href="#scene?t=${work.start > 0 ? work.start : 249}">这场</a><span>/</span><a href="#member/${work.who}">${esc(person.name)}</a></p><p class="kicker">${esc(work.room)} · ${esc(work.kind)}</p><h1>${esc(work.title)}</h1><p class="work-status">${esc(work.statusLabel)}</p><dl class="facts">${facts.map(([k, v]) => `<dt>${k}</dt><dd>${esc(v)}</dd>`).join("")}</dl>${also}<details><summary>这次实际在场的配置</summary><p class="dek">${esc(work.runtime)} 人物正文就是系统身份里的那一篇。工具只列这次提供给模型的，没有一份叫 Harness 的开关。</p></details></header>${body}${footer()}`;
}

function renderInspector(work, step) {
  if (!step) return "";
  if (step.kind === "model" || step.kind === "compact") return modelInspector(step);
  if (step.kind === "tool") return toolInspector(step);
  if (step.kind === "organize") return organizeInspector(step);
  if (step.kind === "capacity") return capacityInspector(step);
  return `<section class="inspector"><h2>${esc(step.title)}</h2><p class="dek">${esc(step.body || "")}</p></section>`;
}

function modelInspector(step) {
  const call = step.call;
  const priced = quoteUsage(call.usage);
  const settled = call.usage.status === "settled";
  const amount = settled ? money(priced.micros) : `预留 ${money(priced.reserved)} · 没有成交价`;
  const usageLine = settled
    ? `输入 ${call.usage.input.toLocaleString("zh-CN")} · 缓存读 ${call.usage.cached.toLocaleString("zh-CN")} · 缓存写 ${call.usage.write.toLocaleString("zh-CN")} · 输出 ${call.usage.output.toLocaleString("zh-CN")}`
    : "没有 token 回执。空着，不用 0 装成已经知道。";
  const segs = call.segments || [];
  const total = segs.reduce((n, seg) => n + (seg.tokens || 0), 0) || 1;
  const bar = segs.map(seg => `<i data-k="${esc(seg.k)}" style="width:${seg.tokens / total * 100}%"></i>`).join("");
  const inLabel = step.kind === "compact" ? "返回窗口" : "实际输入";
  const tabs = `<div class="tabs" role="tablist"><button type="button" data-pane="in" aria-pressed="${state.pane === "in"}">${inLabel}</button><button type="button" data-pane="out" aria-pressed="${state.pane === "out"}">输出</button><button type="button" data-pane="new" aria-pressed="${state.pane === "new"}">只看新增</button></div>`;
  const shrink = step.beforeTokens ? `<div class="shrink" aria-hidden="true"><div><span>送去 ${step.beforeTokens.toLocaleString("zh-CN")}</span><b><i style="width:100%"></i></b></div><div><span>回来 ${step.afterTokens.toLocaleString("zh-CN")}</span><b><i style="width:${(step.afterTokens / step.beforeTokens * 100).toFixed(2)}%"></i></b></div></div>` : "";
  const tools = (call.tools || []).map(name => `<li>${esc(name)}</li>`).join("");
  const before = step.beforeItems ? `<h3>送去压缩的输入 · ${step.beforeTokens.toLocaleString("zh-CN")} tokens</h3><ul class="dropped">${step.beforeItems.map(item => `<li>${esc(item.k)} · 约 ${item.tokens.toLocaleString("zh-CN")}</li>`).join("")}</ul><p class="fine">加起来是 ${step.beforeTokens.toLocaleString("zh-CN")}。供应商没有交回一份可读的删节对照。下面是压缩后的窗口，约 ${step.afterTokens.toLocaleString("zh-CN")} tokens。</p>` : "";
  return `<section class="inspector"><p class="kicker">${step.kind === "compact" ? "原生压缩" : "模型调用"} · ${esc(call.usage.purpose)}</p><h2>${esc(call.model)}</h2><p class="dek">推理 ${esc(call.reasoning)} · 最大输出 ${call.maxOutput.toLocaleString("zh-CN")} · store false · ${call.usage.status === "unknown" ? "时长未知，到 21:28 仍没有回执" : dur(call.ms)}</p>
  <dl class="facts"><dt>状态</dt><dd>${call.usage.status === "settled" ? "已结算" : "未知，预留仍在"}</dd><dt>用量</dt><dd>${usageLine}</dd><dt>金额</dt><dd>${amount}</dd></dl>
  ${call.cacheNote ? `<p class="fine">${esc(call.cacheNote)}</p>` : ""}
  ${before}
  ${shrink}
  ${segs.length ? `<div class="tokenbar" aria-hidden="true">${bar}</div><p class="fine">色条是分段估算，加起来对齐${step.kind === "compact" ? "返回窗口" : "这次输入"}的 token，不是账单。钱只记在上面的金额里。</p>${tabs}<div class="well" id="well">${paneHTML(call)}</div>` : `<div class="well"><p class="well-plain">请求已经离开本机。没有可展示的输入，也没有输出。</p></div>`}
  <h3>这次提供的工具</h3><ul class="tool-list">${tools}</ul></section>`;
}
function paneHTML(call) {
  if (state.pane === "out") {
    return `<p class="well-plain">${esc(call.output || "这次没有可阅读的输出。压缩返回的是新的输入窗口，不是一段给群里看的话。")}</p>`;
  }
  const all = call.segments || [];
  const list = state.pane === "new" ? all.filter(seg => seg.cache !== "read") : all;
  if (!list.length) return `<p class="well-plain">这一页没有新增段落。缓存读到的前缀还在「实际输入」里。</p>`;
  let note = "";
  if (state.pane === "new") {
    const hidden = all.filter(seg => seg.cache === "read").reduce((n, seg) => n + (seg.tokens || 0), 0);
    if (hidden) note = `<p class="well-note">藏起缓存读的 ${hidden.toLocaleString("zh-CN")} tokens。它们仍在「实际输入」里，也仍算在上面的金额里。</p>`;
    else if (!(call.usage.cached || 0)) {
      const wrote = call.usage.write ? `缓存写了 ${call.usage.write.toLocaleString("zh-CN")}，那是这次留下的前缀，不是已经读过的旧段。` : "";
      note = `<p class="well-note">这次没有缓存读。${wrote}整段都按未命中计费。</p>`;
    }
  }
  return note + list.map(seg => `<div class="seg${seg.opaque ? " opaque" : ""}"><div class="seg-k">${esc(seg.k)}${seg.cache === "read" ? "<small>缓存读</small>" : ""}${seg.tokens ? `<small>约 ${seg.tokens.toLocaleString("zh-CN")}</small>` : ""}</div><p class="seg-body">${esc(seg.text)}</p></div>`).join("");
}
function paintPane() {
  document.querySelectorAll("[data-pane]").forEach(button => button.setAttribute("aria-pressed", button.dataset.pane === state.pane ? "true" : "false"));
  const well = document.getElementById("well");
  const step = currentStep();
  if (well && step && step.call) well.innerHTML = paneHTML(step.call);
}
function currentStep() {
  const route = parseRoute();
  const work = workById(route.a);
  if (!work) return null;
  return work.steps.find(item => item.id === route.b) || work.steps[0];
}

function toolInspector(step) {
  const tool = step.tool;
  const priced = quoteUsage(tool.usage);
  const cost = priced ? money(priced.micros) : "不单独计价";
  return `<section class="inspector"><p class="kicker">工具 · ${esc(tool.status === "failed" ? "失败" : "完成")}</p><h2>${esc(tool.name)}</h2><p class="dek">${dur(tool.ms)} · ${cost}</p><p class="fine">${esc(tool.retry)}</p><h3>参数</h3><div class="well"><p class="well-plain">${esc(tool.args)}</p></div><h3>结果</h3><div class="well"><p class="well-plain">${esc(tool.result)}</p></div></section>`;
}
function organizeInspector(step) {
  const col = items => items.map(item => `<div class="doc-item"><strong>${esc(item.k)}</strong><p>${esc(item.text)}</p></div>`).join("");
  const dropped = step.dropped.map(item => `<li><button type="button" class="text-link" data-open="${esc(item.id)}">${esc(item.k)}</button></li>`).join("");
  return `<section class="inspector"><p class="kicker">自行整理</p><h2>她换掉了自己的窗口</h2><p class="dek">这是 organize_context。便笺和点名留下的原文替换旧窗口。移出窗口的内容还在，没有被模型摘要掉。</p><div class="split-doc"><div class="doc-col"><h3>整理前</h3>${col(step.before)}</div><div class="doc-col"><h3>整理后</h3>${col(step.after)}</div></div><h3>移出窗口、仍然能打开</h3><ul class="dropped">${dropped}</ul><div id="opened">${openedHTML(step)}</div></section>`;
}
function capacityInspector(step) {
  return `<section class="inspector"><p class="kicker">容量兜底</p><h2>压缩之后仍然放不下</h2><p class="dek">旧正文换成可定位的回读入口。这不是第三种摘要。原文还在工具结果里。</p><div class="split-doc"><div class="doc-col"><h3>换之前</h3><div class="doc-item"><p>${esc(step.beforeText)}</p></div></div><div class="doc-col"><h3>送进下一次请求的</h3><div class="doc-item"><p>${esc(step.afterText)}</p></div></div></div><p><button type="button" class="text-link" data-open="original">${state.opened === "original" ? "收起仍保存的原文" : "打开仍保存的原文"}</button></p><div id="opened">${state.opened === "original" ? `<div class="doc-item"><p>${esc(step.original)}</p></div>` : ""}</div></section>`;
}
function openedHTML(step) {
  if (!state.opened || !step.dropped) return "";
  const item = step.dropped.find(entry => entry.id === state.opened);
  if (!item) return "";
  return `<div class="doc-item"><strong>${esc(item.k)}</strong><p>${esc(item.text)}</p></div>`;
}

function renderMember(id) {
  const person = PEOPLE[id];
  if (!person || id === "you") return `<p>没有这个成员。</p>${footer()}`;
  const runs = WORKS.filter(work => work.who === id);
  const memory = id === "man" ? `<article class="memory"><p>${esc(MEMORY.text)}</p><p class="fine">个人活动的第二次调用看见了它。<a href="#work/man_personal/mp2">打开那次输入</a>。研究和其他人的行动里没有自动带上这条。</p></article>` : `<p class="dek">这几场没有检索记忆。人物也没有因为聊天被改写。</p>`;
  const list = runs.map(work => `<li><a href="#work/${work.id}"><span>${esc(work.when || clock(work.start))}</span><span>${esc(work.title)}</span><span>${esc(work.statusLabel)}</span></a></li>`).join("");
  return `<article class="member-page"><p class="kicker">成员 · 样例人物，不是资料页上的原文</p><h1>${esc(person.name)}</h1><p class="dek">人物版本 ${person.rev} · ${esc(person.revised)} · ${esc(person.how)}。这几场没有新版本。</p><blockquote><p>${esc(person.focus)}</p><p class="fine">近期关注。它会进个人活动的输入，不写进人物正文。</p></blockquote><h2>人物正文</h2><p class="dek">${esc(person.profile)}</p><h2>记忆</h2>${memory}<h2>行动</h2><ul class="run-list">${list}</ul></article>${footer()}`;
}

function renderLedger(filter) {
  const rows = charges();
  const settled = rows.filter(row => row.priced.kind === "settled");
  const unknown = rows.filter(row => row.priced.kind !== "settled");
  const sum = settled.reduce((n, row) => n + row.priced.micros, 0);
  const hold = unknown.reduce((n, row) => n + row.priced.reserved, 0);
  const groups = new Map();
  for (const row of settled) {
    const key = row.usage.provider === "deepseek" ? "搜索 / 读网页" : row.usage.model;
    const bin = groups.get(key) || { micros: 0, n: 0 };
    bin.micros += row.priced.micros;
    bin.n += 1;
    groups.set(key, bin);
  }
  const chips = [["all", "全部"], ["unknown", "未知"], ["sol", "Sol"], ["terra", "Terra"], ["search", "搜索"]].map(([id, label]) => `<button type="button" data-filter="${id}" aria-pressed="${filter === id ? "true" : "false"}">${label}</button>`).join("");
  const shown = rows.filter(row => {
    if (filter === "unknown") return row.priced.kind !== "settled";
    if (filter === "sol") return row.usage.model === "gpt-6.1-sol";
    if (filter === "terra") return row.usage.model === "gpt-6.1-terra";
    if (filter === "search") return row.usage.provider === "deepseek";
    return true;
  });
  const body = shown.map(row => {
    const u = row.usage;
    const model = u.provider === "deepseek" ? "DeepSeek" : u.model.replace("gpt-6.1-", "");
    const status = row.priced.kind === "settled" ? "已结算" : "未知";
    const amount = row.priced.kind === "settled" ? money(row.priced.micros) : money(row.priced.reserved);
    const cell = n => n == null ? "—" : Number(n).toLocaleString("zh-CN");
    return `<tr class="${status === "未知" ? "unknown" : ""}" data-href="#work/${row.work.id}/${row.step.id}"><td><a href="#work/${row.work.id}/${row.step.id}">${clock(row.step.sec)}</a></td><td>${esc(PEOPLE[row.work.who].name)}</td><td>${esc(u.purpose)}</td><td>${esc(model)}</td><td>${status}</td><td class="num">${dur(row.step.ms)}</td><td class="num">${cell(u.input)}</td><td class="num">${cell(u.cached)}</td><td class="num">${cell(u.write)}</td><td class="num">${cell(u.output)}</td><td class="num">${amount}</td></tr>`;
  }).join("");
  const cards = [...groups.entries()].map(([key, bin]) => `<div><span>${esc(key)} · ${bin.n} 笔</span><strong>${money(bin.micros)}</strong></div>`).join("");
  return `<article class="ledger"><p class="kicker">按调用结算 · 不是按上下文字段拆钱</p><h1>费用</h1><p class="sum">已结算 ${money(sum)} · 未知预留 ${money(hold)} 不在已结算里</p><div class="break">${cards}</div><p class="dek">阿澄这场看起来贵，主要是 Sol 上那次原生压缩，单笔 ${money(120000)}。后面的研究调用又因为新网页变长，但还是比这一笔小。搜索和读网页按次另计，每笔 ${money(750)}。</p><div class="chips">${chips}</div><div class="table-scroll"><table><thead><tr><th>时刻</th><th>谁</th><th>用途</th><th>模型</th><th>状态</th><th>时长</th><th class="num">输入</th><th class="num">缓存读</th><th class="num">缓存写</th><th class="num">输出</th><th class="num">金额</th></tr></thead><tbody>${body}</tbody></table></div><p class="fine">普通输入 = 总输入 − 缓存读 − 缓存写。按该模型标价合成整数纳美元后，向上取整到微美元。等预算的后续还没有调用，所以不在这张表里。发消息和整理窗口不单独计价。</p></article>${footer()}`;
}

function footer() {
  return `<p class="footer-note">样例按现行机制编排，时间停在 11 月 11 日 21:28。还没有接到正在运行的记录。人物是样例，不是资料页上的原文。 / 查找 · 拖动黄线看某一刻 · j k 换帧或换步骤 · esc 回到这场</p>`;
}

function boot() {
  if (!location.hash) history.replaceState(null, "", "#scene?t=249");
  const find = document.getElementById("find");
  find.addEventListener("input", () => {
    state.query = find.value;
    renderRail(parseRoute());
  });
  find.addEventListener("keydown", event => {
    if (event.key !== "Enter") return;
    const first = visibleWorks()[0];
    if (first) location.hash = `#work/${first.id}`;
  });
  const stage = document.getElementById("stage");
  stage.addEventListener("input", event => {
    if (event.target.id !== "scrub") return;
    state.sec = clamp(Number(event.target.value));
    history.replaceState(null, "", `#scene?t=${state.sec}`);
    paint();
  });
  stage.addEventListener("click", event => {
    const frame = event.target.closest("[data-frame]");
    if (frame) {
      state.sec = Number(frame.dataset.frame);
      history.replaceState(null, "", `#scene?t=${state.sec}`);
      paint();
      return;
    }
    const seek = event.target.closest("[data-seek]");
    if (seek) {
      state.sec = Number(seek.dataset.seek);
      history.replaceState(null, "", `#scene?t=${state.sec}`);
      paint();
      return;
    }
    const pane = event.target.closest("[data-pane]");
    if (pane) {
      state.pane = pane.dataset.pane;
      paintPane();
      return;
    }
    const opener = event.target.closest("[data-open]");
    if (opener) {
      state.opened = state.opened === opener.dataset.open ? "" : opener.dataset.open;
      const step = currentStep();
      const box = document.getElementById("opened");
      if (!box || !step) return;
      if (step.kind === "capacity") {
        box.innerHTML = state.opened ? `<div class="doc-item"><p>${esc(step.original)}</p></div>` : "";
        opener.textContent = state.opened ? "收起仍保存的原文" : "打开仍保存的原文";
      } else box.innerHTML = openedHTML(step);
      return;
    }
    const filter = event.target.closest("[data-filter]");
    if (filter) {
      location.hash = `#ledger?f=${filter.dataset.filter}`;
      return;
    }
    const row = event.target.closest("tr[data-href]");
    if (row) location.hash = row.dataset.href;
  });
  window.addEventListener("hashchange", render);
  window.addEventListener("resize", drawLinks);
  document.addEventListener("keydown", event => {
    if (event.key === "/" && document.activeElement !== find) {
      event.preventDefault();
      find.focus();
      return;
    }
    if (event.key === "Escape") {
      if (document.activeElement === find) {
        find.blur();
        return;
      }
      const route = parseRoute();
      if (route.name !== "scene") location.hash = `#scene?t=${state.sec}`;
      return;
    }
    if (event.target.matches("input, textarea")) return;
    if (event.key !== "j" && event.key !== "k") return;
    const dir = event.key === "j" ? 1 : -1;
    const route = parseRoute();
    if (route.name === "work") {
      const work = workById(route.a);
      if (!work || !work.steps.length) return;
      const index = Math.max(0, work.steps.findIndex(step => step.id === route.b));
      const next = work.steps[index + dir];
      if (next) location.hash = `#work/${work.id}/${next.id}`;
      return;
    }
    if (route.name !== "scene") return;
    const secs = FRAMES.map(frame => frame.sec);
    const target = dir > 0 ? secs.find(sec => sec > state.sec + 2) : [...secs].reverse().find(sec => sec < state.sec - 2);
    state.sec = target == null ? clamp(state.sec + dir * 20) : target;
    history.replaceState(null, "", `#scene?t=${state.sec}`);
    paint();
  });
  render();
}

function verify() {
  const problems = [];
  for (const work of WORKS) {
    for (const step of work.steps) {
      if (!step.call || step.call.usage.status !== "settled" || !step.call.segments) continue;
      const sum = step.call.segments.reduce((n, seg) => n + (seg.tokens || 0), 0);
      const expect = step.kind === "compact" ? step.afterTokens : step.call.usage.input;
      if (sum !== expect) problems.push(`${step.id} segments ${sum} != ${expect}`);
      if (step.kind !== "compact") {
        const read = step.call.segments.filter(seg => seg.cache === "read").reduce((n, seg) => n + seg.tokens, 0);
        if (read !== step.call.usage.cached) problems.push(`${step.id} cache ${read} != ${step.call.usage.cached}`);
      }
      const normal = step.call.usage.input - step.call.usage.cached - step.call.usage.write;
      if (normal < 0) problems.push(`${step.id} negative normal input`);
    }
  }
  const compact = quoteUsage(workById("cheng_research").steps.find(step => step.id === "r_compact").call.usage);
  if (compact.micros !== 120000) problems.push("compact price " + compact.micros);
  return problems;
}

if (typeof document === "undefined") {
  globalThis.Meter = { verify, quoteUsage, charges, money, WORKS };
} else if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", boot);
} else {
  boot();
}

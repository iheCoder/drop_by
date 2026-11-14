const timeFormat = new Intl.DateTimeFormat("zh-CN", {
  month: "long", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: "Asia/Shanghai"
});
const stepNames = {
  input_tokens: "输入计数", input_tokens_local: "本地输入估算", websocket: "建立连接",
  compact: "原生压缩", send_message: "发消息", steer: "真人补充"
};
const statusNames = {
  started: "进行中", running: "进行中", succeeded: "成功", done: "完成", failed: "失败", error: "出错",
  sent: "已发送", reused: "沿用已有结果", quiet: "没有发言", act: "参与", not_called: "未调用",
  blocked: "暂缓", pending: "待执行", cancelled: "已取消", skipped: "已跳过",
  resume: "恢复", interrupted: "已中断"
};
const runNames = {
  chat: "聊天响应", explore: "个人探索", research: "研究", followup: "后续行动"
};

async function read(path) {
  const response = await fetch(path);
  if (!response.ok) {
    const error = new Error("请求失败 " + response.status);
    error.status = response.status;
    throw error;
  }
  return response.json();
}

function timeAt(at) {
  return timeFormat.format(at).replace(/\s+/g, " ");
}

function avatarOf(member) {
  if (/^data:image\/(?:png|jpeg|webp);base64,[A-Za-z0-9+/=]+$/.test(member.avatar || "")) return member.avatar;
  if (["portrait-sky", "portrait-traveler", "portrait-cat"].includes(member.avatar_icon)) {
    return "/assets/" + member.avatar_icon + ".webp";
  }
  return "";
}

// Only show totals actually present in a provider receipt. The recorded cost comes from
// the ledger, which can apply pricing and cache-write tiers that are absent from the response.
function usageFrom(output) {
  try {
    const raw = JSON.parse(output);
    const responses = raw?.usage;
    if (Number.isInteger(responses?.input_tokens) && Number.isInteger(responses?.output_tokens)) {
      return {
        input: responses.input_tokens, cached: responses.input_tokens_details?.cached_tokens || 0,
        write: responses.input_tokens_details?.cache_write_tokens || 0, output: responses.output_tokens
      };
    }
    const gemini = raw?.usageMetadata;
    if (Number.isInteger(gemini?.promptTokenCount) && (Number.isInteger(gemini.totalTokenCount) || Number.isInteger(gemini.thoughtsTokenCount))) {
      const input = gemini.promptTokenCount + (gemini.toolUsePromptTokenCount || 0);
      const output = (gemini.candidatesTokenCount || 0) + (gemini.thoughtsTokenCount || 0);
      if (gemini.totalTokenCount == null || gemini.totalTokenCount === input + output) {
        return { input, cached: gemini.cachedContentTokenCount || 0, write: 0, output };
      }
    }
  } catch {
    // Some snapshots are text or contain a response followed by an error.
  }
  return null;
}

function toStep(record, run, actor) {
  const at = Date.parse(record.started_at || run.started_at);
  const end = Date.parse(record.ended_at);
  const model = record.kind === "model" ? record.name : "";
  const title = record.kind === "participation" ? "参与判断" : stepNames[record.name] || record.name || record.kind;
  return {
    id: record.id, kind: record.kind, model, title, at, clock: timeAt(at).slice(-5),
    person: actor(record.member_id),
    sent: record.status === "sent",
    room: record.conversation_id || run.conversation_id,
    target: record.target_conversation_id,
    message: record.kind === "message" && ["sent", "reused"].includes(record.status) ? record.business_id : "",
    parent: record.parent_id,
    note: record.summary,
    statusLabel: statusNames[record.status] || record.status,
    failed: ["failed", "error"].includes(record.status),
    price: record.cost_micros,
    reserve: record.reserved_micros,
    costStatus: record.cost_status,
    ms: Number.isFinite(end) ? Math.max(0, end - at) : 0,
    kicker: record.kind === "model" ? "模型调用" : record.kind === "tool" ? "工具调用" : "执行记录",
    // Snapshots can contain full model windows. Fetch only the step being inspected.
    snapshot: { async load() {
      if (!record.has_snapshot) return {};
      const data = await read("/api/steps/" + encodeURIComponent(record.id));
      return { ...data, usage: usageFrom(data.output) };
    } }
  };
}

export async function observeSource() {
  const [catalog, observation] = await Promise.all([read("/api/catalog").catch(() => null), read("/api/runs")]);
  const people = new Map((catalog?.members || []).map(member => [member.id, {
    id: member.id, name: member.name, avatar: avatarOf(member), note: ""
  }]));
  const rooms = new Map((catalog?.conversations || []).map(room => [room.id, {
    id: room.id, name: room.name, place: room.kind === "group" ? "群" : "私聊",
    members: room.member_ids || [], items: [], blurb: ""
  }]));
  const personal = { id: "personal", name: "个人活动", place: "个人", members: [], items: [], blurb: "没有关联会话的活动" };
  const works = new Map();
  const feeds = new Map();
  const delivered = new Map();
  const notices = (observation.gaps || []).map(gap => gap.note);
  if (!catalog) notices.push("暂时读不到聊天服务。仍可查看已记录的调用。");

  function actor(id) {
    if (id === "user") return "you";
    if (id === "system") return "";
    if (id && !people.has(id)) people.set(id, { id, name: "成员 " + id.slice(-6), note: "" });
    return id || "";
  }

  function putItem(room, item) {
    room.items.push(item);
    if (!item.person || item.person === "you") return;
    if (!feeds.has(item.person)) feeds.set(item.person, []);
    feeds.get(item.person).push(item);
  }

  function roomFor(id) {
    if (!id || id.startsWith("member:")) return personal;
    if (!rooms.has(id)) rooms.set(id, {
      id, name: "会话 " + id.slice(-6), place: "会话", members: [], items: [], blurb: ""
    });
    return rooms.get(id);
  }

  // Runs and chat messages are independent reads. A missing room response must not hide its trace.
  const [runResults, roomResults] = await Promise.all([
    Promise.allSettled(observation.runs.map(run => read("/api/runs/" + encodeURIComponent(run.id)))),
    Promise.allSettled([...rooms.values()].map(room =>
      read("/api/conversations/" + encodeURIComponent(room.id) + "/messages").then(data => [room, data])))
  ]);
  if (runResults.some(result => result.status === "rejected")) notices.push("有些执行步骤读取失败，请刷新重试。");
  if (roomResults.some(result => result.status === "rejected")) notices.push("有些会话的消息读取失败，已保留对应的观测记录。");

  for (const [index, run] of observation.runs.entries()) {
    const result = runResults[index];
    const steps = result.status === "fulfilled" ? result.value.steps.map(record => toStep(record, run, actor)) : [];
    const person = actor(run.member_id);
    const work = {
      id: run.id, person, who: people.get(person)?.name || "执行记录", steps,
      title: runNames[run.kind] || run.kind || "执行记录",
      kind: runNames[run.kind] || run.kind || "执行记录",
      status: [statusNames[run.status] || run.status, run.note].filter(Boolean).join(" · ")
    };
    works.set(work.id, work);

    for (const step of steps) {
      const room = roomFor(step.room);
      const owner = step.person || person;
      const item = {
        id: "step-" + step.id, kind: "event", at: step.at, time: timeAt(step.at),
        person: owner, who: people.get(owner)?.name || work.who,
        title: step.title, where: room.place + " · " + room.name, stepRef: { work: work.id, step: step.id }
      };
      putItem(room, item);

      // A delivery is paired with its chat message by persisted ID. A model draft is never a message.
      if (step.message && step.target) {
        const key = step.target + "/" + step.message;
        if (!delivered.has(key) || step.sent) {
          // A trace can lack the parent attempt, so the delivery itself remains inspectable.
          const parent = steps.find(one => one.id === step.parent);
          delivered.set(key, { work: work.id, step: parent?.id || step.id });
        }
      }
    }
    if (!steps.length) {
      const at = Date.parse(run.started_at);
      const room = roomFor(run.conversation_id);
      const step = {
        id: run.id, kind: "run", at, clock: timeAt(at).slice(-5), title: work.title,
        body: result.status === "rejected" ? "暂时读不到步骤，请刷新重试。" : (run.note || "这次执行还没有记录到步骤。"),
        statusLabel: statusNames[run.status] || run.status, kicker: "执行记录", note: run.note
      };
      work.steps.push(step);
      const item = {
        id: "run-" + run.id, kind: "event", person, who: work.who, title: work.title, at, time: timeAt(at),
        where: room.place + " · " + room.name, stepRef: { work: work.id, step: step.id }
      };
      putItem(room, item);
    }
  }

  for (const result of roomResults) {
    if (result.status !== "fulfilled") continue;
    const [room, data] = result.value;
    if (data.has_earlier) room.origin = "显示主站最近一页消息和近期观测记录";
    for (const message of data.messages || []) {
      const person = actor(message.author_id);
      const at = Date.parse(message.created_at);
      const delivery = delivered.get(room.id + "/" + message.id);
      room.items.push({
        id: "message-" + message.id, kind: "say", person, at, time: timeAt(at),
        who: person === "you" ? "你" : (people.get(person)?.name || (message.author_id === "system" ? "系统" : "成员")),
        text: message.body, where: room.place + " · " + room.name, stepRef: delivery
      });
    }
    room.blurb = "消息和执行步骤按时间排列";
    room.featured = room.place === "群";
  }
  if (personal.items.length) rooms.set(personal.id, personal);
  const orderedRooms = [...rooms.values()];
  const byTime = (first, second) => first.at - second.at;
  for (const room of orderedRooms) room.items.sort(byTime);
  for (const items of feeds.values()) items.sort(byTime);

  return {
    people: [...people.values()], rooms: orderedRooms,
    defaultRoom: orderedRooms.find(room => room.place === "群" && room.items.some(item => item.stepRef))?.id
      || orderedRooms.find(room => room.items.some(item => item.stepRef))?.id || orderedRooms[0]?.id,
    notice: notices.join(" "),
    workById: id => works.get(id),
    roomFeed: room => room?.items || [],
    personFeed: person => feeds.get(person?.id) || []
  };
}

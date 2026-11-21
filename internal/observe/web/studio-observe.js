const timeFormat = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric", month: "long", day: "numeric",
  hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23", timeZone: "Asia/Shanghai"
});
const stepNames = {
  input_tokens: "输入计数", input_tokens_local: "本地输入估算", websocket: "建立连接",
  compact: "原生压缩", send_message: "发消息", steer: "真人补充",
  react_to_message: "回应消息", search: "搜索网页", read_page: "读网页",
  read_conversation: "看聊天近况", recall: "回顾记录", organize_context: "整理思路",
  remember: "整理经历", schedule_followup: "记下后续", schedule_exploration: "安排探索"
};
const statusNames = {
  started: "进行中", running: "进行中", succeeded: "成功", done: "完成", failed: "失败", error: "出错",
  sent: "已发送", reused: "沿用已有结果", quiet: "没有发言", act: "参与", not_called: "未调用",
  blocked: "暂缓", pending: "待执行", cancelled: "已取消", skipped: "已跳过",
  resume: "恢复", interrupted: "已中断", waiting_budget: "预算不足"
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
      const reasoning = responses.output_tokens_details?.reasoning_tokens;
      return {
        input: responses.input_tokens, cached: responses.input_tokens_details?.cached_tokens || 0,
        write: responses.input_tokens_details?.cache_write_tokens || 0, output: responses.output_tokens,
        reasoning: Number.isInteger(reasoning) ? reasoning : null
      };
    }
    const gemini = raw?.usageMetadata;
    if (Number.isInteger(gemini?.promptTokenCount) && (Number.isInteger(gemini.totalTokenCount) || Number.isInteger(gemini.thoughtsTokenCount))) {
      const thoughts = gemini.thoughtsTokenCount;
      const input = gemini.promptTokenCount + (gemini.toolUsePromptTokenCount || 0);
      const output = (gemini.candidatesTokenCount || 0) + (Number.isInteger(thoughts) ? thoughts : 0);
      if (gemini.totalTokenCount == null || gemini.totalTokenCount === input + output) {
        return {
          input, cached: gemini.cachedContentTokenCount || 0, write: 0, output,
          reasoning: Number.isInteger(thoughts) ? thoughts : null
        };
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
  const title = record.kind === "participation" ? "参与判断"
    : record.kind === "message" ? "写入对话"
    : stepNames[record.name] || record.name || record.kind;
  const ledgerId = /^[0-9]+$/.test(record.business_id || "") ? record.business_id : "";
  const budget = record.kind === "model" && ["failed", "error"].includes(record.status) && !ledgerId && /预算/.test((run.note || "") + (record.summary || ""));
  const billed = record.kind === "model" || record.kind === "compact";
  const notSent = billed && record.cost_status === "none" && !!ledgerId && !Number(record.reserved_micros) && !Number(record.cost_micros);
  const stepOpen = !record.status || ["started", "running"].includes(record.status);
  const runOpen = ["started", "running"].includes(run.status);
  const running = runOpen && stepOpen && record.cost_status === "unknown" && Number(record.reserved_micros) > 0;
  const costKind = budget ? "budget" : notSent ? "not_sent" : (record.cost_status === "released" ? "released" : (running ? "running" : ""));
  return {
    id: record.id, kind: record.kind, name: record.name, model, title, at,
    raw: record.started_at || run.started_at, call: record.call_id, ledgerId, costKind,
    person: actor(record.member_id),
    sent: record.status === "sent",
    room: record.conversation_id || run.conversation_id,
    target: record.target_conversation_id,
    message: record.kind === "message" && ["sent", "reused"].includes(record.status) ? record.business_id : "",
    business: record.business_id || "",
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
  const catalog = await read("/api/catalog").catch(() => null);
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
  const deliveryByRoom = new Map();
  const notices = [];
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

  let orderedRooms = null;
  function roomFor(id) {
    if (!id || id.startsWith("member:")) return personal;
    if (!rooms.has(id)) rooms.set(id, {
      id, name: "会话 " + id.slice(-6), place: "会话", members: [], items: [], blurb: ""
    });
    const room = rooms.get(id);
    if (orderedRooms && !orderedRooms.includes(room)) orderedRooms.push(room);
    return room;
  }

  orderedRooms = [...rooms.values(), personal];
  const byTime = (first, second) => first.at - second.at;
  function linkRef(room, messageId) {
    const saved = delivered.get(room.id + "/" + messageId);
    if (saved) return saved;
    const record = deliveryByRoom.get(room.id)?.get(messageId);
    if (!record?.run_id) return null;
    return {
      work: record.run_id, step: record.step_id, rootId: record.root_id || "",
      execution: record.execution || 1, conversationId: record.conversation_id || "",
      memberId: record.member_id || "", kind: record.kind || "", status: record.status || "",
      note: record.note || "", workId: record.work_id || ""
    };
  }
  function sortFeeds() {
    for (const room of orderedRooms) {
      for (const item of room.items) {
        if (item.kind !== "say" || item.stepRef) continue;
        const delivery = linkRef(room, item.id.slice("message-".length));
        if (delivery) item.stepRef = delivery;
      }
      room.items.sort(byTime);
    }
    for (const items of feeds.values()) items.sort(byTime);
  }
  function addRun(run, result, place = true) {
    let work = works.get(run.id);
    if (!work) {
      const steps = result?.status === "fulfilled" ? result.value.steps.map(record => toStep(record, run, actor)) : [];
      const person = actor(run.member_id);
      work = {
        id: run.id, person, who: people.get(person)?.name || "执行记录", steps,
        title: runNames[run.kind] || run.kind || "执行记录",
        kind: runNames[run.kind] || run.kind || "执行记录",
        status: [statusNames[run.status] || run.status, run.note].filter(Boolean).join(" · "),
        rootId: run.root_id || run.work_id || "",
        execution: run.execution || 1,
        conversationId: run.conversation_id || "",
        runStatus: run.status || "",
        placed: false
      };
      works.set(work.id, work);
      for (const step of steps) {
        if (step.message && step.target) {
          const key = step.target + "/" + step.message;
          if (!delivered.has(key) || step.sent) {
            const tool = steps.find(one => one.kind === "tool" && one.name === "send_message" && one.parent === step.parent);
            delivered.set(key, { work: work.id, step: tool?.id || step.id });
          }
        }
      }
      if (!steps.length) {
        const at = Date.parse(run.started_at);
        work.steps.push({
          id: run.id, kind: "run", at, raw: run.started_at, title: work.title,
          body: result?.status === "rejected" ? "暂时读不到步骤，请刷新重试。" : (run.note || "这次执行还没有记录到步骤。"),
          statusLabel: statusNames[run.status] || run.status, kicker: "执行记录", note: run.note,
          person, room: run.conversation_id || ""
        });
      }
    }
    if (!place || work.placed) return;
    work.placed = true;
    for (const step of work.steps) {
      const room = roomFor(step.room || work.conversationId);
      const owner = step.person || work.person;
      putItem(room, {
        id: (step.kind === "run" ? "run-" : "step-") + step.id, kind: "event", at: step.at, raw: step.raw,
        time: Number.isFinite(step.at) ? timeAt(step.at) : "",
        person: owner, who: people.get(owner)?.name || work.who,
        title: step.title, where: room.place + " · " + room.name, stepRef: { work: work.id, step: step.id }
      });
    }
  }
  function placeMemberSteps(work, personId) {
    if (!work || !personId) return;
    if (!feeds.has(personId)) feeds.set(personId, []);
    const feed = feeds.get(personId);
    const seen = new Set(feed.map(item => item.id));
    for (const step of work.steps) {
      const owner = step.person || work.person;
      if (owner !== personId) continue;
      const id = (step.kind === "run" ? "run-" : "step-") + step.id;
      if (seen.has(id)) continue;
      seen.add(id);
      const room = roomFor(step.room || work.conversationId);
      const described = step.kind === "model" ? describeCall(work, step) : { purpose: "", detail: "" };
      feed.push({
        id, kind: "event", at: step.at, raw: step.raw,
        time: Number.isFinite(step.at) ? timeAt(step.at) : "",
        person: personId, who: people.get(personId)?.name || work.who,
        title: described.purpose || step.title, purpose: described.purpose, detail: described.detail,
        where: room.place ? room.place + " · " + room.name : room.name,
        stepRef: { work: work.id, step: step.id }
      });
    }
  }
  function roundOf(steps, step) {
    if (!step || !steps.length) return [];
    const byId = new Map(steps.map(one => [one.id, one]));
    let model = step.kind === "model" ? step : null;
    let cursor = step;
    const seen = new Set();
    while (!model && cursor && !seen.has(cursor.id)) {
      seen.add(cursor.id);
      cursor = byId.get(cursor.parent);
      if (cursor && cursor.kind === "model") model = cursor;
    }
    if (!model) return [step];
    const ids = new Set([model.id]);
    if (model.call) for (const one of steps) if (one.call && one.call === model.call) ids.add(one.id);
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
  const toolPurpose = {
    react_to_message: "给一条消息加了回应",
    send_message: "写了消息，没有发出去",
    search: "搜索网页", read_page: "读网页", read_conversation: "看了聊天近况",
    recall: "回顾了记录", organize_context: "整理了思路", remember: "整理了经历",
    schedule_followup: "记下之后想继续的事", schedule_exploration: "安排了探索",
    read_work: "看了进展", read_tool_result: "核对了材料"
  };
  function nextNote(work, step) {
    const index = work.steps.findIndex(one => one.id === step.id);
    for (let i = index + 1; i < work.steps.length; i++) {
      const one = work.steps[i];
      if (one.kind !== "participation" || one.person !== step.person) continue;
      const text = (one.note || "").replace(/\s+/g, " ").trim();
      if (!text || text === "这次没有叫到") return "";
      return text.split("；")[0];
    }
    return "";
  }
  function describeCall(work, step) {
    const tools = [...new Set(roundOf(work.steps, step).filter(one => one.kind === "tool").map(one => one.name))];
    const note = nextNote(work, step);
    let purpose = "";
    if (tools.length) purpose = tools.map(name => toolPurpose[name] || name).join("，");
    else if (step.failed && /预算/.test(note)) purpose = "想发言，预算不够";
    else if (step.failed && /超时/.test(note)) purpose = "想发言，服务超时";
    else if (step.failed && /连接/.test(note)) purpose = "想发言，服务没有连上";
    else if (step.failed) purpose = note || "这次调用没有完成";
    else purpose = note || "看过，没有再发言";
    return { purpose, detail: note && note !== purpose ? note : "" };
  }
  function placeLooseCalls(work, room) {
    if (!work) return;
    const covered = new Set();
    for (const step of work.steps) {
      if (step.kind !== "message" || !step.message || step.target !== room.id) continue;
      for (const one of roundOf(work.steps, step)) covered.add(one.id);
    }
    const seen = new Set(room.items.map(item => item.id));
    const loose = work.steps.filter(step => step.kind === "model" && !covered.has(step.id) && (step.room || work.conversationId) === room.id);
    const rows = loose.length ? loose : work.steps.filter(step => step.kind === "run");
    for (const step of rows) {
      const id = (step.kind === "run" ? "run-" : "step-") + step.id;
      if (seen.has(id)) continue;
      seen.add(id);
      const person = step.person || work.person;
      const described = step.kind === "model" ? describeCall(work, step) : { purpose: "", detail: "" };
      room.items.push({
        id, kind: "event", at: step.at, raw: step.raw,
        time: Number.isFinite(step.at) ? timeAt(step.at) : "",
        person, who: people.get(person)?.name || work.who,
        title: described.purpose || step.title, purpose: described.purpose, detail: described.detail,
        where: room.place + " · " + room.name,
        stepRef: { work: work.id, step: step.id }
      });
    }
  }
  async function loadLooseCalls(room, next) {
    const key = "room:" + room.id;
    const page = runPages.get(key);
    if (next && !page?.hasEarlier) return;
    let data = { runs: [] };
    try {
      data = await read(runsURL({ conversation: room.id }, next ? page : null));
    } catch {
      notices.push("有些调用读取失败，请刷新重试。");
    }
    const runs = data.runs || [];
    if (runs.length) {
      const oldest = runs[runs.length - 1];
      runPages.set(key, { before: oldest.started_at, id: oldest.id, hasEarlier: !!data.has_earlier });
    } else {
      runPages.set(key, { ...(page || {}), hasEarlier: false });
    }
    const fresh = runs.filter(run => run?.id && !works.has(run.id));
    const results = await Promise.allSettled(fresh.map(run => read("/api/runs/" + encodeURIComponent(run.id))));
    if (results.some(result => result.status === "rejected")) notices.push("有些调用读取失败，请刷新重试。");
    fresh.forEach((run, index) => addRun(run, results[index], false));
    for (const run of runs) placeLooseCalls(works.get(run.id), room);
  }
  function addMessages(room, data) {
    const seen = new Set(room.items.map(item => item.id));
    let oldest = messagePages.get(room.id)?.before || 0;
    for (const message of data.messages || []) {
      if (message.seq && (oldest === 0 || message.seq < oldest)) oldest = message.seq;
      const id = "message-" + message.id;
      if (seen.has(id)) continue;
      const person = actor(message.author_id);
      const at = Date.parse(message.created_at);
      room.items.push({
        id, kind: "say", person, at, raw: message.created_at, time: Number.isFinite(at) ? timeAt(at) : "",
        who: person === "you" ? "你" : (people.get(person)?.name || (message.author_id === "system" ? "系统" : "成员")),
        text: message.body, where: room.place + " · " + room.name, stepRef: linkRef(room, message.id)
      });
    }
    messagePages.set(room.id, { before: oldest, hasEarlier: !!data.has_earlier && oldest > 0 });
  }

  const messagePages = new Map();
  const runPages = new Map();
  const ready = new Set();
  const preparing = new Map();
  const runLoads = new Map();

  function viewKey(view) {
    if (view?.person) return "member:" + view.person;
    if (!view?.room || view.room.id === "personal") return "personal";
    return "room:" + view.room.id;
  }
  function filterOf(view) {
    if (view?.person) return { member: view.person };
    if (!view?.room || view.room.id === "personal") return { personal: "1" };
    return { conversation: view.room.id };
  }
  function viewCount(view) {
    if (view?.person) return feeds.get(view.person)?.length || 0;
    return view?.room?.items.length || 0;
  }
  function runsURL(filter, page) {
    const params = new URLSearchParams({ limit: "40" });
    if (filter.member) params.set("member", filter.member);
    if (filter.conversation) params.set("conversation", filter.conversation);
    if (filter.personal) params.set("personal", "1");
    if (page?.before) {
      params.set("before", page.before);
      params.set("before_id", page.id);
    }
    return "/api/runs?" + params;
  }
  async function loadRunDetails(runs, personId) {
    const fresh = runs.filter(run => run?.id && !works.has(run.id));
    if (fresh.length) {
      const results = await Promise.allSettled(fresh.map(run => read("/api/runs/" + encodeURIComponent(run.id))));
      if (results.some(result => result.status === "rejected")) notices.push("有些执行步骤读取失败，请刷新重试。");
      fresh.forEach((run, index) => addRun(run, results[index], !personId));
    }
    if (personId) for (const run of runs) placeMemberSteps(works.get(run.id), personId);
  }
  async function loadRuns(view, next) {
    const key = viewKey(view);
    const page = runPages.get(key);
    if (next && !page?.hasEarlier) return;
    const data = await read(runsURL(filterOf(view), next ? page : null));
    const runs = data.runs || [];
    if (runs.length) {
      const oldest = runs[runs.length - 1];
      runPages.set(key, { before: oldest.started_at, id: oldest.id, hasEarlier: !!data.has_earlier });
    } else {
      runPages.set(key, { ...(page || {}), hasEarlier: false });
    }
    for (const gap of data.gaps || []) if (gap.note) notices.push(gap.note);
    await loadRunDetails(runs, view?.person || "");
  }
  async function loadMessagePage(room) {
    const page = messagePages.get(room.id);
    if (!page?.hasEarlier) return;
    const data = await read("/api/conversations/" + encodeURIComponent(room.id) + "/messages?before=" + page.before);
    addMessages(room, data);
  }
  function rememberDeliveries(room, list) {
    const map = new Map();
    for (const record of list || []) if (record.message_id) map.set(record.message_id, record);
    deliveryByRoom.set(room.id, map);
  }
  async function loadRoom(room) {
    const [messages, deliveries] = await Promise.all([
      read("/api/conversations/" + encodeURIComponent(room.id) + "/messages").catch(() => {
        notices.push("有些会话的消息读取失败，已保留对应的观测记录。");
        return { messages: [] };
      }),
      read("/api/conversations/" + encodeURIComponent(room.id) + "/deliveries").catch(() => {
        notices.push("有些消息的来源读取失败，请刷新重试。");
        return { deliveries: [] };
      })
    ]);
    rememberDeliveries(room, deliveries.deliveries);
    addMessages(room, messages);
    await loadLooseCalls(room, false);
    room.blurb = room.blurb || "消息和执行步骤按时间排列";
    room.featured = room.place === "群";
  }
  async function prepare(view) {
    const key = viewKey(view);
    if (ready.has(key)) return;
    if (preparing.has(key)) return preparing.get(key);
    const job = (async () => {
      if (view?.room && view.room.id !== "personal") await loadRoom(view.room);
      else await loadRuns(view, false);
      sortFeeds();
      ready.add(key);
    })();
    preparing.set(key, job);
    try {
      await job;
    } finally {
      preparing.delete(key);
    }
  }
  function runView(view) {
    return !!(view?.person || !view?.room || view.room.id === "personal");
  }
  async function earlier(view) {
    await prepare(view);
    const before = viewCount(view);
    if (runView(view)) {
      if (!runPages.get(viewKey(view))?.hasEarlier) return 0;
      await loadRuns(view, true);
    } else {
      if (messagePages.get(view.room.id)?.hasEarlier) await loadMessagePage(view.room);
      if (runPages.get(viewKey(view))?.hasEarlier) await loadLooseCalls(view.room, true);
    }
    sortFeeds();
    return Math.max(0, viewCount(view) - before);
  }
  function hasEarlier(view) {
    if (runView(view)) return !!runPages.get(viewKey(view))?.hasEarlier;
    if (!view?.room) return false;
    return !!messagePages.get(view.room.id)?.hasEarlier || !!runPages.get(viewKey(view))?.hasEarlier;
  }
  function runRecord(ref) {
    if (ref?.id && ref.started_at) return ref;
    return {
      id: ref.id || ref.work, member_id: ref.member_id || ref.memberId || "",
      conversation_id: ref.conversation_id || ref.conversationId || "",
      kind: ref.kind || "", root_id: ref.root_id || ref.rootId || "",
      execution: ref.execution || 1, status: ref.status || "", note: ref.note || "",
      started_at: ref.started_at || "", work_id: ref.work_id || ref.workId || ""
    };
  }
  function ensureRun(ref) {
    const id = ref?.id || ref?.work;
    if (!id) return Promise.resolve(null);
    if (works.has(id)) return Promise.resolve(works.get(id));
    if (runLoads.has(id)) return runLoads.get(id);
    const pending = (async () => {
      const data = await read("/api/runs/" + encodeURIComponent(id));
      addRun(runRecord(ref), { status: "fulfilled", value: data }, false);
      sortFeeds();
      return works.get(id) || null;
    })();
    runLoads.set(id, pending);
    pending.catch(() => runLoads.delete(id));
    return pending;
  }
  const activities = new Map();
  function activitySnapshot(state) {
    return { cost: state.cost, runs: state.runs.slice(), hasEarlier: state.hasEarlier, rootId: state.rootId };
  }
  async function loadActivity(state) {
    const jobs = [];
    if (!state.cost) {
      jobs.push(read("/api/costs?root=" + encodeURIComponent(state.rootId) + "&period=all").then(cost => { state.cost = cost; }));
    }
    const params = new URLSearchParams({ root: state.rootId, limit: "40" });
    if (state.cursor) {
      params.set("before", state.cursor.at);
      params.set("before_id", state.cursor.id);
    }
    jobs.push(read("/api/runs?" + params).then(page => {
      const runs = page.runs || [];
      const seen = new Set(state.runs.map(run => run.id));
      for (const run of runs) if (run.id && !seen.has(run.id)) state.runs.push(run);
      if (runs.length) {
        const oldest = runs[runs.length - 1];
        state.cursor = { at: oldest.started_at, id: oldest.id };
      }
      state.hasEarlier = !!page.has_earlier && runs.length > 0;
    }));
    await Promise.all(jobs);
  }
  function activity(rootId) {
    if (!rootId) return Promise.resolve(null);
    let state = activities.get(rootId);
    if (!state) {
      state = { rootId, runs: [], hasEarlier: false, cursor: null, cost: null, loading: null };
      activities.set(rootId, state);
      state.loading = loadActivity(state).then(() => activitySnapshot(state)).catch(error => {
        activities.delete(rootId);
        throw error;
      });
    }
    return state.loading;
  }
  function activityMore(rootId) {
    const state = activities.get(rootId);
    if (!state) return activity(rootId);
    state.loading = Promise.resolve(state.loading).then(() => {
      if (!state.hasEarlier) return activitySnapshot(state);
      return loadActivity(state).then(() => activitySnapshot(state));
    });
    return state.loading;
  }

  const account = { value: null, error: "" };
  let accountToken = 0;
  async function loadAccount(next) {
    const token = ++accountToken;
    account.value = null;
    account.error = "";
    const params = new URLSearchParams({ period: "all" });
    if (next.scope === "person") params.set("member", next.person);
    else if (next.room === "personal") params.set("personal", "1");
    else params.set("conversation", next.room);
    try {
      const body = await read("/api/costs?" + params);
      if (token !== accountToken) return false;
      account.value = body;
    } catch {
      if (token !== accountToken) return false;
      account.error = "暂时读不到账本。";
    }
    return true;
  }

  const firstRoom = orderedRooms.find(room => room.place === "群") || orderedRooms[0];
  if (firstRoom) {
    try { await prepare({ room: firstRoom }); }
    catch { notices.push("有些执行步骤读取失败，请刷新重试。"); }
  }
  const defaultRoom = firstRoom?.id || "personal";
  await loadAccount({ scope: "room", room: defaultRoom });

  return {
    people: [...people.values()], rooms: orderedRooms, defaultRoom, ledger: true,
    get account() { return account.value; },
    get accountError() { return account.error; },
    get notice() { return [...new Set(notices)].join(" "); },
    loadAccount, prepare, earlier, hasEarlier, activity, activityMore, ensureRun,
    workById: id => works.get(id),
    roomFeed: room => room?.items || [],
    personFeed: person => feeds.get(person?.id) || []
  };
}

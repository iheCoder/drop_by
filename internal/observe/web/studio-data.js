function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, ch => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
}

function money(micros) {
  return "$" + (micros / 1e6).toFixed(6);
}

function dur(ms) {
  if (!ms) return "-";
  if (ms < 1000) return ms + " ms";
  const seconds = ms / 1000;
  return (seconds >= 10 ? seconds.toFixed(0) : seconds.toFixed(1)) + " s";
}

function priceLabel(step) {
  if (!step) return "";
  if (step.costKind === "budget" || step.costKind === "not_sent") return "未发送";
  if (step.costKind === "released") return "不另计";
  if (step.costKind === "running" && step.reserve) return "进行中 " + money(step.reserve);
  if (step.reserve) return "预留 " + money(step.reserve);
  if (step.costStatus === "unknown") return "金额待确认";
  if (step.price || step.costStatus === "settled") return money(step.price || 0);
  return "不另计";
}

function costLine(work) {
  const seen = new Set();
  let estimate = 0;
  let running = 0;
  let reserved = 0;
  let pending = false;
  for (const step of work.steps || []) {
    const key = step.ledgerId ? "call:" + step.ledgerId : "step:" + step.id;
    if (seen.has(key)) continue;
    seen.add(key);
    if (step.costKind === "budget" || step.costKind === "not_sent" || step.costKind === "released") continue;
    if (step.costKind === "running") {
      running += step.reserve || 0;
      continue;
    }
    if (step.reserve) {
      reserved += step.reserve;
      continue;
    }
    if (step.costStatus === "unknown") {
      pending = true;
      continue;
    }
    if (step.price) estimate += step.price;
  }
  return [
    estimate ? money(estimate) + " 用量估算" : "",
    running ? "进行中 " + money(running) : "",
    reserved ? "预留 " + money(reserved) : "",
    pending ? "有金额待确认" : ""
  ].filter(Boolean).join(" · ") || "没有模型费用";
}

export const display = { esc, money, dur, priceLabel, costLine };

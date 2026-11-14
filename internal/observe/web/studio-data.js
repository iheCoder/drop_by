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
  if (step.reserve) return "预留 " + money(step.reserve);
  if (step.costStatus === "unknown") return "金额待确认";
  if (step.price || step.costStatus === "settled") return money(step.price);
  return "不另计";
}

function costLine(work) {
  const settled = work.steps.reduce((sum, step) => sum + (step.reserve ? 0 : step.price || 0), 0);
  const reserved = work.steps.reduce((sum, step) => sum + (step.reserve || 0), 0);
  const pending = work.steps.some(step => !step.reserve && step.costStatus === "unknown");
  return [settled ? money(settled) + " 已结算" : "", reserved ? "预留 " + money(reserved) : "", pending ? "有金额待确认" : ""]
    .filter(Boolean).join(" · ") || "没有模型费用";
}

export const display = { esc, money, dur, priceLabel, costLine };

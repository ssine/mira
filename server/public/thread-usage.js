const count = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
const exactFormat = new Intl.NumberFormat("en-US");
const compactFormat = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 });

export function normalizeTokenUsage(value) {
  const usage = { inputTokens: count(value?.input_tokens), outputTokens: count(value?.output_tokens),
    cachedInputTokens: count(value?.cached_input_tokens) };
  return Object.values(usage).some(value => value !== null) ? usage : null;
}

export function tokenCount(value) {
  return count(value) === null ? "未提供" : exactFormat.format(value);
}

export function compactTokenCount(value) {
  if (count(value) === null) return null;
  return compactFormat.format(value).replace("K", "k");
}

export function compactTokenUsage(usage) {
  const input = compactTokenCount(usage?.inputTokens), output = compactTokenCount(usage?.outputTokens);
  return input !== null && output !== null ? `${input} in · ${output} out` : "";
}

export function tokenUsageTitle(usage) {
  return `累计输入 ${tokenCount(usage?.inputTokens)} Token（含缓存）\n缓存输入 ${tokenCount(usage?.cachedInputTokens)} Token\n累计输出 ${tokenCount(usage?.outputTokens)} Token`;
}

const moneyFormat = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 4 });
export function formatEstimatedCost(value) {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return "暂无法估算";
  return value > 0 && value < 0.0001 ? "< $0.0001" : moneyFormat.format(value);
}

export function compactCost(estimate) {
  const value = estimate?.amount;
  if (value == null) return "—";
  const amount = value > 0 && value < 0.001 ? "<$0.001"
    : new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", maximumFractionDigits: value < 0.01 ? 3 : 2 }).format(value);
  return `${value > 0 && value < 0.001 ? "" : "≈"}${amount}${estimate.status === "partial" ? "*" : ""}`;
}

export function threadTimestamp(value, now = new Date()) {
  if (!value) return "—";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "—";
  const day = time => Date.UTC(time.getFullYear(), time.getMonth(), time.getDate()) / 86400_000;
  const days = day(now) - day(date);
  if (days === 0) return new Intl.DateTimeFormat("zh-CN", {hour: "2-digit", minute: "2-digit", hourCycle: "h23"}).format(date);
  if (days > 0 && days <= 7) return new Intl.DateTimeFormat("zh-CN", {weekday: "long"}).format(date);
  return `${date.getFullYear() === now.getFullYear() ? "" : date.getFullYear() + "/"}${String(date.getMonth() + 1).padStart(2, "0")}/${String(date.getDate()).padStart(2, "0")}`;
}

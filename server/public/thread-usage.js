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

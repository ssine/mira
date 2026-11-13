const weeklyMinutes = 7 * 24 * 60;

export function weeklyQuota(result) {
  const snapshot = result?.rateLimitsByLimitId?.codex ?? result?.rateLimits;
  const codex = snapshot && (!snapshot.limitId || snapshot.limitId === "codex") ? snapshot : null;
  const window = [codex?.primary, codex?.secondary].find(value => value?.windowDurationMins === weeklyMinutes);
  const remaining = Number.isFinite(window?.usedPercent) ? Math.max(0, Math.min(100, 100 - window.usedPercent)) : null;
  const seconds = window?.resetsAt;
  const resetsAt = Number.isFinite(seconds) && seconds > 0 && seconds < 8.64e12 ? seconds * 1000 : null;
  const count = result?.rateLimitResetCredits?.availableCount;
  return { remaining, resetsAt, resetCount: Number.isSafeInteger(count) && count >= 0 ? count : null };
}

// Claude.ai plan windows as reported by the Node (used percentage, Unix ms).
// Remaining percentages mirror Codex so both engines read the same way.
export function claudePlanQuota(usage) {
  const window = (label, value, key) => {
    if (!value) return null;
    const used = value.utilization;
    const remaining = Number.isFinite(used) ? Math.max(0, Math.min(100, 100 - used)) : null;
    const resetsAt = Number.isFinite(value.resetsAt) && value.resetsAt > 0 ? value.resetsAt : null;
    return remaining === null && resetsAt === null ? null : { key, label, remaining, resetsAt };
  };
  const windows = [
    window("5 小时", usage?.fiveHour, "fiveHour"),
    window("本周 · 全部模型", usage?.sevenDay, "sevenDay"),
    window("本周 · Opus", usage?.sevenDayOpus, "sevenDayOpus"),
    window("本周 · Sonnet", usage?.sevenDaySonnet, "sevenDaySonnet"),
    ...(Array.isArray(usage?.models) ? usage.models : []).slice(0, 16)
      .map((value, index) => window(`本周 · ${value?.name}`, value, `model:${index}`)),
  ].filter(Boolean);
  const weekly = windows.find(value => value.key === "sevenDay"), session = windows.find(value => value.key === "fiveHour");
  return { windows, remaining: weekly?.remaining ?? null, resetsAt: weekly?.resetsAt ?? null, resetCount: null, session: session ?? null };
}

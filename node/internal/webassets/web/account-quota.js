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

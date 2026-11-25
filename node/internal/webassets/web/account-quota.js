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

const dayMs = 24 * 60 * 60_000;
const instant = value => Number.isFinite(value) && value > 0 && value < 8.64e15 ? value : null;

// The next monthly anniversary of a subscription, in UTC. Months without the
// original day renew on their last day.
export function nextMonthlyRenewal(startedAt, now = Date.now()) {
  if (instant(startedAt) === null || startedAt > now) return null;
  const start = new Date(startedAt), today = new Date(now);
  const months = (today.getUTCFullYear() - start.getUTCFullYear()) * 12 + today.getUTCMonth() - start.getUTCMonth();
  for (let offset = Math.max(1, months); ; offset++) {
    const lastDay = new Date(Date.UTC(start.getUTCFullYear(), start.getUTCMonth() + offset + 1, 0)).getUTCDate();
    const renewal = new Date(startedAt);
    renewal.setUTCDate(1);
    renewal.setUTCMonth(start.getUTCMonth() + offset);
    renewal.setUTCDate(Math.min(start.getUTCDate(), lastDay));
    if (+renewal > now) return +renewal;
  }
}

// One account may be logged in on several Nodes. The login token refreshed most
// recently has the current billing period; periods are never combined.
export function codexPlanPeriod(reports) {
  const known = reports.filter(report => instant(report?.endsAt) !== null);
  known.sort((a, b) => (instant(b.checkedAt) ?? 0) - (instant(a.checkedAt) ?? 0) || b.endsAt - a.endsAt);
  return known.length ? { kind: "exact", endsAt: known[0].endsAt, startsAt: instant(known[0].startsAt), checkedAt: instant(known[0].checkedAt) } : null;
}

function localDay(text) {
  const match = /^(\d{4})-(\d\d)-(\d\d)$/.exec(text ?? "");
  return match ? new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3])) : null;
}

// A manually entered day wins until it has passed; otherwise the end is
// estimated, because Claude stores only when the subscription began.
export function claudePlanPeriod(endsOn, subscribedAt, now = Date.now()) {
  const day = localDay(endsOn);
  if (day && now < +new Date(day.getFullYear(), day.getMonth(), day.getDate() + 1)) return { kind: "manual", day: +day };
  const endsAt = nextMonthlyRenewal(subscribedAt, now);
  return endsAt === null ? null : { kind: "estimate", endsAt, subscribedAt };
}

export function planPeriodView(period, now = Date.now()) {
  if (!period) return { text: "未提供", level: "", title: "本地登录信息中没有计费周期" };
  const year = timestamp => new Date(timestamp).getFullYear() === new Date(now).getFullYear() ? {} : { year: "numeric" };
  const date = timestamp => new Intl.DateTimeFormat("zh-CN", { ...year(timestamp), month: "2-digit", day: "2-digit" }).format(timestamp);
  const full = timestamp => new Date(timestamp).toLocaleString("zh-CN", { hour12: false });
  if (period.kind === "manual") {
    const today = new Date(now); today.setHours(0, 0, 0, 0);
    const days = Math.round((period.day - today) / dayMs); // A daylight-saving shift moves midnight by an hour.
    return { text: `${date(period.day)} · ${days ? `还剩 ${days} 天` : "今天"}`, level: days <= 1 ? "danger" : days <= 7 ? "warning" : "",
      title: "来自账号设置中填写的日期" };
  }
  const left = period.endsAt - now;
  const source = period.kind === "estimate" ? `按 ${full(period.subscribedAt)} 开始订阅、每月续期推算` :
    `${period.startsAt ? `本期从 ${full(period.startsAt)} 开始；` : ""}来自本地登录信息${period.checkedAt ? `，更新于 ${full(period.checkedAt)}` : ""}`;
  if (left <= 0) return { text: "等待更新", level: "", title: `上次记录的结束时间：${full(period.endsAt)}；${source}` };
  const clock = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(period.endsAt);
  const remaining = left >= dayMs ? `还剩 ${Math.floor(left / dayMs)} 天` : left >= 3_600_000 ? `还剩 ${Math.floor(left / 3_600_000)} 小时` : "不足 1 小时";
  return { text: `${period.kind === "estimate" ? "约 " : ""}${date(period.endsAt)} ${clock} · ${remaining}`,
    level: left < dayMs ? "danger" : left <= 7 * dayMs ? "warning" : "", title: `${full(period.endsAt)}（本地时间）；${source}` };
}

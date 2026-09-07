export const projectThreadVisibleLimit = 4;
export const projectThreadRecentWindowMs = 7 * 24 * 60 * 60 * 1000;

export function compareThreadsByRecency(left, right) {
  return (Date.parse(right?.updatedAt) || 0) - (Date.parse(left?.updatedAt) || 0)
    || String(right?.threadId ?? "").localeCompare(String(left?.threadId ?? ""));
}

export function splitProjectThreads(threads, now = Date.now()) {
  const cutoff = new Date(now).getTime() - projectThreadRecentWindowMs;
  const visible = [], hidden = [];
  for (const thread of [...threads].sort(compareThreadsByRecency)) {
    const updatedAt = Date.parse(thread?.updatedAt);
    if (visible.length < projectThreadVisibleLimit && Number.isFinite(updatedAt) && updatedAt >= cutoff) visible.push(thread);
    else hidden.push(thread);
  }
  return { visible, hidden };
}

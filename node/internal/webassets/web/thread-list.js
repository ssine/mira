export const projectThreadVisibleLimit = 4;
export const projectThreadRecentWindowMs = 7 * 24 * 60 * 60 * 1000;

export function compareThreadsByRecency(left, right) {
  return (Date.parse(right?.updatedAt) || 0) - (Date.parse(left?.updatedAt) || 0)
    || String(right?.threadId ?? "").localeCompare(String(left?.threadId ?? ""));
}

// Group before assigning projects: a child can run on a different Node or cwd.
// These wrappers are only a view; every original thread keeps its own metadata.
export function buildThreadTree(threads) {
  const byId = new Map([...threads].sort(compareThreadsByRecency).map(thread => [thread.threadId,
    { thread, threadId: thread.threadId, updatedAt: thread.updatedAt, children: [], descendantCount: 0 }]));
  const parentById = new Map();
  const roots = [];
  for (const entry of byId.values()) {
    const parentId = entry.thread.parentThreadId;
    let ancestor = parentId;
    while (ancestor && ancestor !== entry.threadId) ancestor = parentById.get(ancestor);
    // Missing/filtered parents and broken cycles must never hide a conversation.
    if (byId.has(parentId) && ancestor !== entry.threadId) {
      parentById.set(entry.threadId, parentId);
      byId.get(parentId).children.push(entry);
    } else roots.push(entry);
  }
  const order = [...roots];
  for (let index = 0; index < order.length; index++) order.push(...order[index].children);
  for (const entry of order.reverse()) {
    for (const child of entry.children) {
      entry.descendantCount += 1 + child.descendantCount;
      if ((Date.parse(child.updatedAt) || 0) > (Date.parse(entry.updatedAt) || 0)) entry.updatedAt = child.updatedAt;
    }
    entry.children.sort(compareThreadsByRecency);
  }
  roots.sort(compareThreadsByRecency);
  return { roots, parentById };
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

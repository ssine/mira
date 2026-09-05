// Activity is a rebuildable view of canonical rollout lifecycle events, never a
// second conversation store. Cache only immutable generation/item-count reads.
const cache = new WeakMap();
const starts = new Set(["task_started", "turn_started"]);
const ends = new Set(["task_complete", "turn_complete", "turn_aborted"]);
function terminalError(payload) {
  if (payload?.type !== "error" || payload.will_retry === true || payload.willRetry === true) return false;
  const info = payload.codex_error_info;
  const type = typeof info === "string" ? info : Object.keys(info ?? {})[0];
  return !["threadrollbackfailed", "activeturnnotsteerable"].includes(String(type).replaceAll("_", "").toLowerCase());
}

export function projectThreadActivity(rows, exhausted = true) {
  const markers = rows.filter(row => row.payload?.type === "event_msg" &&
    (starts.has(row.payload.payload?.type) || ends.has(row.payload.payload?.type) || terminalError(row.payload.payload)));
  const startIndex = markers.findIndex(row => starts.has(row.payload.payload.type));
  const start = markers[startIndex];
  if (!start && !exhausted) return { state: "unknown", turnId: null, reason: "history" };
  const turnId = start?.payload.payload.turn_id;
  // A late completion for an older turn must not stop the current turn.
  const end = (start ? markers.slice(0, startIndex) : markers).find(row =>
    ends.has(row.payload.payload.type) && (!turnId || !row.payload.payload.turn_id || row.payload.payload.turn_id === turnId));
  const error = start && markers.slice(0, startIndex).find(row => terminalError(row.payload.payload) &&
    (!row.payload.payload.turn_id || row.payload.payload.turn_id === turnId));
  const marker = end ?? error ?? start;
  if (!marker) return { state: exhausted ? "idle" : "unknown", turnId: null, reason: exhausted ? null : "history" };
  const payload = marker.payload.payload;
  return {
    state: end ? (payload.type === "turn_aborted" ? "interrupted" : payload.error || error ? "failed" : "idle") : error ? "failed" : "running",
    turnId: turnId ?? payload.turn_id ?? null,
    startedAt: start?.payload.timestamp ?? null,
    updatedAt: marker.payload.timestamp ?? marker.created_at?.toISOString() ?? null,
  };
}

export async function addThreadActivities(pool, storeId, threads) {
  if (!threads.length) return threads;
  let entries = cache.get(pool);
  if (!entries) { entries = new Map(); cache.set(pool, entries); }
  const key = thread => JSON.stringify([storeId, thread.threadId, thread.generation, thread.itemCount]);
  const missing = threads.filter(thread => !entries.has(key(thread)));
  if (missing.length) {
    const result = await pool.query(`
      SELECT selected.thread_id, events.item_seq, events.payload, events.created_at
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) AS selected(thread_id, generation, item_count)
      LEFT JOIN LATERAL (
        SELECT item_seq, payload, created_at FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<=selected.item_count
          AND payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_started|turn_started|task_complete|turn_complete|turn_aborted|error)"'
        ORDER BY item_seq DESC LIMIT 32
      ) events ON TRUE
      ORDER BY selected.thread_id, events.item_seq DESC`,
    [storeId, missing.map(t => t.threadId), missing.map(t => t.generation), missing.map(t => t.itemCount)]);
    const byThread = new Map();
    for (const row of result.rows) {
      if (!byThread.has(row.thread_id)) byThread.set(row.thread_id, []);
      if (row.item_seq != null) byThread.get(row.thread_id).push(row);
    }
    for (const thread of missing) {
      const rows = byThread.get(thread.threadId) ?? [];
      entries.set(key(thread), projectThreadActivity(rows, rows.length < 32));
    }
  }
  // Reachability is deliberately not cached: a running marker is insufficient
  // evidence after a Node disappears or its managed runtime restarts.
  const projected = new Map(threads.map(thread => [thread.threadId, entries.get(key(thread))]));
  while (entries.size > 1000) entries.delete(entries.keys().next().value);
  const nodeIds = [...new Set(threads.map(t => t.runtimeNodeId || t.sourceNodeId).filter(Boolean))];
  const nodes = nodeIds.length ? await pool.query(`SELECT node_id, approval_status, last_seen_at,
    channel_status, reported_app_server FROM codex_nodes WHERE node_id=ANY($1::uuid[])`, [nodeIds]) : { rows: [] };
  const byNode = new Map(nodes.rows.map(node => [node.node_id, node]));
  const result = threads.map(thread => {
    const activity = { ...projected.get(thread.threadId), generation: thread.generation, itemCount: thread.itemCount };
    if (activity.state === "running") {
      const node = byNode.get(thread.runtimeNodeId || thread.sourceNodeId);
      let reason = null;
      if ((thread.importedAt && Date.parse(activity.startedAt) < Date.parse(thread.importedAt)) ||
          (thread.createdAt && Date.parse(activity.startedAt) < Date.parse(thread.createdAt))) reason = "history";
      else if (!node) reason = "unbound";
      else if (node.approval_status !== "approved" || node.channel_status?.connected !== true || Date.now() - Date.parse(node.last_seen_at) >= 15_000) reason = "offline";
      else if (thread.runtimeNodeId && Date.parse(thread.runtimeBoundAt) <= Date.parse(activity.startedAt) &&
        (node.reported_app_server?.status !== "running" || Date.parse(node.reported_app_server?.startedAt) > Date.parse(activity.startedAt))) reason = "runtime";
      if (reason) { activity.state = "unknown"; activity.reason = reason; }
    }
    return { ...thread, activity };
  });
  return result;
}

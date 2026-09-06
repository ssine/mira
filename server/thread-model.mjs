import { modelEventPredicate } from "./thread-cost-migration.mjs";

const caches = new WeakMap();
const validModel = value => typeof value === "string" && value.trim() ? value : null;
export async function addThreadModels(pool, storeId, threads) {
  let cache = caches.get(pool);
  if (!cache) { cache = new Map(); caches.set(pool, cache); }
  const key = thread => JSON.stringify([storeId, thread.threadId, thread.generation, thread.itemCount]);
  let pending = threads.filter(thread => !validModel(thread.model) && !cache.has(key(thread)))
    .map(thread => ({ ...thread, before: thread.itemCount + 1 }));
  while (pending.length) {
    const result = await pool.query(`SELECT selected.thread_id,events.item_seq,events.payload
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) selected(thread_id,generation,before)
      LEFT JOIN LATERAL (
        SELECT item_seq,payload FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<selected.before AND ${modelEventPredicate}
        ORDER BY item_seq DESC LIMIT 1
      ) events ON TRUE`, [storeId, pending.map(t => t.threadId), pending.map(t => t.generation), pending.map(t => t.before)]);
    const rows = new Map(result.rows.map(row => [row.thread_id, row]));
    const next = [];
    for (const thread of pending) {
      const row = rows.get(thread.threadId), record = row?.payload;
      const model = validModel(record?.type === "turn_context" ? record.payload?.model
        : record?.type === "event_msg" && record.payload?.type === "thread_settings_applied"
          ? record.payload.thread_settings?.model : null);
      if (model || row?.item_seq == null) cache.set(key(thread), model);
      else next.push({ ...thread, before: Number(row.item_seq) });
    }
    pending = next;
  }
  const result = threads.map(thread => ({ ...thread, model: validModel(thread.model) ?? cache.get(key(thread)) ?? null }));
  while (cache.size > 1000) cache.delete(cache.keys().next().value);
  return result;
}

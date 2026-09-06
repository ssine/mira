import { normalizeTokenUsage } from "./public/thread-usage.js";
import { tokenUsagePredicate } from "./thread-token-usage-migration.mjs";

// Older imported rollouts may predate the runtime's metadata projection. Read
// their last cumulative snapshot; summing token_count snapshots double-counts.
const caches = new WeakMap();

export async function addThreadTokenUsage(pool, storeId, threads) {
  let cache = caches.get(pool);
  if (!cache) { cache = new Map(); caches.set(pool, cache); }
  const key = thread => JSON.stringify([storeId, thread.threadId, thread.generation, thread.itemCount]);
  const projected = new Map(threads.map(thread => [thread.threadId, normalizeTokenUsage(thread.tokenUsage)]));
  let pending = threads.filter(thread => !projected.get(thread.threadId) && !cache.has(key(thread)))
    .map(thread => ({ ...thread, before: thread.itemCount + 1 }));
  while (pending.length) {
    const result = await pool.query(`SELECT selected.thread_id,events.item_seq,events.payload
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) selected(thread_id,generation,before)
      LEFT JOIN LATERAL (
        SELECT item_seq,payload FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<selected.before AND ${tokenUsagePredicate}
        ORDER BY item_seq DESC LIMIT 1
      ) events ON TRUE`, [storeId, pending.map(t => t.threadId), pending.map(t => t.generation), pending.map(t => t.before)]);
    const rows = new Map(result.rows.map(row => [row.thread_id, row]));
    const next = [];
    for (const thread of pending) {
      const row = rows.get(thread.threadId);
      // Canonical payload is JSON (not JSONB) and can contain escaped NUL. Keep
      // structural validation in JS; PostgreSQL JSON extraction rejects such rows.
      const record = row?.payload;
      const usage = record?.type === "event_msg" && record.payload?.type === "token_count"
        ? normalizeTokenUsage(record.payload.info?.total_token_usage) : null;
      if (usage || row?.item_seq == null) cache.set(key(thread), usage);
      else next.push({ ...thread, before: Number(row.item_seq) });
    }
    pending = next;
  }
  const result = threads.map(thread => ({ ...thread,
    tokenUsage: projected.get(thread.threadId) ?? cache.get(key(thread)) ?? null }));
  while (cache.size > 1000) cache.delete(cache.keys().next().value);
  return result;
}

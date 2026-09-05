import { lockScope } from "./storage-rows.mjs";
import { threadUpdatePredicate } from "./thread-read-state-migration.mjs";

const updates = new WeakMap();
function visibleUpdate(record) {
  const payload = record?.payload;
  if (record?.type === "event_msg") {
    return ["user_message", "agent_message", "item_completed", "view_image_tool_call", "task_complete", "turn_complete", "turn_aborted"].includes(payload?.type) ||
      (payload?.type === "error" && payload.will_retry !== true && payload.willRetry !== true);
  }
  return record?.type === "response_item" && ((payload?.type === "message" && ["user", "assistant"].includes(payload.role)) ||
    ["function_call_output", "custom_tool_call_output"].includes(payload?.type));
}

async function latestThreadUpdates(pool, storeId, threads) {
  let cache = updates.get(pool);
  if (!cache) { cache = new Map(); updates.set(pool, cache); }
  const key = t => JSON.stringify([storeId, t.threadId, t.generation, t.itemCount]);
  let pending = threads.filter(t => !cache.has(key(t))).map(t => ({ ...t, before: t.itemCount + 1 }));
  while (pending.length) {
    const result = await pool.query(`SELECT selected.thread_id, events.item_seq, events.payload
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) selected(thread_id,generation,before)
      LEFT JOIN LATERAL (
        SELECT item_seq,payload FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<selected.before AND ${threadUpdatePredicate}
        ORDER BY item_seq DESC LIMIT 1
      ) events ON TRUE ORDER BY selected.thread_id,events.item_seq DESC`,
    [storeId, pending.map(t => t.threadId), pending.map(t => t.generation), pending.map(t => t.before)]);
    const grouped = Map.groupBy(result.rows.filter(row => row.item_seq != null), row => row.thread_id);
    const next = [];
    for (const thread of pending) {
      const rows = grouped.get(thread.threadId) ?? [];
      const latest = rows.find(row => visibleUpdate(row.payload));
      if (latest || !rows.length) cache.set(key(thread), Number(latest?.item_seq ?? 0));
      else next.push({ ...thread, before: Number(rows.at(-1).item_seq) });
    }
    pending = next;
  }
  const result = new Map(threads.map(thread => [thread.threadId, cache.get(key(thread))]));
  while (cache.size > 1000) cache.delete(cache.keys().next().value);
  return result;
}

// Read positions belong to the single administrator, shared by all Web clients.
// They are independent of rebuildable conversation projections and never write
// Codex history or affect its version, generation, title or recency.
export async function addThreadReadStates(pool, storeId, threads) {
  if (!threads.length) return threads;
  const latest = await latestThreadUpdates(pool, storeId, threads);
  const result = await pool.query(`
    SELECT selected.thread_id, selected.generation, reads.item_count AS read_count
    FROM unnest($2::text[], $3::bigint[]) selected(thread_id,generation)
    LEFT JOIN mira_thread_read_positions reads ON reads.store_id=$1
      AND reads.thread_id=selected.thread_id AND reads.generation=selected.generation`,
  [storeId, threads.map(t => t.threadId), threads.map(t => t.generation)]);
  const states = new Map(result.rows.map(row => [row.thread_id, {
    generation: Number(row.generation), latestItemSeq: latest.get(row.thread_id),
    readItemCount: Number(row.read_count ?? 0),
    unread: latest.get(row.thread_id) > Number(row.read_count ?? 0),
  }]));
  return threads.map(thread => ({ ...thread, readState: states.get(thread.threadId) }));
}

export async function markThreadRead(pool, storeId, threadId, body) {
  if (!Number.isSafeInteger(body?.generation) || body.generation < 1 ||
      !Number.isSafeInteger(body?.itemCount) || body.itemCount < 0) {
    return { status: 400, body: { error: "无效的已读位置", code: "invalid_request" } };
  }
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    await lockScope(client, storeId, [threadId]);
    const result = await client.query(`SELECT active_generation, item_count FROM codex_thread_projections
      WHERE store_id=$1 AND thread_id=$2`, [storeId, threadId]);
    if (!result.rowCount) {
      await client.query("ROLLBACK");
      return { status: 404, body: { error: "会话不存在或已删除", code: "not_found" } };
    }
    const head = result.rows[0];
    if (body.generation !== Number(head.active_generation) || body.itemCount > Number(head.item_count)) {
      await client.query("ROLLBACK");
      return { status: 409, body: { error: "会话历史已变化，请重新读取", code: "thread_changed" } };
    }
    const saved = await client.query(`INSERT INTO mira_thread_read_positions(store_id,thread_id,generation,item_count)
      VALUES($1,$2,$3,$4) ON CONFLICT(store_id,thread_id) DO UPDATE
      SET generation=EXCLUDED.generation,
        item_count=CASE WHEN mira_thread_read_positions.generation=EXCLUDED.generation
          THEN GREATEST(mira_thread_read_positions.item_count,EXCLUDED.item_count) ELSE EXCLUDED.item_count END,
        updated_at=NOW()
      RETURNING generation,item_count`, [storeId, threadId, body.generation, body.itemCount]);
    await client.query("COMMIT");
    return { status: 200, body: { threadId, generation: Number(saved.rows[0].generation), readItemCount: Number(saved.rows[0].item_count) } };
  } catch (error) { await client.query("ROLLBACK"); throw error; }
  finally { client.release(); }
}

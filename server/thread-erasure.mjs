// Only explicit permanent deletion may erase canonical history. The deletion
// fence and new store head commit first; this durable queue removes old bytes in
// bounded transactions without holding the store's writer lock.
export async function threadErasureStatus(pool, storeId) {
  const result = await pool.query(`SELECT count(*)::int AS pending
    FROM mira_thread_erasures WHERE store_id=$1 AND phase<>'complete'`, [storeId]);
  return result.rows[0];
}

export async function processThreadErasureBatch(pool, { storeId = null, eventBatchSize = 16, itemBatchSize = 256 } = {}) {
  if (!Number.isInteger(eventBatchSize) || eventBatchSize < 1 || eventBatchSize > 64 ||
      !Number.isInteger(itemBatchSize) || itemBatchSize < 1 || itemBatchSize > 1024) throw new Error("invalid erasure batch size");
  const client = await pool.connect();
  let job;
  try {
    await client.query("BEGIN");
    const selected = await client.query(`SELECT * FROM mira_thread_erasures
      WHERE phase<>'complete' AND retry_at<=NOW() AND ($1::text IS NULL OR store_id=$1)
      ORDER BY updated_at,store_id,thread_id LIMIT 1 FOR UPDATE SKIP LOCKED`, [storeId]);
    job = selected.rows[0];
    if (!job) { await client.query("ROLLBACK"); return null; }
    const keys = [job.store_id, job.thread_id];
    let phase = job.phase, cursor = job.after_event_seq, changed = 0;
    if (phase === "events") {
      const result=await client.query(`DELETE FROM codex_store_state_changes WHERE ctid IN (
        SELECT ctid FROM codex_store_state_changes WHERE store_id=$1 AND thread_id=$2 LIMIT $3
      )`, [...keys,eventBatchSize]);
      changed=result.rowCount;
      cursor=Number(cursor)+changed;
      if(!changed) phase="history";
    } else if (phase === "history") {
      const result = await client.query(`DELETE FROM codex_thread_events WHERE ctid IN (
        SELECT ctid FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2 LIMIT $3
      )`, [...keys, itemBatchSize]);
      changed = result.rowCount;
      if (!changed) phase = "provenance";
    } else if (phase === "provenance") {
      // A surviving imported fork may still own these immutable source records.
      await client.query(`DELETE FROM mira_codex_session_import_segments segments USING mira_codex_session_imports imports
        WHERE segments.import_id=imports.import_id AND imports.store_id=$1 AND imports.thread_id=$2
        AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)`, keys);
      const result = await client.query(`DELETE FROM mira_codex_session_import_records WHERE ctid IN (
        SELECT records.ctid FROM mira_codex_session_import_records records JOIN mira_codex_session_imports imports
          ON records.import_id=imports.import_id
        WHERE imports.store_id=$1 AND imports.thread_id=$2
        AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)
        AND NOT EXISTS(SELECT 1 FROM mira_codex_session_import_segments WHERE source_import_id=imports.import_id)
        LIMIT $3
      )`, [...keys, itemBatchSize]);
      changed = result.rowCount;
      if (!changed) {
        await client.query(`DELETE FROM mira_codex_session_imports imports WHERE store_id=$1 AND thread_id=$2
          AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)
          AND NOT EXISTS(SELECT 1 FROM mira_codex_session_import_segments WHERE source_import_id=imports.import_id)`, keys);
        phase = "complete";
      }
    }
    await client.query(`UPDATE mira_thread_erasures SET phase=$3,after_event_seq=$4,updated_at=NOW(),
      last_error_code=NULL,completed_at=CASE WHEN $3='complete' THEN NOW() ELSE NULL END
      WHERE store_id=$1 AND thread_id=$2`, [...keys, phase, cursor]);
    await client.query("COMMIT");
    return { phase, changed, afterEventSeq: Number(cursor) };
  } catch (error) {
    await client.query("ROLLBACK");
    if (job) await client.query(`UPDATE mira_thread_erasures
      SET retry_at=NOW()+INTERVAL '5 seconds',last_error_code=$3 WHERE store_id=$1 AND thread_id=$2`,
    [job.store_id, job.thread_id, String(error.code ?? "erasure_failed").slice(0, 80)]);
    throw error;
  } finally { client.release(); }
}

export function startThreadErasureWorker(pool) {
  let stopped = false, timer, running = Promise.resolve();
  const tick = async () => {
    let worked = false;
    try { worked = Boolean(await processThreadErasureBatch(pool)); }
    catch (error) { console.error("thread erasure batch failed", { code: error.code ?? "erasure_failed" }); }
    if (!stopped) { timer = setTimeout(run, worked ? 200 : 3000); timer.unref?.(); }
  };
  const run = () => { running = tick(); };
  run();
  return async () => { stopped = true; clearTimeout(timer); await running; };
}

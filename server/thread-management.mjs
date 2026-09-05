import { commitDelta, getStoreHead } from "./thread-store.mjs";

// Use Codex's existing name field and append-only store commits. Renaming never
// loads or rewrites transcript items and works while the execution Node is offline.
export async function renameThread(pool, storeId, threadId, body) {
  body ??= {};
  const name = typeof body.name === "string" ? body.name.trim() : "";
  if (!name || name.length > 200 || /[\u0000-\u001f\u007f]/.test(name) ||
      !(body.expectedName === null || typeof body.expectedName === "string") ||
      !Number.isSafeInteger(body.generation) || body.generation < 1 ||
      typeof body.operationId !== "string" || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(body.operationId)) {
    return { status: 400, body: { error: "请填写 1–200 字的单行标题。", code: "invalid_request" } };
  }
  const head = await getStoreHead(pool, storeId);
  const entry = head.historyManifest[threadId];
  if (!entry) return { status: 404, body: { error: "会话不存在或已不可访问", code: "not_found" } };
  const names = head.state.names ?? {};
  const expected = body.expectedName === null && !Object.hasOwn(names, threadId)
    ? { exists: false } : { exists: true, value: body.expectedName };
  const result = await commitDelta(pool, storeId, {
    expectedVersion: head.version,
    stateChanges: [{ path: ["names", threadId], mode: "set", conflictPolicy: "compareAndSwap", expected, value: name }],
    historyChanges: [{ threadId, mode: "append", expectedGeneration: body.generation, expectedItemCount: entry.itemCount, items: [] }],
  }, { "x-codex-operation-id": body.operationId, "x-codex-version": "mira-web" });
  if (result.status === 409) return { status: 409, body: { error: "此会话已在其他窗口更新，请重新打开编辑后再保存。", code: "thread_changed" } };
  return result;
}

export async function manageThread(pool, storeId, threadId, action, body) {
  if (!["archive", "restore", "delete"].includes(action) || !Number.isSafeInteger(body?.generation) || body.generation < 1 ||
      typeof body.operationId !== "string" || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(body.operationId) ||
      (action === "delete" && (!Number.isSafeInteger(body.itemCount) || body.itemCount < 0))) {
    return { status: 400, body: { error: "无效的会话操作参数", code: "invalid_request" } };
  }
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    await client.query("SELECT version FROM codex_store_heads WHERE store_id=$1 FOR UPDATE", [storeId]);
    const duplicate = await client.query("SELECT thread_id, action, generation, item_count FROM mira_thread_actions WHERE store_id=$1 AND operation_id=$2", [storeId, body.operationId]);
    if (duplicate.rowCount) {
      const old = duplicate.rows[0];
      await client.query("ROLLBACK");
      return old.thread_id === threadId && old.action === action && Number(old.generation) === body.generation && (action !== "delete" || Number(old.item_count) === body.itemCount)
        ? { status: 200, body: { threadId, action, duplicate: true } }
        : { status: 409, body: { error: "操作标识已被其他操作使用", code: "operation_conflict" } };
    }
    const head = await getStoreHead(client, storeId);
    const entry = head.historyManifest[threadId];
    if (!entry) { await client.query("ROLLBACK"); return { status: 404, body: { error: "会话不存在或已删除", code: "not_found" } }; }
    if (entry.generation !== body.generation || (action === "delete" && entry.itemCount !== body.itemCount)) {
      await client.query("ROLLBACK");
      return { status: 409, body: { error: "会话内容已变化，请等当前运行结束后重新操作。", code: "thread_changed" } };
    }
    await client.query("INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation,item_count) VALUES($1,$2,$3,$4,$5,$6)", [storeId, threadId, action, body.operationId, body.generation, action === "delete" ? body.itemCount : null]);
    if (action === "delete") {
      // Explicit administrator deletion removes content from every generation
      // and old event metadata. The content-free action fences stale writers.
      await client.query(`UPDATE codex_store_events SET state=mira_without_thread_state(state,$2), history_manifest=history_manifest-$2
        WHERE store_id=$1`, [storeId, threadId]);
      await client.query("DELETE FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query("DELETE FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query("DELETE FROM codex_thread_store_snapshots WHERE store_id=$1", [storeId]);
      await client.query("DELETE FROM mira_codex_thread_runtimes WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query("UPDATE mira_appserver_thread_start_requests SET response='{\"deleted\":true}'::jsonb WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query(`DELETE FROM mira_codex_session_import_segments segments USING mira_codex_session_imports imports
        WHERE segments.import_id=imports.import_id AND imports.store_id=$1 AND imports.thread_id=$2
        AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)`, [storeId, threadId]);
      // Source segments shared by surviving imported forks belong to those forks
      // too; retain them. Unreferenced source records are physically removed.
      await client.query(`DELETE FROM mira_codex_session_import_records records USING mira_codex_session_imports imports
        WHERE records.import_id=imports.import_id AND imports.store_id=$1 AND imports.thread_id=$2
        AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)
        AND NOT EXISTS(SELECT 1 FROM mira_codex_session_import_segments WHERE source_import_id=imports.import_id)`, [storeId, threadId]);
      await client.query(`DELETE FROM mira_codex_session_imports imports WHERE store_id=$1 AND thread_id=$2
        AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)
        AND NOT EXISTS(SELECT 1 FROM mira_codex_session_import_segments WHERE source_import_id=imports.import_id)`, [storeId, threadId]);
      const advanced = await client.query("UPDATE codex_store_heads SET version=version+1,updated_at=NOW() WHERE store_id=$1 RETURNING version::text", [storeId]);
      await client.query(`INSERT INTO codex_store_events(store_id,event_seq,previous_event_seq,operation_id,event_format_version,codex_version,state,history_manifest)
        SELECT store_id,$2,event_seq,$3,event_format_version,'mira-web',state,history_manifest FROM codex_store_events WHERE store_id=$1 AND event_seq=$4`,
      [storeId, advanced.rows[0].version, body.operationId, head.version]);
    }
    await client.query("COMMIT");
    return { status: 200, body: { threadId, action } };
  } catch (error) { await client.query("ROLLBACK"); throw error; }
  finally { client.release(); }
}

import { commitDelta, getStoreHead } from "./thread-store.mjs";
import { lockScope, beginReceipt, publishReceipt } from "./storage-rows.mjs";

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
  const head = await getStoreHead(pool, storeId, [threadId]);
  const entry = head.historyManifest[threadId];
  if (!entry) return { status: 404, body: { error: "会话不存在或已不可访问", code: "not_found" } };
  const names = head.state.names ?? {};
  const expected = body.expectedName === null && !Object.hasOwn(names, threadId)
    ? { exists: false } : { exists: true, value: body.expectedName };
  const result = await commitDelta(pool, storeId, {
    expectedVersion: head.version,
    stateChanges: [{ path: ["names", threadId], mode: "set", conflictPolicy: "compareAndSwap", expected, value: name }],
    historyChanges: [{ threadId, mode: "append", expectedGeneration: body.generation, expectedItemCount: entry.itemCount, items: [] }],
  }, { "x-codex-operation-id": body.operationId, "x-codex-version": "mira-web" }, { action: 'rename', threadId, ...body, name });
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
  let inTransaction = false;
  const replay = async () => {
    const duplicate = await client.query(`SELECT actions.thread_id,actions.action,actions.generation,actions.item_count,erasures.phase AS erasure_phase
      FROM mira_thread_actions actions LEFT JOIN mira_thread_erasures erasures USING(store_id,thread_id)
      WHERE actions.store_id=$1 AND actions.operation_id=$2`, [storeId, body.operationId]);
    if (!duplicate.rowCount) return null;
    const old = duplicate.rows[0];
    return old.thread_id === threadId && old.action === action && Number(old.generation) === body.generation && (action !== "delete" || Number(old.item_count) === body.itemCount)
      ? { status: 200, body: { threadId, action, duplicate: true, ...(action === "delete" ? { cleanupPending: Boolean(old.erasure_phase && old.erasure_phase !== "complete") } : {}) } }
      : { status: 409, body: { error: "操作标识已被其他操作使用", code: "operation_conflict" } };
  };
  const validate = (head) => {
    const entry = head.historyManifest[threadId];
    if (!entry) return { status: 404, body: { error: "会话不存在或已删除", code: "not_found" } };
    if (entry.generation !== body.generation || (action === "delete" && entry.itemCount !== body.itemCount)) {
      return { status: 409, body: { error: "会话内容已变化，请等当前运行结束后重新操作。", code: "thread_changed" } };
    }
    return null;
  };
  try {
    await client.query("BEGIN");
    inTransaction = true;
    if (action === "delete") {
      // Claim the operation before taking thread locks, like ordinary commits.
      // A reused UUID must not invert the receipt/thread lock order.
      const receipt = await beginReceipt(client, storeId, body.operationId, { threadId, action, ...body }, "mira-web");
      if (receipt) {
        const result = receipt.status === 200 ? await replay() : receipt;
        await client.query("ROLLBACK");
        return result ?? { status: 409, body: { error: "操作标识已被其他操作使用", code: "operation_conflict" } };
      }
    }
    await lockScope(client, storeId, [threadId]);
    const duplicate = await replay();
    if (duplicate) {
      await client.query("ROLLBACK");
      return duplicate;
    }
    const head = await getStoreHead(client, storeId, [threadId]);
    const invalid = validate(head);
    if (invalid) { await client.query("ROLLBACK"); return invalid; }
    const recorded = await client.query("INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation,item_count) VALUES($1,$2,$3,$4,$5,$6) RETURNING action_seq", [storeId, threadId, action, body.operationId, body.generation, action === "delete" ? body.itemCount : null]);
    if (action === "delete") {
      // Commit the access/write fence and a clean current head atomically.
      // Historical erasure runs separately in bounded per-thread batches.
      await client.query("DELETE FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query("DELETE FROM codex_store_state_entries WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query("DELETE FROM mira_codex_thread_runtimes WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query("UPDATE mira_appserver_thread_start_requests SET response='{\"deleted\":true}'::jsonb WHERE store_id=$1 AND thread_id=$2", [storeId, threadId]);
      await client.query(`INSERT INTO mira_thread_erasures(store_id,thread_id,action_seq,through_event_seq)
        VALUES($1,$2,$3,$4)`, [storeId, threadId, recorded.rows[0].action_seq, head.version]);
      await client.query(`INSERT INTO codex_thread_revisions(store_id,thread_id,operation_id,generation,item_count,active)
        VALUES($1,$2,$3,$4,$5,false)`, [storeId,threadId,body.operationId,body.generation,body.itemCount]);
      await publishReceipt(client,storeId,body.operationId,new Set());
    }
    await client.query("COMMIT");
    return { status: 200, body: { threadId, action, ...(action === "delete" ? { cleanupPending: true } : {}) } };
  } catch (error) { if (inTransaction) await client.query("ROLLBACK"); throw error; }
  finally { client.release(); }
}

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

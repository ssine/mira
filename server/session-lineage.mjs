import { stageSessionTransfer } from "./session-transfer.mjs";

const invalid = (message) => Object.assign(new Error(message), { code: "invalid_history_base", statusCode: 409 });
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// Follow upstream RolloutLineage: references identify immutable rollout IDs,
// each segment skips its own session_meta and stops at the exact byte/ordinal
// boundary. Publish a self-contained legacy history with the child's own meta.
// Ancestors are provenance, not new independent live threads.
export async function stageSessionLineage(pool, service, principal, nodeId, summary, storeId, request, staged, context) {
  const lineage = [staged];
  const seen = new Set();
  let current = staged;
  let source = summary;
  while (current.meta.history_base) {
    context.signal?.throwIfAborted();
    const base = current.meta.history_base;
    if (!uuid.test(base.thread_id ?? "") || !Number.isSafeInteger(base.end_ordinal_exclusive) || base.end_ordinal_exclusive < 1 ||
        !Number.isSafeInteger(base.end_byte_offset) || base.end_byte_offset < 1) throw invalid("祖先历史引用格式无效");
    if (seen.has(base.thread_id)) throw invalid("祖先历史引用包含循环");
    seen.add(base.thread_id);
    context.onProgress?.({ phase: "resolving", ancestors: lineage.length });
    source = await service.invoke(principal, nodeId, "codexSessions", {
      action: "resolve", path: source.path, rolloutId: base.thread_id,
    }, { request, timeoutMs: 120_000, auditMetadata: { purpose: "codex_session_lineage" } });
    if (!source?.path || !uuid.test(source.threadId ?? "")) throw invalid("找不到有效的祖先历史源文件，请更新节点并检查源会话");
    current = await stageSessionTransfer(pool, service, principal, nodeId, source, storeId, request, { ...context, endPosition: base });
    lineage.push(current);
  }
  const segments = [{ importId: staged.importId, firstLine: 1, count: 1, boundary: null }];
  for (const part of lineage.reverse()) {
    if (part.count > 1) segments.push({ importId: part.importId, firstLine: 2, count: part.count - 1, boundary: part.boundary });
  }
  return { segments, count: segments.reduce((sum, part) => sum + part.count, 0), ancestorCount: lineage.length - 1 };
}

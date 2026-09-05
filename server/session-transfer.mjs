import crypto from "node:crypto";
import { assertThreadsNotDeleted } from "./thread-store.mjs";

// The temporary table is transaction-local. A cancelled or broken transfer
// rolls it back; a complete source becomes immutable import provenance.
export async function stageSessionTransfer(pool, capabilityService, principal, nodeId, summary, storeId, request, context = {}) {
  const { signal, onProgress = () => {} } = context;
  const boundary = context.endPosition;
  const totalBytes = boundary?.end_byte_offset ?? summary.sizeBytes;
  if (!Number.isSafeInteger(totalBytes) || totalBytes <= 0 || totalBytes > summary.sizeBytes) {
    throw Object.assign(new Error("祖先历史字节边界无效"), { code: "invalid_history_base" });
  }
  const client = await pool.connect();
  let cursor = 0;
  let count = 0;
  let meta = null;
  let carry = "";
  let modifiedAt = null;
  let nextOrdinal = null;
  const hash = crypto.createHash("sha256");
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const check = () => signal?.throwIfAborted();
  let batch = [];
  let batchBytes = 0;
  const flush = async () => {
    if (!batch.length) return;
    check();
    const params = batch.flat();
    const tuples = batch.map((_, i) => `($${i * 3 + 1},$${i * 3 + 2}::json,$${i * 3 + 3})`);
    await client.query(`INSERT INTO mira_session_transfer (line_seq,raw_record,raw_sha256) VALUES ${tuples.join(",")}`, params);
    batch = [];
    batchBytes = 0;
  };
  const record = async (line) => {
    if (!line.trim()) return;
    check();
    let value;
    try { value = JSON.parse(line); } catch { throw Object.assign(new Error(`Invalid JSONL record ${count + 1}`), { code: "invalid_session_jsonl" }); }
    if (!value || typeof value.type !== "string" || !value.payload || typeof value.payload !== "object" || Array.isArray(value.payload)) {
      throw Object.assign(new Error(`Invalid rollout envelope at record ${count + 1}`), { code: "invalid_session_record" });
    }
    if (!count && value.type !== "session_meta") throw new Error("First rollout record must be session_meta");
    if (value.type === "session_meta" && !meta) meta = value.payload;
    if (boundary) {
      nextOrdinal ??= meta.history_base?.end_ordinal_exclusive ?? 0;
      if (!Number.isSafeInteger(value.ordinal) || value.ordinal !== nextOrdinal++) {
        throw Object.assign(new Error("祖先历史序号与引用边界不一致"), { code: "invalid_history_base" });
      }
    }
    batch.push([++count, line, crypto.createHash("sha256").update(line).digest("hex")]);
    batchBytes += Buffer.byteLength(line);
    if (batch.length >= 100 || batchBytes >= 4 * 1024 * 1024) await flush();
  };
  try {
    await client.query("BEGIN");
    await client.query("CREATE TEMP TABLE mira_session_transfer (line_seq BIGINT PRIMARY KEY, raw_record JSON NOT NULL, raw_sha256 TEXT NOT NULL) ON COMMIT DROP");
    onProgress({ phase: "reading", bytes: 0, totalBytes, records: 0, ancestor: Boolean(boundary) });
    for (;;) {
      check();
      const chunk = await capabilityService.invoke(principal, nodeId, "codexSessions", {
        action: "read", path: summary.path, cursor, limit: Math.min(4 * 1024 * 1024, totalBytes - cursor), encoding: "base64",
      }, { request, timeoutMs: 120_000, auditMetadata: { purpose: "codex_session_import" } });
      check();
      if (chunk.cursor !== cursor || typeof chunk.content !== "string" || !Number.isSafeInteger(chunk.nextCursor) ||
          chunk.nextCursor < cursor || typeof chunk.eof !== "boolean") throw new Error("Invalid Node session chunk");
      modifiedAt ??= chunk.modifiedAt ?? null;
      if (chunk.sizeBytes !== summary.sizeBytes || (modifiedAt && (modifiedAt !== chunk.modifiedAt ||
          modifiedAt !== summary.modifiedAt))) {
        throw Object.assign(new Error("源会话在导入期间发生变化，请停止桌面端写入后重新扫描"), { code: "session_changed" });
      }
      const bytes = Buffer.from(chunk.content, chunk.encoding === "base64" ? "base64" : "utf8");
      if (chunk.nextCursor - cursor !== bytes.length || (!chunk.eof && !bytes.length)) throw new Error("Invalid session chunk length");
      if (chunk.nextCursor > totalBytes) throw new Error("Node read past requested source boundary");
      const done = chunk.nextCursor === totalBytes;
      if (boundary && done && bytes.at(-1) !== 10) throw Object.assign(new Error("祖先字节边界必须位于完整 JSONL 记录末尾"), { code: "invalid_history_base" });
      hash.update(bytes);
      carry += decoder.decode(bytes, { stream: !done });
      let from = 0;
      for (;;) {
        const end = carry.indexOf("\n", from);
        if (end < 0) break;
        await record(carry.slice(from, end));
        from = end + 1;
      }
      carry = carry.slice(from);
      cursor = chunk.nextCursor;
      await flush();
      onProgress({ phase: "reading", bytes: cursor, totalBytes, records: count, ancestor: Boolean(boundary) });
      if (done) { await record(carry); await flush(); break; }
      if (chunk.eof) throw new Error("Incomplete source prefix");
    }
    if (cursor !== totalBytes || !count || meta?.id !== summary.threadId) throw new Error("Incomplete session or mismatched thread identity");
    if (boundary && nextOrdinal !== boundary.end_ordinal_exclusive) throw Object.assign(new Error("祖先字节边界与序号边界不一致"), { code: "invalid_history_base" });
    check();
    const sha256 = hash.digest("hex");
    await client.query("SELECT version FROM codex_store_heads WHERE store_id=$1 FOR UPDATE", [storeId]);
    // A surviving fork may still explicitly import an ancestor as its provenance.
    // Publishing the deleted thread itself is forbidden, including a staging race.
    if (!boundary) await assertThreadsNotDeleted(client, storeId, [meta.id]);
    const inserted = await client.query(`INSERT INTO mira_codex_session_imports
      (store_id, thread_id, source_node_id, source_path, source_sha256, source_size_bytes,
       source_modified_at, source_codex_version, source_item_count, source_boundary, status)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,'staged')
      ON CONFLICT (source_node_id, source_path, source_sha256) DO NOTHING RETURNING import_id`,
    [storeId, meta.id, nodeId, summary.path, sha256, cursor, summary.modifiedAt, summary.codexVersion ?? null, count, JSON.stringify(boundary ?? null)]);
    let importId = inserted.rows[0]?.import_id;
    const duplicate = !importId;
    if (importId) {
      await client.query(`INSERT INTO mira_codex_session_import_records (import_id, line_seq, raw_record, raw_sha256)
        SELECT $1, line_seq, raw_record, raw_sha256 FROM mira_session_transfer ORDER BY line_seq`, [importId]);
    } else {
      const existing = await client.query(`SELECT import_id FROM mira_codex_session_imports
        WHERE source_node_id=$1 AND source_path=$2 AND source_sha256=$3`, [nodeId, summary.path, sha256]);
      importId = existing.rows[0].import_id;
    }
    check();
    await client.query("COMMIT");
    return { importId, meta, count, sizeBytes: cursor, duplicate, boundary: boundary ?? null };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally { client.release(); }
}

import crypto from "node:crypto";

import { appendAudit } from "./auth.mjs";
import { commitDelta, getStoreHead, getThreadHistory } from "./thread-store.mjs";

const defaultStoreId = process.env.MIRA_CODEX_STORE_ID ?? "personal";
const maximumImportBytes = Number.parseInt(process.env.MIRA_MAX_SESSION_IMPORT_BYTES ?? "268435456", 10);
const maximumImportRecords = 500_000;

function safeStoreId(value) {
  return typeof value === "string" && /^[a-zA-Z0-9._-]{1,128}$/.test(value) ? value : null;
}

function stableValue(value) {
  if (Array.isArray(value)) return value.map(stableValue);
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stableValue(value[key])]));
  }
  return value;
}

function sameJson(left, right) {
  return JSON.stringify(stableValue(left)) === JSON.stringify(stableValue(right));
}

function isPrefix(prefix, value) {
  return prefix.length <= value.length && prefix.every((item, index) => sameJson(item, value[index]));
}

function validThreadId(value) {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

function baseInstructions(value) {
  if (typeof value === "string") return { text: value };
  if (value && typeof value === "object" && typeof value.text === "string") return value;
  return { text: "" };
}

function canonicalRolloutItem(record) {
  const payload = structuredClone(record.payload);
  // Older JSONL files persisted base instructions as a string. The supported
  // ThreadStore representation uses the upstream BaseInstructions object.
  // The unmodified source record remains in mira_codex_session_import_records.
  if (record.type === "session_meta" && Object.hasOwn(payload, "base_instructions")) {
    payload.base_instructions = baseInstructions(payload.base_instructions);
  }
  // The current remote HTTP ThreadStore exposes the complete rollout through
  // the legacy history API. Local Codex stores may already use paginated
  // history, but persisting that flag would make App Server call list_turns /
  // list_items, which this adapter intentionally does not advertise yet. The
  // untouched source record remains in mira_codex_session_import_records.
  if (record.type === "session_meta") payload.history_mode = "legacy";
  return { type: record.type, payload };
}

function legacyCompatibleHistory(items) {
  return items.map((item) => item?.type === "session_meta" ? canonicalRolloutItem(item) : item);
}

function dateOrNull(value) {
  if (typeof value !== "string" || Number.isNaN(Date.parse(value))) return null;
  return new Date(value).toISOString();
}

function createdThread(meta, threadId) {
  const sessionId = validThreadId(meta.session_id) ? meta.session_id : threadId;
  const windowId = validThreadId(meta.context_window?.window_id) ? meta.context_window.window_id : threadId;
  return {
    source: meta.source ?? "cli",
    metadata: {
      cwd: typeof meta.cwd === "string" ? meta.cwd : null,
      memory_mode: meta.memory_mode === "disabled" ? "disabled" : "enabled",
      model_provider: typeof meta.model_provider === "string" ? meta.model_provider : "openai",
    },
    thread_id: threadId,
    originator: typeof meta.originator === "string" ? meta.originator : "Codex",
    session_id: sessionId,
    extra_config: null,
    history_base: meta.history_base ?? null,
    // Imported sessions are adapted to the history API supported by Mira's
    // remote ThreadStore. The source mode is retained in import provenance.
    history_mode: "legacy",
    dynamic_tools: Array.isArray(meta.dynamic_tools) ? meta.dynamic_tools : [],
    selected_capability_roots: Array.isArray(meta.selected_capability_roots) ? meta.selected_capability_roots : [],
    thread_source: meta.thread_source ?? null,
    forked_from_id: validThreadId(meta.forked_from_id) ? meta.forked_from_id : null,
    parent_thread_id: validThreadId(meta.parent_thread_id) ? meta.parent_thread_id : null,
    base_instructions: baseInstructions(meta.base_instructions),
    initial_window_id: windowId,
    multi_agent_version: meta.multi_agent_version ?? null,
    subagent_history_start_ordinal: Number.isSafeInteger(meta.subagent_history_start_ordinal)
      ? meta.subagent_history_start_ordinal : null,
  };
}

function metadataPatch(meta, summary) {
  const firstUserMessage = typeof summary.title === "string" ? summary.title : null;
  const createdAt = dateOrNull(summary.startedAt ?? meta.timestamp);
  const updatedAt = dateOrNull(summary.modifiedAt);
  return {
    preview: firstUserMessage,
    title: firstUserMessage,
    model_provider: typeof meta.model_provider === "string" ? meta.model_provider : "openai",
    created_at: createdAt,
    updated_at: updatedAt,
    advance_recency_at: updatedAt,
    source: meta.source ?? "cli",
    cwd: typeof meta.cwd === "string" ? meta.cwd : null,
    cli_version: typeof meta.cli_version === "string" ? meta.cli_version : null,
    first_user_message: firstUserMessage,
    memory_mode: meta.memory_mode === "disabled" ? "disabled" : "enabled",
  };
}

async function readSession(capabilityService, principal, nodeId, summary, request) {
  const hash = crypto.createHash("sha256");
  const records = [];
  let cursor = 0;
  let sizeBytes = null;
  for (;;) {
    const chunk = await capabilityService.invoke(principal, nodeId, "codexSessions", {
      action: "read", path: summary.path, cursor, limit: 8 * 1024 * 1024,
    }, { request, timeoutMs: 120_000, auditMetadata: { purpose: "codex_session_import" } });
    if (chunk.cursor !== cursor || typeof chunk.content !== "string" || !Number.isSafeInteger(chunk.nextCursor) ||
        chunk.nextCursor < cursor || typeof chunk.eof !== "boolean") {
      throw Object.assign(new Error("Node returned an invalid Codex session chunk"), { code: "invalid_node_response" });
    }
    sizeBytes ??= chunk.sizeBytes;
    if (chunk.sizeBytes !== sizeBytes) {
      throw Object.assign(new Error("Codex session changed while it was being imported; scan and retry"), {
        code: "session_changed",
      });
    }
    if (!Number.isSafeInteger(sizeBytes) || sizeBytes < 0 || sizeBytes > maximumImportBytes) {
      throw Object.assign(new Error(`Codex session exceeds the ${maximumImportBytes} byte import limit`), { code: "session_too_large" });
    }
    const bytes = Buffer.from(chunk.content, "utf8");
    hash.update(bytes);
    for (const line of chunk.content.split("\n")) {
      if (!line.trim()) continue;
      let record;
      try { record = JSON.parse(line); } catch {
        throw Object.assign(new Error(`Codex session contains invalid JSON at record ${records.length + 1}`), { code: "invalid_session_jsonl" });
      }
      if (!record || typeof record !== "object" || Array.isArray(record) || typeof record.type !== "string" ||
          !record.payload || typeof record.payload !== "object" || Array.isArray(record.payload)) {
        throw Object.assign(new Error(`Codex session record ${records.length + 1} has an invalid rollout envelope`), { code: "invalid_session_record" });
      }
      records.push({ raw: record, rawSha256: crypto.createHash("sha256").update(line).digest("hex") });
      if (records.length > maximumImportRecords) {
        throw Object.assign(new Error(`Codex session exceeds the ${maximumImportRecords} record import limit`), { code: "session_too_many_records" });
      }
    }
    if (chunk.eof) {
      cursor = chunk.nextCursor;
      break;
    }
    if (chunk.nextCursor === cursor) throw new Error("Node did not advance the Codex session cursor");
    cursor = chunk.nextCursor;
  }
  if (cursor !== sizeBytes || cursor > maximumImportBytes || records.length === 0) {
    throw Object.assign(new Error("Codex session is empty or too large"), { code: "invalid_session" });
  }
  return { records, sourceSha256: hash.digest("hex"), sizeBytes };
}

async function insertStagedImport(pool, value) {
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const inserted = await client.query(
      `INSERT INTO mira_codex_session_imports (
         store_id, thread_id, source_node_id, source_path, source_sha256,
         source_size_bytes, source_modified_at, source_codex_version,
         source_item_count, status
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'staged')
       ON CONFLICT (source_node_id, source_path, source_sha256) DO NOTHING
       RETURNING import_id`,
      [value.storeId, value.threadId, value.nodeId, value.summary.path, value.sourceSha256,
        value.sizeBytes, dateOrNull(value.summary.modifiedAt), value.summary.codexVersion || null,
        value.records.length],
    );
    if (inserted.rowCount === 0) {
      const existing = await client.query(
        `SELECT import_id, status, store_event_seq::text FROM mira_codex_session_imports
         WHERE source_node_id = $1 AND source_path = $2 AND source_sha256 = $3`,
        [value.nodeId, value.summary.path, value.sourceSha256],
      );
      await client.query("ROLLBACK");
      return {
        importId: existing.rows[0].import_id, status: existing.rows[0].status,
        storeEventSeq: existing.rows[0].store_event_seq === null ? null : Number(existing.rows[0].store_event_seq),
        existing: true,
      };
    }
    const importId = inserted.rows[0].import_id;
    for (let offset = 0; offset < value.records.length; offset += 200) {
      const batch = value.records.slice(offset, offset + 200);
      const parameters = [];
      const tuples = batch.map((record, index) => {
        const base = index * 4;
        parameters.push(importId, offset + index + 1, JSON.stringify(record.raw), record.rawSha256);
        return `($${base + 1}, $${base + 2}, $${base + 3}::jsonb, $${base + 4})`;
      });
      await client.query(
        `INSERT INTO mira_codex_session_import_records (import_id, line_seq, raw_record, raw_sha256)
         VALUES ${tuples.join(",")}`,
        parameters,
      );
    }
    await client.query("COMMIT");
    return { importId, status: "staged", storeEventSeq: null, existing: false };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

async function markImport(pool, importId, status, storeEventSeq = null, errorCode = null) {
  await pool.query(
    `UPDATE mira_codex_session_imports SET status = $2, store_event_seq = $3,
       error_code = $4, updated_at = NOW() WHERE import_id = $1`,
    [importId, status, storeEventSeq, errorCode],
  );
}

export async function scanCodexSessions(pool, capabilityService, principal, nodeId, request) {
  const result = await capabilityService.invoke(principal, nodeId, "codexSessions", { action: "list" }, {
    request, timeoutMs: 120_000, auditMetadata: { purpose: "codex_session_scan" },
  });
  const paths = (result.sessions ?? []).map((session) => session.path);
  const imported = paths.length === 0 ? { rows: [] } : await pool.query(
    `SELECT DISTINCT ON (source_path) source_path, source_sha256, status, thread_id,
            store_id, source_size_bytes::text, source_modified_at, import_id, created_at
     FROM mira_codex_session_imports
     WHERE source_node_id = $1 AND source_path = ANY($2::text[])
     ORDER BY source_path, created_at DESC`,
    [nodeId, paths],
  );
  const byPath = new Map(imported.rows.map((row) => [row.source_path, row]));
  return {
    ...result,
    sessions: (result.sessions ?? []).map((session) => {
      const row = byPath.get(session.path);
      return { ...session, import: row ? {
        importId: row.import_id, status: row.status, threadId: row.thread_id,
        storeId: row.store_id, sourceSha256: row.source_sha256,
        unchanged: Number(row.source_size_bytes) === session.sizeBytes &&
          row.source_modified_at?.getTime() === new Date(session.modifiedAt).getTime(),
        importedAt: row.created_at.toISOString(),
      } : null };
    }),
  };
}

export async function importCodexSession(pool, capabilityService, principal, nodeId, body, request) {
  const storeId = safeStoreId(body.storeId ?? defaultStoreId);
  if (!storeId || typeof body.path !== "string" || body.path.length === 0 || body.path.length > 32_768) {
    return { status: 400, body: { error: "valid path and storeId are required", code: "invalid_request" } };
  }
  const scan = await scanCodexSessions(pool, capabilityService, principal, nodeId, request);
  const summary = scan.sessions.find((session) => session.path === body.path);
  if (!summary) return { status: 404, body: { error: "Codex session was not found in a detected default location", code: "not_found" } };

  const loaded = await readSession(capabilityService, principal, nodeId, summary, request);
  const sessionMetaRecord = loaded.records.find((record) => record.raw.type === "session_meta");
  const threadId = sessionMetaRecord?.raw?.payload?.id;
  if (!validThreadId(threadId)) {
    return { status: 409, body: { error: "Codex session has no valid thread id", code: "invalid_session" } };
  }
  const rolloutItems = loaded.records.map((record) => canonicalRolloutItem(record.raw));
  const staged = await insertStagedImport(pool, {
    ...loaded, storeId, threadId, nodeId, summary,
  });
  try {
    const head = await getStoreHead(pool, storeId);
    const existingCreated = head.state?.created_threads?.[threadId];
    const existingMetadata = head.state?.metadata_updates?.[threadId];
    const manifest = head.historyManifest?.[threadId];
    let existingItems = [];
    let desiredItems = rolloutItems;
    if (manifest) {
      const history = await getThreadHistory(pool, storeId, threadId, manifest.generation, head.version);
      if (history.status !== 200) throw Object.assign(new Error(history.body.error), { code: "history_read_failed" });
      existingItems = history.body.items;
      const compatibleExistingItems = legacyCompatibleHistory(existingItems);
      if (!isPrefix(compatibleExistingItems, rolloutItems) && !isPrefix(rolloutItems, compatibleExistingItems)) {
        await markImport(pool, staged.importId, "failed", null, "history_diverged");
        return { status: 409, body: {
          error: "This local session diverges from the PostgreSQL thread and was preserved as a staged import",
          code: "history_diverged", importId: staged.importId, threadId, storeId,
        } };
      }
      desiredItems = compatibleExistingItems.length < rolloutItems.length ? rolloutItems : compatibleExistingItems;
    }
    const stateChanges = [];
    if (!existingCreated) stateChanges.push({
      path: ["created_threads", threadId], mode: "set", conflictPolicy: "compareAndSwap",
      expected: { exists: false }, value: createdThread(sessionMetaRecord.raw.payload, threadId),
    });
    else if (existingCreated.history_mode !== "legacy") stateChanges.push({
      // This also repairs sessions imported before Mira normalized local
      // paginated history at the adapter boundary.
      path: ["created_threads", threadId, "history_mode"], mode: "set", conflictPolicy: "compareAndSwap",
      expected: { exists: true, value: existingCreated.history_mode ?? null }, value: "legacy",
    });
    if (!existingMetadata) stateChanges.push({
      path: ["metadata_updates", threadId], mode: "set", conflictPolicy: "compareAndSwap",
      expected: { exists: false }, value: metadataPatch(sessionMetaRecord.raw.payload, summary),
    });
    const historyChanges = [];
    if (!manifest) historyChanges.push({
      threadId, mode: "append", expectedGeneration: 0, expectedItemCount: 0, items: rolloutItems,
    });
    else if (!sameJson(existingItems, desiredItems)) {
      const appendOnly = isPrefix(existingItems, desiredItems);
      historyChanges.push({
        threadId, mode: appendOnly ? "append" : "replace", expectedGeneration: manifest.generation,
        expectedItemCount: existingItems.length,
        items: appendOnly ? desiredItems.slice(existingItems.length) : desiredItems,
      });
    }
    const operationId = crypto.randomUUID();
    const committed = await commitDelta(pool, storeId, {
      expectedVersion: head.version, stateChanges, historyChanges,
    }, {
      "x-codex-operation-id": operationId,
      "x-codex-version": summary.codexVersion || "local-jsonl-import",
    });
    if (committed.status !== 200) {
      await markImport(pool, staged.importId, "failed", null, "store_conflict");
      return { status: committed.status, body: { ...committed.body, importId: staged.importId } };
    }
    await markImport(pool, staged.importId, "imported", committed.body.version);
    await appendAudit(pool, {
      action: "codex_session.imported", principal, targetNodeId: nodeId, threadId, request,
      metadata: { importId: staged.importId, storeId, itemCount: rolloutItems.length, sourceBytes: loaded.sizeBytes },
    });
    return { status: 200, body: {
      importId: staged.importId, storeId, threadId, version: committed.body.version,
      duplicate: staged.existing || committed.body.noChange === true, itemCount: rolloutItems.length,
      parentThreadId: sessionMetaRecord.raw.payload.parent_thread_id ?? null,
    } };
  } catch (error) {
    await markImport(pool, staged.importId, "failed", null, error.code ?? "import_failed");
    throw error;
  }
}

export async function normalizeImportedThreadHistoryModes(pool) {
  const imported = await pool.query(
    `SELECT DISTINCT store_id, thread_id
     FROM mira_codex_session_imports
     WHERE status = 'imported'
     ORDER BY store_id, thread_id`,
  );
  const byStore = new Map();
  for (const row of imported.rows) {
    const threadIds = byStore.get(row.store_id) ?? [];
    threadIds.push(row.thread_id);
    byStore.set(row.store_id, threadIds);
  }
  let normalized = 0;
  for (const [storeId, threadIds] of byStore) {
    const head = await getStoreHead(pool, storeId);
    const stateChanges = [];
    const historyChanges = [];
    for (const threadId of threadIds) {
      const mode = head.state?.created_threads?.[threadId]?.history_mode;
      if (mode === "paginated") {
        stateChanges.push({
          path: ["created_threads", threadId, "history_mode"],
          mode: "set",
          conflictPolicy: "compareAndSwap",
          expected: { exists: true, value: mode },
          value: "legacy",
        });
      }
      const manifest = head.historyManifest?.[threadId];
      if (!manifest) continue;
      const history = await getThreadHistory(pool, storeId, threadId, manifest.generation, head.version);
      if (history.status !== 200) throw new Error(`failed to load imported thread ${threadId}: ${history.body.error}`);
      const compatibleItems = legacyCompatibleHistory(history.body.items);
      if (!sameJson(history.body.items, compatibleItems)) historyChanges.push({
        threadId,
        mode: "replace",
        expectedGeneration: manifest.generation,
        expectedItemCount: manifest.itemCount,
        items: compatibleItems,
      });
    }
    if (stateChanges.length === 0 && historyChanges.length === 0) continue;
    const committed = await commitDelta(pool, storeId, {
      expectedVersion: head.version,
      stateChanges,
      historyChanges,
    }, {
      "x-codex-operation-id": crypto.randomUUID(),
      "x-codex-version": "mira-import-compatibility",
    });
    if (committed.status !== 200) {
      throw new Error(`failed to normalize imported thread history modes in ${storeId}: ${committed.body.error}`);
    }
    normalized += new Set([
      ...stateChanges.map((change) => change.path[1]),
      ...historyChanges.map((change) => change.threadId),
    ]).size;
  }
  return normalized;
}

export async function listImportedThreads(pool, storeId = defaultStoreId, limit = 200) {
  const id = safeStoreId(storeId);
  if (!id) throw Object.assign(new Error("invalid store id"), { statusCode: 400, code: "invalid_request" });
  const result = await pool.query(
    `SELECT projections.thread_id, projections.parent_thread_id, projections.source_kind,
            projections.title, projections.cwd, projections.item_count::text,
            projections.active_generation::text, projections.updated_at,
            imports.import_id, imports.source_node_id, imports.source_codex_version,
            imports.created_at AS imported_at
     FROM codex_thread_projections projections
     LEFT JOIN LATERAL (
       SELECT import_id, source_node_id, source_codex_version, created_at
       FROM mira_codex_session_imports
       WHERE store_id = projections.store_id AND thread_id = projections.thread_id AND status = 'imported'
       ORDER BY created_at DESC LIMIT 1
     ) imports ON TRUE
     WHERE projections.store_id = $1
     ORDER BY projections.updated_at DESC LIMIT $2`,
    [id, limit],
  );
  return result.rows.map((row) => ({
    threadId: row.thread_id, parentThreadId: row.parent_thread_id,
    sourceKind: row.source_kind, title: row.title, cwd: row.cwd,
    itemCount: Number(row.item_count), generation: Number(row.active_generation),
    updatedAt: row.updated_at.toISOString(), importId: row.import_id,
    sourceNodeId: row.source_node_id, sourceCodexVersion: row.source_codex_version,
    importedAt: row.imported_at?.toISOString() ?? null,
  }));
}

export { defaultStoreId };

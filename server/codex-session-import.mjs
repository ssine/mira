import crypto from "node:crypto";

import { appendAudit } from "./auth.mjs";
import { assertThreadsNotDeleted, commitDelta, commitImportedHistory, getStoreHead, getThreadHistory } from "./thread-store.mjs";
import { stageSessionTransfer } from "./session-transfer.mjs";
import { stageSessionLineage } from "./session-lineage.mjs";
import { addThreadActivities } from "./thread-activity.mjs";
import { addThreadReadStates } from "./thread-read-state.mjs";

const defaultStoreId = process.env.MIRA_CODEX_STORE_ID ?? "personal";

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

function validThreadId(value) {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

function inferredExecutionMode(session, sourceNode) {
  if (typeof session.executionMode === "string" && session.executionMode.length > 0) {
    return session.executionMode;
  }
  if (
    sourceNode?.node_mode === "windows" &&
    typeof session.cwd === "string" &&
    /^\/(?!\/)/.test(session.cwd)
  ) {
    return "wsl";
  }
  return sourceNode?.node_mode ?? sourceNode?.platform ?? "unknown";
}

async function addSessionRuntimeHints(pool, sourceNodeId, sessions) {
  const nodes = await pool.query(
    `SELECT node_id, hostname, platform, node_mode, capabilities, last_seen_at,
            COALESCE((channel_status->>'connected')::boolean, false) AS connected
     FROM codex_nodes
     WHERE approval_status = 'approved'
     ORDER BY COALESCE((channel_status->>'connected')::boolean, false) DESC, last_seen_at DESC`,
  );
  const source = nodes.rows.find((node) => node.node_id === sourceNodeId);
  return sessions.map((session) => {
    const executionMode = inferredExecutionMode(session, source);
    let suggestedRuntime = null;
    if (executionMode === "wsl") {
      suggestedRuntime = nodes.rows.find((node) =>
        node.node_mode === "wsl" &&
        node.capabilities?.appServer === true &&
        (!source || node.hostname.toLowerCase() === source.hostname.toLowerCase())) ?? null;
    }
    return {
      ...session,
      executionMode,
      storageNodeId: sourceNodeId,
      suggestedRuntimeNodeId: suggestedRuntime?.node_id ?? null,
    };
  });
}

async function validImportRuntime(pool, runtimeNodeId) {
  if (runtimeNodeId === null || runtimeNodeId === undefined || runtimeNodeId === "") return null;
  if (!validThreadId(runtimeNodeId)) {
    throw Object.assign(new Error("Invalid Codex runtime node id"), { statusCode: 400, code: "invalid_request" });
  }
  const result = await pool.query(
    `SELECT node_id FROM codex_nodes
     WHERE node_id=$1 AND approval_status='approved' AND capabilities->>'appServer'='true'`,
    [runtimeNodeId],
  );
  if (result.rowCount === 0) {
    throw Object.assign(new Error("Selected node cannot run Codex App Server"), { statusCode: 409, code: "capability_unavailable" });
  }
  return result.rows[0].node_id;
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

async function markImport(pool, importId, status, storeEventSeq = null, errorCode = null) {
  await pool.query(
    `UPDATE mira_codex_session_imports SET status = $2, store_event_seq = $3,
       error_code = $4, updated_at = NOW() WHERE import_id = $1 AND (status <> 'imported' OR $2 = 'imported')`,
    [importId, status, storeEventSeq, errorCode],
  );
}

export async function scanCodexSessions(pool, capabilityService, principal, nodeId, request) {
  const result = await capabilityService.invoke(principal, nodeId, "codexSessions", { action: "list" }, {
    request, timeoutMs: 120_000, auditMetadata: { purpose: "codex_session_scan" },
  });
  const sessions = await addSessionRuntimeHints(pool, nodeId, result.sessions ?? []);
  const paths = sessions.map((session) => session.path);
  const imported = paths.length === 0 ? { rows: [] } : await pool.query(
    `SELECT DISTINCT ON (source_path) source_path, source_sha256, status, thread_id,
            store_id, source_size_bytes::text, source_modified_at, import_id, created_at
     FROM mira_codex_session_imports imports
     WHERE source_node_id = $1 AND source_path = ANY($2::text[])
       AND NOT EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=imports.store_id AND thread_id=imports.thread_id AND action='delete')
     ORDER BY source_path, created_at DESC`,
    [nodeId, paths],
  );
  const byPath = new Map(imported.rows.map((row) => [row.source_path, row]));
  return {
    ...result,
    sessions: sessions.map((session) => {
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

export async function importCodexSession(pool, capabilityService, principal, nodeId, body, request, context = {}) {
  const storeId = safeStoreId(body.storeId ?? defaultStoreId);
  if (!storeId || typeof body.path !== "string" || body.path.length === 0 || body.path.length > 32_768) {
    return { status: 400, body: { error: "valid path and storeId are required", code: "invalid_request" } };
  }
  context.signal?.throwIfAborted();
  context.onProgress?.({ phase: "scanning" });
  const scan = await scanCodexSessions(pool, capabilityService, principal, nodeId, request);
  const summary = scan.sessions.find((session) => session.path === body.path);
  if (!summary) return { status: 404, body: { error: "Session was not found in a detected local Codex directory", code: "not_found" } };
  if (!validThreadId(summary.threadId)) return { status: 409, body: { error: "Invalid thread id", code: "invalid_session" } };
  await assertThreadsNotDeleted(pool, storeId, [summary.threadId]);
  const runtimeNodeId = await validImportRuntime(
    pool,
    body.runtimeNodeId ?? summary.suggestedRuntimeNodeId,
  );
  const staged = await stageSessionTransfer(pool, capabilityService, principal, nodeId, summary, storeId, request, context);
  try {
    const expanded = await stageSessionLineage(pool, capabilityService, principal, nodeId, summary, storeId, request, staged, context);
    const meta = structuredClone(staged.meta);
    if (meta.history_base) {
      // Copied-fork semantics: raw references remain in provenance. Legacy
      // item indexes are no longer the source's paginated ordinal namespace.
      meta.history_base = null;
      meta.forked_from_ordinal_exclusive = null;
      meta.subagent_history_start_ordinal = null;
    }
    const normalize = (record) => {
      const item = canonicalRolloutItem(record);
      if (expanded.ancestorCount && item.type === "session_meta" && item.payload.id === meta.id) {
        item.payload.history_base = null;
        item.payload.forked_from_ordinal_exclusive = null;
        item.payload.subagent_history_start_ordinal = null;
      }
      return item;
    };
    // Upstream subagent ancestry is sometimes encoded only in source.
    const parent = meta.parent_thread_id ?? meta.source?.subagent?.thread_spawn?.parent_thread_id;
    const created = createdThread({ ...meta, parent_thread_id: parent }, meta.id);
    const committed = await commitImportedHistory(pool, storeId, {
      threadId: meta.id, importId: staged.importId, count: expanded.count, segments: expanded.segments,
      created, metadata: metadataPatch(meta, summary), normalize,
      codexVersion: summary.codexVersion || "local-jsonl-import",
    }, context);
    if (runtimeNodeId) {
      await pool.query(
        `INSERT INTO mira_codex_thread_runtimes (store_id, thread_id, node_id, bound_at)
         VALUES ($1,$2,$3,NOW())
         ON CONFLICT (store_id,thread_id) DO UPDATE SET node_id=EXCLUDED.node_id,bound_at=EXCLUDED.bound_at`,
        [storeId, meta.id, runtimeNodeId],
      );
    }
    await markImport(pool, staged.importId, "imported", committed.version);
    await appendAudit(pool, {
      action: "codex_session.imported", principal, targetNodeId: nodeId, threadId: meta.id, request,
      metadata: { importId: staged.importId, storeId, itemCount: staged.count, sourceBytes: staged.sizeBytes,
        executionMode: summary.executionMode, runtimeNodeId },
    });
    return { status: 200, body: {
      importId: staged.importId, storeId, threadId: meta.id, version: committed.version,
      duplicate: staged.duplicate || committed.noChange === true, itemCount: expanded.count, ancestorCount: expanded.ancestorCount,
      parentThreadId: parent ?? null,
      runtimeNodeId,
    } };
  } catch (error) {
    await markImport(pool, staged.importId, "failed", null, error.name === "AbortError" ? "cancelled" : error.code ?? "import_failed");
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
      // Raw JSON can contain escaped NUL. PostgreSQL JSON extraction functions
      // also reject it, so inspect the first envelope in the application.
      const first = await pool.query(`SELECT payload FROM codex_thread_events_versioned WHERE store_id=$1 AND thread_id=$2
        AND generation=$3 AND store_event_seq<=$4 AND item_seq=1`,
      [storeId, threadId, manifest.generation, head.version]);
      if (first.rows[0]?.payload?.type === "session_meta" && first.rows[0].payload.payload?.history_mode === "legacy") continue;
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

export async function listImportedThreads(pool, storeId = defaultStoreId, limit = 200, threadId = null, archived = null) {
  const id = safeStoreId(storeId);
  if (!id) throw Object.assign(new Error("invalid store id"), { statusCode: 400, code: "invalid_request" });
  const result = await pool.query(
    `SELECT projections.thread_id, projections.parent_thread_id, projections.source_kind,
            COALESCE(NULLIF(projections.state->>'name', ''), projections.title) AS title,
            projections.state->>'name' AS name, COALESCE(actions.action='archive',false) AS archived,
            projections.cwd, projections.item_count::text,
            COALESCE(projections.state #>> '{createdThread,metadata,timestamp}',
                     projections.state #>> '{metadata,created_at}') AS created_at,
            projections.active_generation::text, activity.updated_at,
            imports.import_id, imports.source_node_id, imports.source_codex_version,
            imports.created_at AS imported_at, runtimes.node_id AS runtime_node_id,
            runtimes.bound_at AS runtime_bound_at
     FROM codex_thread_projections projections
     LEFT JOIN LATERAL (
       SELECT action FROM mira_thread_actions WHERE store_id=projections.store_id AND thread_id=projections.thread_id
       ORDER BY action_seq DESC LIMIT 1
     ) actions ON TRUE
     LEFT JOIN LATERAL (
       -- Projection rebuilds touch every row; use the conversation's own clock.
       SELECT value::timestamptz AS updated_at
       FROM (VALUES
         (1, projections.state #>> '{metadata,updated_at}'),
         (2, projections.state #>> '{metadata,advance_recency_at}'),
         (3, projections.state #>> '{metadata,created_at}'),
         (4, projections.state #>> '{createdThread,metadata,timestamp}')
       ) timestamps(priority, value)
       WHERE value ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T'
         AND pg_input_is_valid(value, 'timestamp with time zone')
       ORDER BY priority LIMIT 1
     ) activity ON TRUE
     LEFT JOIN LATERAL (
       SELECT import_id, source_node_id, source_codex_version, created_at
       FROM mira_codex_session_imports
       WHERE store_id = projections.store_id AND thread_id = projections.thread_id AND status = 'imported'
       ORDER BY created_at DESC LIMIT 1
     ) imports ON TRUE
     LEFT JOIN mira_codex_thread_runtimes runtimes
       ON runtimes.store_id = projections.store_id AND runtimes.thread_id = projections.thread_id
     WHERE projections.store_id = $1 AND ($3::text IS NULL OR projections.thread_id = $3)
       AND ($4::boolean IS NULL OR COALESCE(actions.action='archive',false)=$4)
     ORDER BY activity.updated_at DESC NULLS LAST, projections.thread_id DESC LIMIT $2`,
    [id, limit, threadId, archived],
  );
  return addThreadReadStates(pool, id, await addThreadActivities(pool, id, result.rows.map((row) => ({
    threadId: row.thread_id, parentThreadId: row.parent_thread_id,
    sourceKind: row.source_kind, title: row.title, name: row.name, archived: row.archived, cwd: row.cwd,
    itemCount: Number(row.item_count), generation: Number(row.active_generation),
    createdAt: row.created_at ?? null,
    updatedAt: row.updated_at?.toISOString() ?? null, importId: row.import_id,
    sourceNodeId: row.source_node_id, sourceCodexVersion: row.source_codex_version,
    importedAt: row.imported_at?.toISOString() ?? null,
    runtimeNodeId: row.runtime_node_id, runtimeBoundAt: row.runtime_bound_at?.toISOString() ?? null,
  }))));
}

export { defaultStoreId };

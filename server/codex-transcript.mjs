import { getStoreHead, getThreadHistory } from "./thread-store.mjs";
import { isDeepStrictEqual } from "node:util";
import { toolItemView, responseToolView, reasoningText, reasoningParts } from "./public/trace-activity.js";

const maximumProjectedTextBytes = 1024 * 1024;

function isoTimestamp(value, numericScale = 1) {
  const milliseconds = typeof value === "number" ? value * numericScale : Date.parse(value ?? "");
  const date = new Date(milliseconds);
  return Number.isFinite(date.getTime()) ? date.toISOString() : null;
}

function recordTimestamp(record) {
  const payload = record?.payload;
  return isoTimestamp(payload?.completed_at_ms) ?? isoTimestamp(payload?.completed_at, 1000) ??
    isoTimestamp(payload?.item?.completedAt) ?? isoTimestamp(record?.timestamp ?? payload?.timestamp);
}

function turnStartedAt(record) {
  return isoTimestamp(record?.payload?.started_at, 1000) ?? isoTimestamp(record?.payload?.started_at_ms) ??
    (["task_started", "turn_started"].includes(record?.payload?.type) ? recordTimestamp(record) : null);
}

function elapsedMilliseconds(startedAt, completedAt) {
  const start = Date.parse(startedAt ?? "");
  const end = Date.parse(completedAt ?? "");
  return Number.isFinite(start) && Number.isFinite(end) && end >= start ? end - start : null;
}

function valueText(value) {
  if (typeof value === "string") return value;
  if (value === null || value === undefined) return "";
  if (Array.isArray(value)) return value.map(valueText).filter(Boolean).join("\n");
  if (typeof value === "object") {
    for (const key of ["text", "inputText", "input_text", "outputText", "output_text", "message"]) {
      if (typeof value[key] === "string") return value[key];
    }
    if (value.content !== undefined) return valueText(value.content);
    if (value.output !== undefined) return valueText(value.output);
  }
  return "";
}

function parseJsonString(value) {
  if (typeof value !== "string") return value;
  const trimmed = value.trim();
  if (!trimmed || !["{", "["].includes(trimmed[0])) return value;
  try { return JSON.parse(trimmed); } catch { return value; }
}

function projectedErrorMessage(value, depth = 0) {
  if (depth > 6 || value === null || value === undefined) return "";
  const parsed = parseJsonString(value);
  if (typeof parsed === "string") return parsed;
  if (typeof parsed !== "object" || Array.isArray(parsed)) return "";
  return projectedErrorMessage(parsed.error, depth + 1) ||
    projectedErrorMessage(parsed.message, depth + 1) ||
    projectedErrorMessage(parsed.detail, depth + 1);
}

function printable(value) {
  const parsed = parseJsonString(value);
  const text = valueText(parsed);
  if (text) return text;
  if (typeof parsed === "string") return parsed;
  if (parsed === null || parsed === undefined) return "";
  return JSON.stringify(parsed, null, 2);
}

function boundedText(value) {
  const text = String(value ?? "");
  if (Buffer.byteLength(text) <= maximumProjectedTextBytes) return text;
  let end = Math.min(text.length, maximumProjectedTextBytes);
  while (Buffer.byteLength(text.slice(0, end)) > maximumProjectedTextBytes) end = Math.floor(end * 0.9);
  return `${text.slice(0, end)}\n\n[网页投影已截断；完整事件仍保存在 PostgreSQL]`;
}

function toolBody(input, output) {
  const sections = [];
  const inputText = printable(input);
  const outputText = printable(output);
  if (inputText) sections.push(`输入\n${inputText}`);
  if (outputText) sections.push(`输出\n${outputText}`);
  return boundedText(sections.join("\n\n"));
}

function normalizedToolTitle(item) {
  const name = item.name ?? item.tool ?? item.action ?? item.type ?? "Tool";
  if (name === "exec") return "functions.exec";
  if (name === "apply_patch") return "functions.apply_patch";
  const namespace = item.namespace ?? item.server;
  return namespace ? `${namespace} · ${name}` : String(name);
}

function visibleResponseMessage(payload) {
  if (!payload || !["user", "assistant"].includes(payload.role)) return false;
  if (payload.role !== "user") return true;
  const kinds = payload.internal_chat_message_metadata_passthrough?.content_item_kinds;
  // Codex persists environment/permission injections as role=user response
  // items as well. They are model context, not messages authored by the user.
  return !Array.isArray(kinds) || kinds.length === 0 || kinds.some((kind) => String(kind).startsWith("user."));
}

function projectedMaterializedItem(item, context) {
  const type = String(item?.type ?? "").replaceAll("_", "").toLowerCase();
  const base = {
    key: `history-${context.itemSeq}-${item?.id ?? context.index}`,
    turnId: context.turnId ?? null,
    sourceItemSeq: context.itemSeq,
    itemId: item?.id ?? null,
    status: item?.status ?? "",
  };
  const tool = toolItemView(item);
  if (tool) return { ...base, ...tool, body: boundedText(tool.body) };
  if (type === "usermessage") return {
    ...base, kind: "user", title: "你", markdown: true, body: boundedText(valueText(item.content)),
  };
  if (type === "agentmessage") return {
    ...base, kind: "assistant", title: "Codex", markdown: true,
    body: boundedText(valueText(item.content ?? item.text)), phase: item.phase ?? null,
  };
  if (type === "reasoning") return {
    ...base, kind: "reasoning", title: "推理摘要", markdown: true,
    body: boundedText(reasoningText(item)), summaryParts: reasoningParts(item).map(boundedText),
  };
  if (type === "plan") return {
    ...base, kind: "reasoning", title: "计划", markdown: true,
    body: boundedText(valueText(item.text ?? item.plan ?? item.content)),
  };
  if (type === "contextcompaction") return {
    ...base, kind: "compaction", title: "上下文自动压缩", markdown: false,
    body: "较早的上下文已自动压缩。",
  };
  if (["websearch", "imageview", "imagegeneration", "subagentactivity", "functioncalloutput"].includes(type)) return {
    ...base, kind: "tool", title: normalizedToolTitle(item), markdown: false,
    body: boundedText(printable(item)),
  };
  return null;
}

function responseToolCall(payload, itemSeq, index, turnId) {
  const type = payload.type;
  const callId = payload.call_id ?? payload.id ?? `response-${itemSeq}-${index}`;
  const isCall = ["custom_tool_call", "function_call", "local_shell_call", "tool_search_call", "web_search_call"].includes(type);
  const isOutput = ["custom_tool_call_output", "function_call_output", "tool_search_output"].includes(type);
  if (!isCall && !isOutput) return null;
  return {
    callId,
    isCall,
    isOutput,
    input: payload.input ?? payload.arguments ?? payload.command ?? payload.query,
    output: payload.output ?? payload.result,
    entry: {
      key: `history-tool-${turnId ?? "unscoped"}-${callId}`,
      turnId: turnId ?? null,
      sourceItemSeq: itemSeq,
      itemId: callId,
      kind: "tool",
      title: isCall ? normalizedToolTitle(payload) : "工具输出",
      markdown: false,
      status: isCall ? (payload.status ?? "运行") : "完成",
      body: "",
    },
  };
}

/**
 * Project canonical Codex rollout items into a stable, presentation-oriented
 * trace. The source items are never changed; this projection can be rebuilt.
 */
export function projectCodexTranscript(items, options = {}) {
  const records = Array.isArray(items) ? items : [];
  const offset = options.itemOffset ?? 0;
  const responseCalls = new Set();
  const materializedTools = new Map();
  const turnTimings = new Map();
  const callKey = (turnId, id) => JSON.stringify([turnId ?? null, id]);
  let scannedTurnId = options.initialTurnId ?? null;
  if (scannedTurnId && options.initialTurnStartedAt) {
    turnTimings.set(scannedTurnId, { startedAt: options.initialTurnStartedAt, startedApproximate: options.initialTurnStartedApproximate });
  }
  for (const [index, record] of [...records, ...(options.timingRecords ?? [])].entries()) {
    const payload = record?.payload ?? {};
    if (record?.type === "turn_context" || (record?.type === "event_msg" && ["task_started", "turn_started"].includes(payload.type))) {
      scannedTurnId = payload.turn_id ?? scannedTurnId;
    }
    const turnId = payload.turn_id ?? payload.internal_chat_message_metadata_passthrough?.turn_id ?? scannedTurnId;
    if (turnId) {
      const timing = turnTimings.get(turnId) ?? {};
      if (record?.type === "turn_context" && !timing.startedAt) {
        timing.startedAt = recordTimestamp(record) ?? options.recordedAt?.get(offset + index + 1);
        timing.startedApproximate = Boolean(timing.startedAt);
      }
      if (record?.type === "event_msg" && ["task_started", "turn_started"].includes(payload.type)) {
        const startedAt = turnStartedAt(record);
        if (startedAt) { timing.startedAt = startedAt; timing.startedApproximate = false; }
      }
      if (record?.type === "event_msg" && ["task_complete", "turn_complete", "turn_aborted"].includes(payload.type)) {
        const startedAt = turnStartedAt(record);
        if (startedAt) { timing.startedAt = startedAt; timing.startedApproximate = false; }
        timing.completedAt = recordTimestamp(record);
        if (Number.isFinite(payload.duration_ms) && payload.duration_ms >= 0) timing.elapsedMs = payload.duration_ms;
      }
      turnTimings.set(turnId, timing);
    }
    if (record?.type === "response_item") {
      const tool = responseToolCall(payload, offset + index + 1, index, turnId);
      if (tool?.isCall || (options.fragments && tool?.isOutput)) responseCalls.add(callKey(turnId, tool.callId));
    }
    if (record?.type === "event_msg" && payload.type === "item_completed" && payload.item?.id) {
      const entry = projectedMaterializedItem(payload.item, { itemSeq: offset + index + 1, index, turnId });
      if (entry?.kind === "tool") materializedTools.set(callKey(turnId, payload.item.id), entry);
    }
  }
  const trace = [];
  const projectedById = new Map();
  const projectedNarratives = new Map();
  const toolCalls = new Map();
  let currentTurnId = options.initialTurnId ?? null;

  function withTiming(entry) {
    if (!entry) return entry;
    const timing = turnTimings.get(entry.turnId);
    const itemClock = entry.completedAt ?? recordTimestamp(records[(entry.sourceItemSeq ?? (offset + 1)) - offset - 1]);
    const recordedAt = options.recordedAt?.get(entry.sourceItemSeq);
    const completedAt = itemClock ?? (["assistant", "user"].includes(entry.kind) ? timing?.completedAt ?? recordedAt : null);
    if (completedAt) entry.completedAt = completedAt;
    if (!itemClock && completedAt) entry.timingScope = timing?.completedAt ? "turn" : "recorded";
    if (["user", "assistant"].includes(entry.kind) && entry.elapsedMs == null) {
      entry.elapsedMs = !itemClock && timing?.elapsedMs != null ? timing.elapsedMs : elapsedMilliseconds(timing?.startedAt, completedAt);
      if (timing?.startedApproximate && (itemClock || timing?.elapsedMs == null)) entry.elapsedApproximate = true;
    }
    return entry;
  }

  function push(entry) {
    entry = withTiming(entry);
    if (!entry || (!entry.body && entry.kind !== "tool")) return;
    if (["user", "assistant", "reasoning"].includes(entry.kind)) {
      const signature = JSON.stringify([
        entry.turnId ?? null, entry.kind, entry.phase ?? null, entry.body,
      ]);
      const previous = projectedNarratives.get(signature);
      // A single Codex message can be persisted as item_completed,
      // event_msg and response_item records. Deduplicate those representations
      // without collapsing equal messages from different turns.
      if (previous && (entry.turnId || entry.sourceItemSeq - previous.sourceItemSeq <= 3)) {
        if ((!previous.completedAt && entry.completedAt) || (previous.timingScope && !entry.timingScope && entry.completedAt)) {
          previous.completedAt = entry.completedAt;
          previous.elapsedMs = entry.elapsedMs;
          previous.timingScope = entry.timingScope;
          previous.elapsedApproximate = entry.elapsedApproximate;
        }
        return;
      }
      projectedNarratives.set(signature, entry);
    }
    const existing = projectedById.get(entry.key);
    if (existing) Object.assign(existing, entry);
    else {
      trace.push(entry);
      projectedById.set(entry.key, entry);
    }
  }

  for (const [index, record] of records.entries()) {
    const payload = record?.payload ?? {};
    const itemSeq = offset + index + 1;
    const recordTurnId = payload.turn_id ?? payload.internal_chat_message_metadata_passthrough?.turn_id ?? currentTurnId;
    if (record.type === "turn_context") {
      currentTurnId = payload.turn_id ?? currentTurnId;
      continue;
    }
    if (record.type === "compacted") {
      push({ key: `history-${itemSeq}-compaction`, turnId: recordTurnId, sourceItemSeq: itemSeq,
        kind: "compaction", title: "上下文自动压缩", markdown: false, body: "较早的上下文已自动压缩。" });
      continue;
    }
    if (record.type === "event_msg" && ["task_started", "turn_started"].includes(payload.type)) {
      currentTurnId = payload.turn_id ?? currentTurnId;
      continue;
    }
    if (record.type === "event_msg" && ["task_complete", "turn_complete", "turn_aborted"].includes(payload.type)) {
      currentTurnId = payload.turn_id ?? currentTurnId;
      const failure = projectedErrorMessage(payload.error);
      if (failure) push({
        key: `history-${itemSeq}-turn-error`, turnId: currentTurnId ?? null, sourceItemSeq: itemSeq,
        kind: "error", title: "Turn 失败", markdown: false, status: "失败", body: boundedText(failure),
      });
      continue;
    }
    if (record.type === "event_msg" && payload.type === "item_completed") {
      const materialized = payload.item;
      // Only deduplicate a proven counterpart. A code-mode wrapper and its
      // nested command items are distinct; sharing a turn is not duplication.
      const materializedTurnId = payload.turn_id ?? recordTurnId;
      const key = callKey(materializedTurnId, materialized?.id);
      if (!(materializedTools.has(key) && responseCalls.has(key))) {
        const entry = projectedMaterializedItem(materialized, {
          itemSeq, index, turnId: materializedTurnId,
        });
        if (options.fragments && entry?.kind === "tool" && entry.itemId) {
          entry.key = `history-tool-${entry.turnId ?? "unscoped"}-${entry.itemId}`;
          entry.toolFragment = { materialized: true };
        }
        push(entry);
      }
      continue;
    }
    if (record.type === "event_msg") {
      if (payload.type === "user_message") push({
        key: `history-${itemSeq}-user`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
        kind: "user", title: "你", markdown: true, status: "", body: boundedText(payload.message ?? valueText(payload)),
      });
      if (payload.type === "agent_message") push({
        key: `history-${itemSeq}-agent`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
        kind: "assistant", title: "Codex", markdown: true, status: "", phase: payload.phase ?? null,
        body: boundedText(payload.message ?? valueText(payload)),
      });
      if (payload.type === "agent_reasoning") push({
        key: `history-${itemSeq}-reasoning`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
        kind: "reasoning", title: "推理摘要", markdown: true, status: "", body: boundedText(payload.text ?? valueText(payload)),
      });
    }
    if (record.type !== "response_item") continue;
    const tool = responseToolCall(payload, itemSeq, index, recordTurnId);
    if (tool) {
      const key = callKey(recordTurnId, tool.callId);
      let state = toolCalls.get(key);
      if (!state) {
        const materialized = materializedTools.get(key);
        state = { entry: withTiming({ ...tool.entry, ...materialized, key: tool.entry.key, sourceItemSeq: itemSeq }), materialized,
          input: undefined, output: undefined };
        toolCalls.set(key, state);
        trace.push(state.entry);
        projectedById.set(state.entry.key, state.entry);
      }
      if (tool.isCall) {
        state.input = tool.input;
        state.payload = payload;
        if (!state.materialized) {
          state.entry.title = tool.entry.title;
          state.entry.status = payload.status ?? state.entry.status;
        }
        state.entry.sourceItemSeq = Math.min(state.entry.sourceItemSeq, itemSeq);
      }
      if (tool.isOutput) {
        state.output = tool.output;
        if (!state.materialized) state.entry.status = "完成";
      }
      if (!state.materialized) {
        const view = responseToolView(state.payload ?? {}, state.entry.status);
        if (view) state.entry.activity = view.activity;
      }
      state.entry.body = state.materialized?.body || toolBody(state.input, state.output);
      if (options.fragments) state.entry.toolFragment = state.materialized
        ? { materialized: true }
        : {
          input: state.input === undefined ? null : boundedText(printable(state.input)),
          output: state.output === undefined ? null : boundedText(printable(state.output)),
        };
      continue;
    }
    if (payload.type === "reasoning") push({
      key: `history-${itemSeq}-${payload.id ?? "reasoning"}`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
      itemId: payload.id ?? null, summaryParts: reasoningParts(payload).map(boundedText),
      kind: "reasoning", title: "推理摘要", markdown: true, status: "",
      body: boundedText(reasoningText(payload)),
    });
    if (payload.type === "message" && visibleResponseMessage(payload)) push({
      key: `history-${itemSeq}-${payload.id ?? payload.role}`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
      kind: payload.role === "user" ? "user" : "assistant",
      title: payload.role === "user" ? "你" : "Codex", markdown: true, status: "", phase: payload.phase ?? null,
      body: boundedText(valueText(payload.content)),
    });
  }
  return trace.filter((entry) => entry.body || entry.activity || entry.kind !== "tool");
}

export function paginateCodexTranscript(trace, cursor = null, limit = 60) {
  const end = cursor === null ? trace.length : Math.min(cursor, trace.length);
  const start = Math.max(0, end - limit);
  return {
    trace: trace.slice(start, end),
    nextCursor: start > 0 ? String(start) : null,
    totalTraceItems: trace.length,
  };
}

export async function getCodexTranscript(pool, storeId, threadId, options = {}) {
  if (options.tail) return getCodexTranscriptTail(pool, storeId, threadId, options);
  const head = await getStoreHead(pool, storeId);
  const manifest = head.historyManifest?.[threadId];
  if (!manifest) return { status: 404, body: { error: "thread history not found", code: "not_found" } };
  const history = await getThreadHistory(pool, storeId, threadId, manifest.generation, head.version);
  if (history.status !== 200) return history;
  const projected = projectCodexTranscript(history.body.items);
  const page = paginateCodexTranscript(projected, options.cursor ?? null, options.limit ?? 60);
  return {
    status: 200,
    body: {
      storeId,
      threadId,
      storeVersion: head.version,
      generation: history.body.generation,
      itemCount: history.body.itemCount,
      ...page,
    },
  };
}

async function restoreImportedTimestamps(pool, storeId, threadId, rows) {
  const imported = await pool.query(
    `SELECT import_id, source_item_count::text FROM mira_codex_session_imports
     WHERE store_id=$1 AND thread_id=$2 AND status='imported'
     ORDER BY store_event_seq DESC, created_at DESC LIMIT 1`, [storeId, threadId]);
  const recordedAt = new Map(rows.map((row) => [Number(row.item_seq), row.created_at?.toISOString()]));
  if (!imported.rowCount) return recordedAt;
  const source = imported.rows[0];
  const segments = await pool.query(
    `SELECT source_import_id, first_line_seq::text, item_count::text
     FROM mira_codex_session_import_segments WHERE import_id=$1 ORDER BY segment_index`, [source.import_id]);
  const parts = segments.rowCount ? segments.rows : [{ source_import_id: source.import_id, first_line_seq: 1, item_count: source.source_item_count }];
  let offset = 0;
  for (const segment of parts) {
    const count = Number(segment.item_count);
    const matching = rows.filter((row) => Number(row.item_seq) > offset && Number(row.item_seq) <= offset + count);
    const firstLine = Number(segment.first_line_seq);
    if (matching.length) {
      const raw = await pool.query(
        `SELECT line_seq::text, raw_record FROM mira_codex_session_import_records
         WHERE import_id=$1 AND line_seq=ANY($2::bigint[])`,
        [segment.source_import_id, matching.map((row) => firstLine + Number(row.item_seq) - offset - 1)]);
      const byLine = new Map(raw.rows.map((row) => [Number(row.line_seq), row.raw_record]));
      for (const row of matching) {
        const original = byLine.get(firstLine + Number(row.item_seq) - offset - 1);
        // A replacement generation may have different content at the same
        // position. Attach provenance clocks only to an exact matching item.
        if (original?.type !== row.payload?.type || !isDeepStrictEqual(original?.payload, row.payload?.payload)) continue;
        recordedAt.delete(Number(row.item_seq));
        const timestamp = recordTimestamp(original);
        if (timestamp && !recordTimestamp(row.payload)) row.payload = { ...row.payload, timestamp };
      }
    }
    offset += count;
  }
  return recordedAt;
}

// V2 cursors use immutable raw sequence positions, scoped to one generation.
// Keep the original numeric-cursor reader for already-open older Web clients.
export async function getCodexTranscriptTail(pool, storeId, threadId, options = {}) {
  const cursor = options.cursor == null ? null : /^t2:(\d+):(\d+):(\d+)$/.exec(String(options.cursor));
  if (options.cursor != null && (!cursor || cursor.slice(1).some((value) => !Number.isSafeInteger(Number(value))))) {
    return { status: 400, body: { error: "invalid transcript cursor", code: "invalid_request" } };
  }
  const head = await pool.query(
    `SELECT active_generation::text, item_count::text, through_event_seq::text
     FROM codex_thread_projections WHERE store_id = $1 AND thread_id = $2`,
    [storeId, threadId],
  );
  if (!head.rowCount) return { status: 404, body: { error: "thread history not found", code: "not_found" } };
  const generation = Number(head.rows[0].active_generation);
  const itemCount = Number(head.rows[0].item_count);
  if (cursor && (Number(cursor[1]) !== generation || Number(cursor[3]) > itemCount)) {
    return { status: 409, body: { error: "会话历史已更新，请重新加载", code: "stale_transcript_cursor" } };
  }
  const snapshotCount = cursor ? Number(cursor[3]) : itemCount;
  const end = cursor ? Number(cursor[2]) : snapshotCount + 1;
  if (end < 1 || end > snapshotCount + 1) {
    return { status: 400, body: { error: "invalid transcript cursor", code: "invalid_request" } };
  }
  const limit = Math.max(10, Math.min(200, options.limit ?? 60));
  const windowSize = Math.max(120, limit * 4);
  const result = await pool.query(
    `SELECT item_seq::text, payload, created_at FROM codex_thread_events AS events
     WHERE store_id = $1 AND thread_id = $2 AND generation = $3 AND item_seq < $4
     ORDER BY events.item_seq DESC LIMIT $5`,
    [storeId, threadId, generation, end, windowSize],
  );
  const rows = result.rows.reverse();
  const start = rows.length ? Number(rows[0].item_seq) : end;
  if (rows.length !== Math.min(windowSize, end - 1) ||
      rows.some((row, index) => Number(row.item_seq) !== end - rows.length + index)) {
    return { status: 409, body: { error: "thread history is incomplete", code: "history_incomplete" } };
  }
  // Only fetch candidate turn markers, never the preceding full history.
  // JSON -> operators decode escaped NUL even in unrelated values. A text
  // prefilter avoids that; validate the actual structure in JS (which preserves
  // NUL) so nested lookalikes cannot supply another turn's identity.
  let context = null;
  let contextRow = null;
  let contextBefore = start;
  while (contextBefore > 1) {
    const candidates = await pool.query(
      `SELECT item_seq::text, payload, created_at FROM codex_thread_events AS events
       WHERE store_id = $1 AND thread_id = $2 AND generation = $3 AND item_seq < $4
         AND payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_started|turn_started|turn_context)"'
       ORDER BY events.item_seq DESC LIMIT 1`,
      [storeId, threadId, generation, contextBefore],
    );
    contextRow = candidates.rows.find(({ payload: record }) => record?.type === "turn_context" || (record?.type === "event_msg" &&
      ["task_started", "turn_started"].includes(record.payload?.type)));
    context = contextRow?.payload;
    if (context || candidates.rows.length === 0) break;
    contextBefore = Number(candidates.rows.at(-1).item_seq);
  }
  const recordedAt = await restoreImportedTimestamps(pool, storeId, threadId, contextRow ? [contextRow, ...rows] : rows);
  context = contextRow?.payload;
  const projectionOptions = {
    itemOffset: start - 1,
    initialTurnId: context?.payload?.turn_id,
    initialTurnStartedAt: turnStartedAt(context) ?? recordTimestamp(context) ?? recordedAt.get(Number(contextRow?.item_seq)),
    initialTurnStartedApproximate: !turnStartedAt(context),
    fragments: true,
    recordedAt,
  };
  let projected = projectCodexTranscript(rows.map((row) => row.payload), projectionOptions);
  // A page can end inside a turn. Its completion marker may be in the newer
  // page; read just that marker within the same immutable snapshot boundary.
  if (end <= snapshotCount && projected.some((item) => item.kind === "assistant" && (!item.completedAt || item.timingScope === "recorded"))) {
    let after = end - 1;
    while (after < snapshotCount) {
      const candidates = await pool.query(
        `SELECT item_seq::text, payload FROM codex_thread_events
         WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 AND item_seq<=$5
           AND payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_complete|turn_complete|turn_aborted)"'
         ORDER BY item_seq LIMIT 1`, [storeId, threadId, generation, after, snapshotCount]);
      if (!candidates.rowCount) break;
      let record = candidates.rows[0].payload;
      if (record?.type === "event_msg" && ["task_complete", "turn_complete", "turn_aborted"].includes(record.payload?.type)) {
        if (!recordTimestamp(record)) {
          await restoreImportedTimestamps(pool, storeId, threadId, candidates.rows);
          record = candidates.rows[0].payload;
        }
        projected = projectCodexTranscript(rows.map((row) => row.payload), { ...projectionOptions, timingRecords: [record] });
        break;
      }
      after = Number(candidates.rows[0].item_seq);
    }
  }
  const trace = projected.slice(-limit);
  const before = projected.length > limit ? Math.min(...trace.map((item) => item.sourceItemSeq)) : start;
  return {
    status: 200,
    body: {
      storeId, threadId, generation, itemCount,
      storeVersion: Number(head.rows[0].through_event_seq),
      trace, projectionVersion: 2, totalTraceItems: null,
      nextCursor: before > 1 ? `t2:${generation}:${before}:${snapshotCount}` : null,
    },
  };
}

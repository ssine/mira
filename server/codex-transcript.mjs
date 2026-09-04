import { getStoreHead, getThreadHistory } from "./thread-store.mjs";
import { toolItemView, responseToolView, reasoningText, reasoningParts } from "./public/trace-activity.js";

const maximumProjectedTextBytes = 1024 * 1024;

function recordTimestamp(record) {
  const value = record?.timestamp ?? record?.payload?.timestamp;
  const milliseconds = typeof value === "number" ? value : Date.parse(value ?? "");
  return Number.isFinite(milliseconds) ? new Date(milliseconds).toISOString() : null;
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
    ...base, kind: "system", title: "上下文压缩", markdown: true,
    body: boundedText(valueText(item.summary) || "已压缩较早上下文"),
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
export function projectCodexTranscript(items) {
  const records = Array.isArray(items) ? items : [];
  const responseCalls = new Set();
  const materializedTools = new Map();
  const turnTimings = new Map();
  const callKey = (turnId, id) => JSON.stringify([turnId ?? null, id]);
  let scannedTurnId = null;
  for (const [index, record] of records.entries()) {
    const payload = record?.payload ?? {};
    if (record?.type === "event_msg" && ["task_started", "turn_started"].includes(payload.type)) {
      scannedTurnId = payload.turn_id ?? scannedTurnId;
    }
    const turnId = payload.turn_id ?? payload.internal_chat_message_metadata_passthrough?.turn_id ?? scannedTurnId;
    const completedAt = recordTimestamp(record);
    if (turnId && completedAt) {
      const timing = turnTimings.get(turnId) ?? {};
      if (record?.type === "event_msg" && ["task_started", "turn_started"].includes(payload.type)) {
        timing.startedAt ??= completedAt;
      }
      if (record?.type === "event_msg" && ["task_complete", "turn_complete", "turn_aborted"].includes(payload.type)) {
        timing.completedAt = completedAt;
      }
      turnTimings.set(turnId, timing);
    }
    if (record?.type === "response_item") {
      const tool = responseToolCall(payload, index + 1, index, turnId);
      if (tool?.isCall) responseCalls.add(callKey(turnId, tool.callId));
    }
    if (record?.type === "event_msg" && payload.type === "item_completed" && payload.item?.id) {
      const entry = projectedMaterializedItem(payload.item, { itemSeq: index + 1, index, turnId });
      if (entry?.kind === "tool") materializedTools.set(callKey(turnId, payload.item.id), entry);
    }
  }
  const trace = [];
  const projectedById = new Map();
  const projectedNarratives = new Map();
  const toolCalls = new Map();
  let currentTurnId = null;

  function withTiming(entry) {
    if (!entry) return entry;
    const completedAt = entry.completedAt ?? recordTimestamp(records[(entry.sourceItemSeq ?? 1) - 1]);
    if (completedAt) entry.completedAt = completedAt;
    if (["user", "assistant"].includes(entry.kind) && entry.elapsedMs == null) {
      entry.elapsedMs = elapsedMilliseconds(turnTimings.get(entry.turnId)?.startedAt, completedAt);
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
      if (previous && (entry.turnId || entry.sourceItemSeq - previous.sourceItemSeq <= 3)) return;
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
    const itemSeq = index + 1;
    const recordTurnId = payload.turn_id ?? payload.internal_chat_message_metadata_passthrough?.turn_id ?? currentTurnId;
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
        push(projectedMaterializedItem(materialized, {
          itemSeq, index, turnId: materializedTurnId,
        }));
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

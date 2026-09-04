import { getStoreHead, getThreadHistory } from "./thread-store.mjs";

const maximumProjectedTextBytes = 1024 * 1024;

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
    status: item?.status ?? "",
  };
  if (type === "usermessage") return {
    ...base, kind: "user", title: "你", markdown: true, body: boundedText(valueText(item.content)),
  };
  if (type === "agentmessage") return {
    ...base, kind: "assistant", title: "Codex", markdown: true,
    body: boundedText(valueText(item.content ?? item.text)), phase: item.phase ?? null,
  };
  if (type === "reasoning") return {
    ...base, kind: "reasoning", title: "推理摘要", markdown: true,
    body: boundedText(valueText(item.summary_text ?? item.summary ?? item.content ?? item.raw_content)),
  };
  if (type === "commandexecution") return {
    ...base, kind: "tool", title: "Shell", markdown: false,
    body: boundedText([
      Array.isArray(item.command) ? item.command.join(" ") : printable(item.command),
      item.cwd ? `cwd: ${item.cwd}` : "",
      printable(item.aggregated_output ?? item.formatted_output ?? [item.stdout, item.stderr]),
      item.exit_code === undefined || item.exit_code === null ? "" : `exit code: ${item.exit_code}`,
    ].filter(Boolean).join("\n\n")),
  };
  if (type === "filechange") return {
    ...base, kind: "tool", title: "文件修改", markdown: false,
    body: boundedText([printable(item.changes), printable([item.stdout, item.stderr])].filter(Boolean).join("\n\n")),
  };
  if (["mcptoolcall", "dynamictoolcall", "toolcall", "collabagenttoolcall"].includes(type)) return {
    ...base, kind: "tool", title: normalizedToolTitle(item), markdown: false,
    body: toolBody(item.arguments ?? item.input ?? item.prompt, item.result ?? item.content_items ?? item.output ?? item.error),
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
      key: `history-tool-${callId}`,
      turnId: turnId ?? null,
      sourceItemSeq: itemSeq,
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
  const responseToolTurns = new Set();
  let scannedTurnId = null;
  for (const record of records) {
    const payload = record?.payload ?? {};
    if (record?.type === "event_msg" && ["task_started", "turn_started"].includes(payload.type)) {
      scannedTurnId = payload.turn_id ?? scannedTurnId;
    }
    const turnId = payload.turn_id ?? payload.internal_chat_message_metadata_passthrough?.turn_id ?? scannedTurnId;
    if (record?.type === "response_item" && [
      "custom_tool_call", "function_call", "local_shell_call", "tool_search_call", "web_search_call",
    ].includes(payload.type)) responseToolTurns.add(turnId ?? "unscoped");
  }
  const trace = [];
  const projectedById = new Map();
  const projectedNarratives = new Map();
  const toolCalls = new Map();
  let currentTurnId = null;

  function push(entry) {
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
      continue;
    }
    if (record.type === "event_msg" && payload.type === "item_completed") {
      const materialized = payload.item;
      const materializedType = String(materialized?.type ?? "").replaceAll("_", "").toLowerCase();
      // Response tool call/output pairs carry the exact model-facing input and
      // output. Prefer them over their CommandExecution duplicate.
      const materializedTurnId = payload.turn_id ?? recordTurnId;
      if (!(materializedType === "commandexecution" && responseToolTurns.has(materializedTurnId ?? "unscoped"))) {
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
      if (["agent_reasoning", "agent_reasoning_raw_content"].includes(payload.type)) push({
        key: `history-${itemSeq}-reasoning`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
        kind: "reasoning", title: "推理摘要", markdown: true, status: "", body: boundedText(payload.text ?? valueText(payload)),
      });
    }
    if (record.type !== "response_item") continue;
    const tool = responseToolCall(payload, itemSeq, index, recordTurnId);
    if (tool) {
      let state = toolCalls.get(tool.callId);
      if (!state) {
        state = { entry: tool.entry, input: undefined, output: undefined };
        toolCalls.set(tool.callId, state);
        trace.push(state.entry);
        projectedById.set(state.entry.key, state.entry);
      }
      if (tool.isCall) {
        state.input = tool.input;
        state.entry.title = tool.entry.title;
        state.entry.status = payload.status ?? state.entry.status;
        state.entry.sourceItemSeq = Math.min(state.entry.sourceItemSeq, itemSeq);
      }
      if (tool.isOutput) {
        state.output = tool.output;
        state.entry.status = "完成";
      }
      state.entry.body = toolBody(state.input, state.output);
      continue;
    }
    if (payload.type === "reasoning") push({
      key: `history-${itemSeq}-${payload.id ?? "reasoning"}`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
      kind: "reasoning", title: "推理摘要", markdown: true, status: "",
      body: boundedText(valueText(payload.summary ?? payload.content)),
    });
    if (payload.type === "message" && visibleResponseMessage(payload)) push({
      key: `history-${itemSeq}-${payload.id ?? payload.role}`, turnId: recordTurnId ?? null, sourceItemSeq: itemSeq,
      kind: payload.role === "user" ? "user" : "assistant",
      title: payload.role === "user" ? "你" : "Codex", markdown: true, status: "", phase: payload.phase ?? null,
      body: boundedText(valueText(payload.content)),
    });
  }
  return trace.filter((entry) => entry.body || entry.kind !== "tool");
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

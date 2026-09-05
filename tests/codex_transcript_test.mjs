import assert from "node:assert/strict";
import test from "node:test";

import { paginateCodexTranscript, projectCodexTranscript } from "../server/codex-transcript.mjs";

const record = (type, payload) => ({ type, payload });

test("preserves nested command activities alongside distinct code-mode wrapper calls", () => {
  const turnId = "00000000-0000-4000-8000-000000000001";
  const result = projectCodexTranscript([
    record("event_msg", { type: "task_started", turn_id: turnId }),
    record("event_msg", { type: "item_completed", turn_id: turnId, item: {
      id: "user-1", type: "UserMessage", content: [{ type: "text", text: "# Request" }],
    } }),
    record("response_item", { type: "custom_tool_call", call_id: "call-1", name: "exec", input: "text(true);" }),
    record("event_msg", { type: "item_completed", turn_id: turnId, item: {
      id: "exec-1", type: "CommandExecution", command: ["true"], status: "completed", exit_code: 0,
    } }),
    record("response_item", { type: "custom_tool_call_output", call_id: "call-1", output: JSON.stringify([
      { type: "input_text", text: "Script completed" },
    ]) }),
    record("event_msg", { type: "item_completed", turn_id: turnId, item: {
      id: "reasoning-1", type: "Reasoning", summary_text: ["Checked the result"],
    } }),
    record("event_msg", { type: "item_completed", turn_id: turnId, item: {
      id: "agent-1", type: "AgentMessage", phase: "final_answer",
      content: [{ type: "Text", text: "## Done\n\nEverything works." }],
    } }),
  ]);

  assert.deepEqual(result.map((item) => item.kind), ["user", "tool", "tool", "reasoning", "assistant"]);
  assert.equal(result.filter((item) => item.kind === "tool").length, 2);
  assert.match(result[1].body, /text\(true\)/);
  assert.match(result[1].body, /Script completed/);
  assert.equal(result[2].activity.actions[0].kind, "run");
  assert.equal(result[4].markdown, true);
  assert.match(result[4].body, /^## Done/);
});

test("projects legacy messages, reasoning, and paired custom tool output", () => {
  const result = projectCodexTranscript([
    record("event_msg", { type: "task_started", turn_id: "turn-1" }),
    record("event_msg", { type: "user_message", message: "Please inspect" }),
    record("response_item", { type: "reasoning", id: "reasoning-1", summary: [
      { type: "summary_text", text: "I will inspect it." },
    ] }),
    record("response_item", { type: "custom_tool_call", call_id: "call-1", name: "exec", input: "text(result);" }),
    record("response_item", { type: "custom_tool_call_output", call_id: "call-1", output: JSON.stringify([
      { type: "input_text", text: "Output line" },
    ]) }),
    record("event_msg", { type: "agent_message", phase: "final_answer", message: "**Finished.**" }),
  ]);

  assert.deepEqual(result.map((item) => item.kind), ["user", "reasoning", "tool", "assistant"]);
  assert.equal(result[2].title, "functions.exec");
  assert.equal(result[2].status, "完成");
  assert.match(result[2].body, /Output line/);
  assert.equal(result[3].body, "**Finished.**");
});

test("projects resumed mixed-format turns and deduplicates parallel message records", () => {
  const oldTurnId = "00000000-0000-4000-8000-000000000010";
  const resumedTurnId = "00000000-0000-4000-8000-000000000011";
  const result = projectCodexTranscript([
    record("event_msg", { type: "task_started", turn_id: oldTurnId }),
    record("event_msg", { type: "item_completed", turn_id: oldTurnId, item: {
      id: "old-user", type: "UserMessage", content: [{ type: "text", text: "Old imported request" }],
    } }),
    record("event_msg", { type: "item_completed", turn_id: oldTurnId, item: {
      id: "old-agent", type: "AgentMessage", content: [{ type: "text", text: "Old imported response" }],
    } }),
    record("event_msg", { type: "task_started", turn_id: resumedTurnId }),
    record("response_item", {
      id: "environment", role: "user", type: "message",
      content: [{ type: "input_text", text: "<environment_context>hidden</environment_context>" }],
      internal_chat_message_metadata_passthrough: {
        turn_id: resumedTurnId, content_item_kinds: ["environment_context"],
      },
    }),
    record("response_item", {
      id: "new-user", role: "user", type: "message",
      content: [{ type: "input_text", text: "New resumed request" }],
      internal_chat_message_metadata_passthrough: { turn_id: resumedTurnId, content_item_kinds: ["user.text"] },
    }),
    record("event_msg", { type: "user_message", message: "New resumed request" }),
    record("event_msg", { type: "agent_message", phase: "final_answer", message: "New resumed response" }),
    record("response_item", {
      id: "new-agent", role: "assistant", type: "message", phase: "final_answer",
      content: [{ type: "output_text", text: "New resumed response" }],
      internal_chat_message_metadata_passthrough: { turn_id: resumedTurnId },
    }),
  ]);

  assert.deepEqual(result.map((item) => item.body), [
    "Old imported request", "Old imported response", "New resumed request", "New resumed response",
  ]);
  assert.deepEqual(result.map((item) => item.kind), ["user", "assistant", "user", "assistant"]);
});

test("paginates newest transcript items backwards without reordering", () => {
  const trace = Array.from({ length: 125 }, (_value, index) => ({ key: `item-${index}` }));
  const newest = paginateCodexTranscript(trace, null, 60);
  assert.equal(newest.trace[0].key, "item-65");
  assert.equal(newest.trace.at(-1).key, "item-124");
  assert.equal(newest.nextCursor, "65");
  assert.equal(newest.totalTraceItems, 125);

  const older = paginateCodexTranscript(trace, Number(newest.nextCursor), 60);
  assert.equal(older.trace[0].key, "item-5");
  assert.equal(older.trace.at(-1).key, "item-64");
  assert.equal(older.nextCursor, "5");

  const oldest = paginateCodexTranscript(trace, Number(older.nextCursor), 60);
  assert.deepEqual(oldest.trace.map((item) => item.key), ["item-0", "item-1", "item-2", "item-3", "item-4"]);
  assert.equal(oldest.nextCursor, null);
});

test("projects durable completion clocks and turn elapsed time for narrative messages", () => {
  const turnId = "00000000-0000-4000-8000-000000000020";
  const result = projectCodexTranscript([
    { timestamp: "2026-09-05T10:00:00.000Z", ...record("event_msg", { type: "task_started", turn_id: turnId }) },
    { timestamp: "2026-09-05T10:00:00.120Z", ...record("event_msg", { type: "user_message", turn_id: turnId, message: "Start" }) },
    { timestamp: "2026-09-05T10:00:06.250Z", ...record("event_msg", { type: "agent_message", turn_id: turnId, message: "Finished" }) },
    { timestamp: "2026-09-05T10:00:06.400Z", ...record("event_msg", { type: "task_complete", turn_id: turnId }) },
  ]);

  assert.equal(result[0].completedAt, "2026-09-05T10:00:00.120Z");
  assert.equal(result[0].elapsedMs, 120);
  assert.equal(result[1].completedAt, "2026-09-05T10:00:06.250Z");
  assert.equal(result[1].elapsedMs, 6250);
});

test("native ThreadStore timing fields survive without outer rollout timestamps", () => {
  const records = [
    record("event_msg", { type: "task_started", turn_id: "native", started_at: 1788602400 }),
    record("event_msg", { type: "agent_message", message: "Working" }),
    record("event_msg", { type: "agent_message", message: "Done" }),
    record("event_msg", { type: "task_complete", turn_id: "native", started_at: 1788602400, completed_at: 1788602412, duration_ms: 12542 }),
  ];
  const before = JSON.stringify(records);
  const trace = projectCodexTranscript(records);
  assert.equal(trace.length, 2);
  for (const item of trace) {
    assert.equal(item.completedAt, "2026-09-05T10:00:12.000Z");
    assert.equal(item.elapsedMs, 12542);
    assert.equal(item.timingScope, "turn", "turn timing must not pretend to be an individual message clock");
  }
  assert.equal(JSON.stringify(records), before, "presentation never rewrites canonical history");
  const page = projectCodexTranscript(records.slice(1, 3), {
    itemOffset: 1, initialTurnId: "native", initialTurnStartedAt: "2026-09-05T10:00:00.000Z", timingRecords: [records[3]],
  });
  assert.equal(page[0].elapsedMs, 12542, "a paginated turn can use its completion marker from a newer page");
});

test("deduplication keeps precise item timing and does not fabricate missing clocks", () => {
  const trace = projectCodexTranscript([
    record("event_msg", { type: "task_started", turn_id: "native", started_at: 1788602400 }),
    record("event_msg", { type: "agent_message", message: "Done" }),
    record("event_msg", { type: "item_completed", turn_id: "native", completed_at_ms: 1788602404250,
      item: { type: "agentMessage", id: "message", text: "Done" } }),
    record("event_msg", { type: "task_complete", turn_id: "native", completed_at: 1788602412, duration_ms: 12542 }),
  ]);
  assert.equal(trace.length, 1);
  assert.equal(trace[0].completedAt, "2026-09-05T10:00:04.250Z");
  assert.equal(trace[0].elapsedMs, 4250);
  assert.equal(trace[0].timingScope, undefined);
  assert.equal(projectCodexTranscript([record("event_msg", { type: "agent_message", message: "Unknown" })])[0].completedAt, undefined);
});

test("legacy turn_context prevents resumed messages inheriting a previous turn's clock", () => {
  const trace = projectCodexTranscript([
    record("event_msg", { type: "task_started", turn_id: "old", started_at: 1788602400 }),
    record("event_msg", { type: "task_complete", turn_id: "old", completed_at: 1788602412, duration_ms: 12542 }),
    record("turn_context", { turn_id: "resumed" }),
    record("event_msg", { type: "agent_message", message: "Resumed reply" }),
    record("response_item", { type: "message", role: "assistant", content: [{ type: "output_text", text: "Resumed reply" }],
      internal_chat_message_metadata_passthrough: { turn_id: "resumed" } }),
  ], { recordedAt: new Map([[3, "2026-09-05T11:00:00.000Z"], [4, "2026-09-05T11:00:20.000Z"]]) });
  assert.equal(trace.length, 1);
  assert.equal(trace[0].turnId, "resumed");
  assert.equal(trace[0].completedAt, "2026-09-05T11:00:20.000Z");
  assert.equal(trace[0].elapsedMs, 20000);
  assert.equal(trace[0].elapsedApproximate, true);
});

test("canonical compaction remains a small durable transcript notice", () => {
  const trace = projectCodexTranscript([
    record("compacted", { message: "Long internal context summary", replacement_history: [] }),
  ]);
  assert.equal(trace.length, 1);
  assert.equal(trace[0].kind, "compaction");
  assert.equal(trace[0].body, "较早的上下文已自动压缩。");
  assert.equal(trace[0].body.includes("internal"), false);
});

test("projects nested task completion errors as durable readable history", () => {
  const turnId = "00000000-0000-4000-8000-000000000021";
  const result = projectCodexTranscript([
    { timestamp: "2026-09-05T11:00:00.000Z", ...record("event_msg", { type: "task_started", turn_id: turnId }) },
    { timestamp: "2026-09-05T11:00:00.100Z", ...record("event_msg", { type: "user_message", turn_id: turnId, message: "Continue" }) },
    { timestamp: "2026-09-05T11:00:00.400Z", ...record("event_msg", {
      type: "task_complete", turn_id: turnId,
      error: {
        message: JSON.stringify({ type: "error", status: 400, error: {
          type: "invalid_request_error",
          message: "The requested model requires a newer version of Codex.",
        } }),
        codex_error_info: "other",
      },
    }) },
  ]);

  assert.deepEqual(result.map((item) => item.kind), ["user", "error"]);
  assert.equal(result[1].body, "The requested model requires a newer version of Codex.");
  assert.equal(result[1].turnId, turnId);
  assert.equal(result[1].completedAt, "2026-09-05T11:00:00.400Z");
});

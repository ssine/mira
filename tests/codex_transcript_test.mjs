import assert from "node:assert/strict";
import test from "node:test";

import { paginateCodexTranscript, projectCodexTranscript } from "../server/codex-transcript.mjs";

const record = (type, payload) => ({ type, payload });

test("projects paginated item_completed history without duplicating command tools", () => {
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

  assert.deepEqual(result.map((item) => item.kind), ["user", "tool", "reasoning", "assistant"]);
  assert.equal(result.filter((item) => item.kind === "tool").length, 1);
  assert.match(result[1].body, /text\(true\)/);
  assert.match(result[1].body, /Script completed/);
  assert.equal(result[3].markdown, true);
  assert.match(result[3].body, /^## Done/);
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

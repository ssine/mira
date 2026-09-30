import assert from "node:assert/strict";
import test from "node:test";
import { TurnLifecycle } from "../node/internal/clauderuntime/assets/turn-lifecycle.mjs";

const tasks = (...entries) => ({ type: "system", subtype: "background_tasks_changed", tasks: entries });
const result = { type: "result", is_error: false };

test("background research outlives the parent's interim result and completes with its synthesis", () => {
  const state = new TurnLifecycle();
  assert.equal(state.observe(tasks({ task_id: "research" })), false);
  assert.equal(state.observe(result), false, "must not close the SDK input on the interim result");
  assert.equal(state.observe({ type: "system", subtype: "task_progress", task_id: "research" }), false);
  assert.equal(state.observe(tasks()), false, "wait for the parent's response after the task finishes");
  assert.equal(state.observe({ type: "system", subtype: "task_notification", status: "completed", task_id: "research" }), false);
  assert.equal(state.observe({ type: "assistant" }), false);
  assert.equal(state.observe(result), true);
});

test("task snapshots replace state independently of bookend ordering", () => {
  const state = new TurnLifecycle();
  state.observe(tasks({ task_id: "one" }, { task_id: "two" }));
  state.observe({ type: "system", subtype: "task_notification", task_id: "one", status: "completed" });
  assert.equal(state.observe(result), false);
  state.observe(tasks({ task_id: "two" }));
  assert.equal(state.observe(result), false);
  state.observe(tasks());
  state.observe({ type: "system", subtype: "task_started", task_id: "two" });
  assert.equal(state.observe(result), true);
});

test("ordinary completion, errors and ambient tasks retain bounded turn ownership", () => {
  assert.equal(new TurnLifecycle().observe(result), true);
  const ambient = new TurnLifecycle();
  ambient.observe(tasks({ task_id: "watcher", ambient: true }));
  assert.equal(ambient.observe(result), true);
  const failed = new TurnLifecycle();
  failed.observe(tasks({ task_id: "research" }));
  assert.equal(failed.observe({ ...result, is_error: true }), true);
  const child = new TurnLifecycle();
  assert.equal(child.observe({ ...result, parent_tool_use_id: "tool" }), false);
});

const lifecycle = (uuid, state) => ({ type: "command_lifecycle", command_uuid: uuid, state });

test("a steer folded at a tool boundary completes with the same result", () => {
  // Observed SDK order: queued, started at the next tool result, completed, then one result listing both prompts.
  const state = new TurnLifecycle();
  assert.equal(state.steer("steer"), true);
  state.observe(lifecycle("steer", "queued"));
  state.observe(lifecycle("steer", "started"));
  state.observe(lifecycle("steer", "completed"));
  assert.equal(state.observe({ ...result, user_message_uuids: ["first", "steer"] }), true);
});

test("a steer queued during the final answer keeps input open for its own response", () => {
  // Observed SDK order: the first result omits the queued steer; the second lists it before its completed lifecycle.
  const state = new TurnLifecycle();
  state.steer("steer");
  state.observe(lifecycle("steer", "queued"));
  assert.equal(state.observe({ ...result, user_message_uuids: ["first"] }), false);
  state.observe(lifecycle("first", "completed"));
  state.observe(lifecycle("steer", "started"));
  assert.equal(state.observe({ ...result, user_message_uuids: ["steer"] }), true);
  assert.equal(state.steer("late"), false, "a finished turn cannot accept more input");
});

test("cancelled steers and failed results release the turn", () => {
  const cancelled = new TurnLifecycle();
  cancelled.steer("steer");
  cancelled.observe(lifecycle("steer", "cancelled"));
  assert.equal(cancelled.observe(result), true);
  const failed = new TurnLifecycle();
  failed.steer("steer");
  assert.equal(failed.observe({ ...result, is_error: true }), true);
});

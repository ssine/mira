import assert from "node:assert/strict";
import test from "node:test";
import { projectThreadActivity } from "../server/thread-activity.mjs";
const event = (type, turn_id = "turn", extra = {}) => ({ payload: { type: "event_msg", timestamp: "2026-09-05T10:00:00Z", payload: { type, turn_id, ...extra } } });
test("canonical lifecycle projection handles late older completions, failure and abort", () => {
  assert.equal(projectThreadActivity([event("task_started")]).state, "running");
  assert.equal(projectThreadActivity([event("task_complete"), event("task_started")]).state, "idle");
  assert.equal(projectThreadActivity([event("task_complete", "older"), event("task_started")]).state, "running");
  assert.equal(projectThreadActivity([event("turn_aborted"), event("turn_started")]).state, "interrupted");
  assert.equal(projectThreadActivity([event("task_complete", "turn", { error: { message: "failed" } }), event("task_started")]).state, "failed");
  assert.equal(projectThreadActivity([event("task_complete"), event("error"), event("task_started")]).state, "failed");
  assert.equal(projectThreadActivity([event("error", "turn", { codex_error_info: "thread_rollback_failed" }), event("task_started")]).state, "running");
  assert.equal(projectThreadActivity([event("error", "turn", { codex_error_info: { active_turn_not_steerable: {} } }), event("task_started")]).state, "running");
  assert.equal(projectThreadActivity([event("error", "turn", { will_retry: true }), event("task_started")]).state, "running");
  assert.equal(projectThreadActivity([{ payload: { type: "future_tool", payload: { type: "task_complete" } } }, event("task_started")]).state, "running");
  assert.equal(projectThreadActivity([], false).state, "unknown");
});
test("native store events carry Unix seconds without a rollout timestamp envelope", () => {
  const started = { payload: { type: "event_msg", payload: { type: "task_started", turn_id: "native", started_at: 1788622585 } } };
  const ended = { payload: { type: "event_msg", payload: { type: "task_complete", turn_id: "native", completed_at: 1788622587 } } };
  assert.equal(projectThreadActivity([started]).startedAt, "2026-09-05T15:36:25.000Z");
  assert.equal(projectThreadActivity([ended, started]).updatedAt, "2026-09-05T15:36:27.000Z");
  const legacy = { payload: { type: "event_msg", payload: { type: "task_started", turn_id: "legacy" } }, created_at: new Date("2026-09-05T15:00:00Z") };
  assert.equal(projectThreadActivity([legacy]).startedAt, "2026-09-05T15:00:00.000Z");
});

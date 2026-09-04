import assert from "node:assert/strict";
import test from "node:test";
import { activitySummary, activityStatus, formatActivityDuration, toolItemView, responseToolView,
  reasoningText, reasoningHeading, summarizeActivities } from "../server/public/trace-activity.js";
import { projectCodexTranscript } from "../server/codex-transcript.mjs";

const record = (type, payload) => ({ type, payload });
const materialized = (item, turnId = "turn-1") => record("event_msg", { type: "item_completed", turn_id: turnId, item });

test("App Server and canonical command records share descriptions, details and durations", () => {
  const live = toolItemView({ type: "commandExecution", id: "read-1", command: "cat config.go", cwd: "/project",
    commandActions: [{ type: "read", command: "cat config.go", path: "/project/config.go", name: "config.go" }],
    status: "completed", aggregatedOutput: "package main", exitCode: 0, durationMs: 15200 });
  const item = { type: "CommandExecution", id: "read-1", command: ["cat", "config.go"], cwd: "/project",
    parsed_cmd: [{ type: "read", cmd: "cat config.go", path: "/project/config.go", name: "config.go" }],
    status: "completed", aggregated_output: "package main", exit_code: 0, duration: { secs: 15, nanos: 200000000 } };
  const history = projectCodexTranscript([materialized(item)])[0];
  assert.deepEqual(history.activity, live.activity);
  assert.equal(history.body, live.body);
  assert.equal(activitySummary(live.activity), "已读取 /project/config.go");
  assert.equal(formatActivityDuration(history.activity.durationMs), "15.2 秒");
  assert.equal(history.itemId, "read-1");
});

test("search, directory listing and unknown actions use structured data only", () => {
  const view = toolItemView({ type: "commandExecution", status: "inProgress", command: "opaque shell command",
    commandActions: [{ type: "search", query: "sshRelay", path: "server.mjs" },
      { type: "listFiles", path: "node" }, { type: "unknown", command: "go test ./..." }] });
  assert.equal(activitySummary(view.activity), "正在搜索 “sshRelay”（server.mjs） · 正在列出 node · 正在执行 go test ./...");
  assert.equal(summarizeActivities([view.activity]), "搜索 × 1 · 列出 × 1 · 执行 × 1");
  const unknown = toolItemView({ type: "commandExecution", command: "echo 'cat secrets.txt'" });
  assert.equal(unknown.activity.actions[0].kind, "run");
});

test("live file diffs and stored file maps have equivalent statistics and rename destinations", () => {
  const diff = "--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n-old\n+new\n+++ added line\n";
  const live = toolItemView({ type: "fileChange", status: "completed", changes: [
    { path: "main.go", kind: { type: "update", movePath: "new.go" }, diff },
  ] });
  const history = toolItemView({ type: "FileChange", status: "completed", changes: {
    "main.go": { type: "update", move_path: "new.go", unified_diff: diff },
  } });
  assert.deepEqual(live, history);
  assert.equal(activitySummary(live.activity), "已修改 main.go → new.go +2 −1");
  assert.match(live.body, /\+\+\+ added line/);
});

test("created/deleted files count content lines, including missing final newline", () => {
  const view = toolItemView({ type: "FileChange", changes: {
    "a.txt": { type: "add", content: "a\nb" },
    "b.txt": { type: "delete", content: "x\ny\n" },
    "empty.txt": { type: "add", content: "" },
  } });
  assert.deepEqual(view.activity.actions.map(({ kind, added, removed }) => [kind, added, removed]),
    [["create", 2, 0], ["delete", 0, 2], ["create", 0, 0]]);
  const live = toolItemView({ type: "fileChange", changes: [
    { path: "a.txt", kind: { type: "add" }, diff: "a\nb" },
    { path: "b.txt", kind: { type: "delete" }, diff: "x\ny\n" },
    { path: "empty.txt", kind: { type: "add" }, diff: "" },
  ] });
  assert.deepEqual(live.activity, view.activity);
});

test("failed, declined and interrupted work never claims successful execution", () => {
  for (const [status, expected] of [["failed", "失败"], ["declined", "已拒绝"], ["interrupted", "已中断"]]) {
    const view = toolItemView({ type: "commandExecution", command: "false", status });
    assert.match(activitySummary(view.activity), new RegExp(expected));
    assert.ok(!activitySummary(view.activity).startsWith("已执行"));
  }
  assert.equal(activityStatus("completed", 1), "failed");
  assert.equal(toolItemView({ type: "dynamicToolCall", tool: "screen", success: false }).activity.status, "failed");
});

test("missing and future metadata degrades to a bounded command description", () => {
  const view = toolItemView({ type: "commandExecution", command: "x".repeat(1000),
    commandActions: [{ type: "futureAction" }], durationMs: -1 });
  assert.ok(activitySummary(view.activity).length < 200);
  assert.equal(view.activity.durationMs, null);
  assert.equal(formatActivityDuration(null), "");
  assert.equal(formatActivityDuration(0), "0 ms");
  assert.equal(formatActivityDuration(62300), "1 分 2 秒");
  assert.equal(toolItemView({ type: "futureItem" }), null);
});

test("legacy shell calls and apply_patch have activities without executing or parsing code-mode JS", () => {
  const shell = responseToolView({ name: "exec_command", arguments: '{"cmd":"go test ./...","workdir":"/project"}' });
  assert.match(activitySummary(shell.activity), /已执行 go test/);
  const patch = responseToolView({ name: "apply_patch", input: "*** Begin Patch\n*** Add File: a.go\n+package a\n*** Update File: b.go\n@@\n-old\n+new\n*** End Patch" });
  assert.equal(activitySummary(patch.activity), "已创建 a.go +1 −0 · 已修改 b.go +1 −1");
  assert.equal(responseToolView({ name: "exec", input: "text(await tools.exec_command(...))" }), null);
});

test("exact tool IDs deduplicate representations, enrich history and retain unrelated commands", () => {
  const source = [
    record("event_msg", { type: "task_started", turn_id: "turn-1" }),
    record("response_item", { type: "function_call", call_id: "cmd-1", name: "exec_command", arguments: '{"cmd":"cat a"}' }),
    materialized({ id: "cmd-1", type: "CommandExecution", command: ["cat", "a"], status: "completed",
      parsed_cmd: [{ type: "read", path: "a" }], aggregated_output: "hello", exit_code: 0 }),
    record("response_item", { type: "function_call_output", call_id: "cmd-1", output: "hello" }),
    materialized({ id: "cmd-2", type: "CommandExecution", command: ["cat", "b"], status: "completed",
      parsed_cmd: [{ type: "read", path: "b" }] }),
  ];
  const before = JSON.stringify(source);
  const projected = projectCodexTranscript(source);
  assert.equal(projected.length, 2);
  assert.deepEqual(projected.map((entry) => entry.itemId), ["cmd-1", "cmd-2"]);
  assert.deepEqual(projected.map((entry) => activitySummary(entry.activity)), ["已读取 a", "已读取 b"]);
  assert.match(projected[0].body, /hello/);
  assert.equal(JSON.stringify(source), before, "projection must not mutate canonical data");
});

test("reused call IDs across turns remain independent, including output-first replay", () => {
  const source = ["turn-a", "turn-b"].flatMap((turn_id) => [
    record("event_msg", { type: "task_started", turn_id }),
    record("response_item", { type: "function_call_output", call_id: "same", output: turn_id }),
    record("response_item", { type: "function_call", call_id: "same", name: "exec_command", arguments: '{"cmd":"pwd"}' }),
  ]);
  const result = projectCodexTranscript(source);
  assert.equal(result.length, 2);
  assert.notEqual(result[0].key, result[1].key);
  assert.match(result[0].body, /turn-a/);
  assert.match(result[1].body, /turn-b/);
  assert.equal(result[0].activity.status, "completed");
});

test("empty/raw reasoning stays hidden and real summaries supply concise headings", () => {
  assert.equal(reasoningText({ summary: [], content: ["raw block"] }), "");
  assert.equal(reasoningText({ content: ["raw block"] }), "");
  assert.equal(reasoningHeading("\n**Planning native tests**\n\nMore detail"), "Planning native tests");
  const result = projectCodexTranscript([
    materialized({ id: "empty", type: "Reasoning", summary_text: [], raw_content: ["raw"] }),
    record("response_item", { type: "reasoning", content: ["raw"] }),
    record("event_msg", { type: "agent_reasoning_raw_content", text: "raw" }),
    materialized({ id: "real", type: "Reasoning", summary_text: ["**Inspecting files**\n\nChecking config."] }),
  ]);
  assert.equal(result.length, 1);
  assert.equal(reasoningHeading(result[0].body), "Inspecting files");
});

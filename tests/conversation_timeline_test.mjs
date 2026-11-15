import test from "node:test";
import assert from "node:assert/strict";
import { activitySegments, compactDuration, hourWindow, liveLanes, mergeTimeline, modelPhases, timelineLayout } from "../server/public/conversation-timeline.js";

const minute = 60_000, now = 1_800_000_000_000;

test("idle gaps over ten minutes split segments; shorter gaps join", () => {
  const lanes = [{ turns: [{ start: 0, end: 5 * minute }, { start: 12 * minute, end: 14 * minute }, { start: 3 * 60 * minute, end: 3 * 60 * minute + minute }], tools: [], messages: [] }];
  assert.deepEqual(activitySegments(lanes), [{ start: 0, end: 14 * minute }, { start: 180 * minute, end: 181 * minute }]);
  const layout = timelineLayout(activitySegments(lanes), 600);
  assert.ok(layout.width <= 601 && layout.position(181 * minute) > layout.position(14 * minute), "segments sit next to each other");
  assert.equal(compactDuration(166 * minute), "2小时46分");
});

test("running conversations grow unfinished work to now; stale and finished work stays", () => {
  const lanes = [{ threadId: "t", messages: [],
    turns: [{ start: now - 10 * minute, end: now - minute, open: true }, { start: now - 5 * 60 * minute, end: now - 4 * 60 * minute, open: true }, { start: now - 20 * minute, end: now - 15 * minute }],
    tools: [{ start: now - 3 * minute, end: now - 3 * minute, open: true, kind: "tool" }, { start: now - 9 * minute, end: now - 8 * minute, kind: "tool" }] }];
  const live = liveLanes(lanes, now, true)[0];
  assert.deepEqual(live.turns.map(turn => [turn.end === now, !!turn.live]), [[true, true], [false, false], [false, false]]);
  assert.deepEqual(live.tools.map(tool => [tool.end === now, !!tool.live]), [[true, true], [false, false]]);
  assert.equal(liveLanes(lanes, now, false), lanes, "idle conversations are unchanged");
  // The model phase before the running tool is bounded by it; the tool owns the tail.
  assert.deepEqual(modelPhases(live).filter(([start]) => start >= now - 10 * minute), [[now - 10 * minute, now - 9 * minute], [now - 8 * minute, now - 3 * minute]]);
});

test("the hour window ends now while running and at the last activity otherwise", () => {
  const lanes = [{ turns: [{ start: now - 3 * 60 * minute, end: now - 2 * 60 * minute }], tools: [], messages: [] }];
  assert.deepEqual(hourWindow(lanes, now, true), { start: now - 60 * minute, end: now });
  assert.deepEqual(hourWindow(lanes, now, false), { start: now - 3 * 60 * minute, end: now - 2 * 60 * minute });
  assert.equal(timelineLayout([hourWindow(lanes, now, true)], 720).width, 720, "one real-time segment fills the width");
});

test("incremental timeline reads merge by identity", () => {
  const base = { engine: "claude", cursor: "c1.10", lanes: [
    { threadId: "s", depth: 0, title: "root",
      turns: [{ id: "t1", start: 0, end: 5 * minute, status: "completed" }, { id: "t2", start: 6 * minute, end: 7 * minute, open: true }],
      tools: [{ id: "a", name: "Bash", start: 6 * minute, end: 6 * minute, open: true, kind: "tool", seq: 5 }, { id: "task", name: "Task · 调研", start: 6 * minute, end: 6 * minute, open: true, kind: "agent", seq: 6 }],
      messages: [{ at: 6 * minute, seq: 4, role: "user", preview: "go" }] },
    { threadId: "s:old", depth: 1, parentThreadId: "s", title: "Task · 旧", turns: [{ id: "old", start: minute, end: 2 * minute }], tools: [], messages: [] },
  ] };
  const merged = mergeTimeline(base, { engine: "claude", delta: true, cursor: "c1.14", lanes: [
    { threadId: "s", depth: 0, title: "root",
      turns: [{ id: "t1", start: 0, end: 5 * minute, status: "completed" }, { id: "t2", start: 6 * minute, end: 9 * minute, status: "completed" }],
      tools: [{ id: "a", start: 8 * minute, end: 8 * minute, close: true, seq: 12 }, { id: "missing", start: 8 * minute, end: 8 * minute, close: true, seq: 13 },
        { id: "b", name: "Read", start: 8 * minute, end: 8 * minute, open: true, kind: "tool", seq: 14 }],
      messages: [{ at: 9 * minute, seq: 14, role: "assistant", preview: "done" }] },
    // A subagent first seen in an increment: no title, the Task call is older.
    { threadId: "s:task", depth: 1, parentThreadId: "s", title: null, turns: [{ id: "task", start: 7 * minute, end: 8 * minute }], tools: [], messages: [] },
  ] });
  const root = merged.lanes.find(lane => lane.threadId === "s");
  assert.equal(merged.cursor, "c1.14");
  assert.equal(merged.delta, false);
  assert.deepEqual(root.turns.map(turn => [turn.id, turn.end / minute, !!turn.open]), [["t1", 5, false], ["t2", 9, false]]);
  assert.deepEqual(root.tools.map(tool => [tool.id, tool.name, tool.start / minute, tool.end / minute, !!tool.open]),
    [["a", "Bash", 6, 8, false], ["task", "Task · 调研", 6, 6, true], ["b", "Read", 8, 8, true]], "a close only ends a known tool");
  assert.deepEqual(root.messages.map(message => message.seq), [4, 14]);
  assert.equal(merged.lanes.find(lane => lane.threadId === "s:task").title, "Task · 调研");
  assert.ok(merged.lanes.some(lane => lane.threadId === "s:old"), "lanes absent from an update keep their items");

  // Codex: unchanged lanes keep everything; a reset lane starts over.
  const codex = { cursor: "x1.a", lanes: [{ threadId: "r", depth: 0, turns: [{ id: "x", start: 0, end: minute }], tools: [{ id: "", seq: 3, start: 0, end: minute }], messages: [] },
    { threadId: "c", depth: 1, parentThreadId: "r", turns: [{ id: "y", start: 0, end: minute }], tools: [], messages: [] }] };
  const next = mergeTimeline(codex, { delta: true, cursor: "x1.b", lanes: [{ threadId: "r", depth: 0, reset: true, turns: [{ id: "z", start: 2 * minute, end: 3 * minute }], tools: [], messages: [] },
    { threadId: "c", depth: 1, parentThreadId: "r", unchanged: true, turns: null, tools: null, messages: null }] });
  assert.deepEqual(next.lanes[0].turns.map(turn => turn.id), ["z"]);
  assert.deepEqual(next.lanes[1].turns.map(turn => turn.id), ["y"]);
  // A full response replaces the copy.
  assert.equal(mergeTimeline(codex, { lanes: [] }).lanes.length, 0);
});

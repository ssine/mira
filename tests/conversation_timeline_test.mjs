import test from "node:test";
import assert from "node:assert/strict";
import { activitySegments, compactDuration, hourWindow, liveLanes, modelPhases, timelineLayout } from "../server/public/conversation-timeline.js";

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

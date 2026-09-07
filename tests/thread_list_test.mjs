import assert from "node:assert/strict";
import test from "node:test";
import { projectThreadRecentWindowMs, splitProjectThreads } from "../server/public/thread-list.js";

const now = Date.parse("2026-09-08T12:00:00.000Z");
const thread = (threadId, ageMs) => ({ threadId, updatedAt: new Date(now - ageMs).toISOString() });

test("each project exposes at most four recent conversations", () => {
  const threads = [
    thread("fourth", 4_000), thread("old", projectThreadRecentWindowMs + 1), thread("second", 2_000),
    thread("sixth", 6_000), thread("first", 1_000), thread("fifth", 5_000), thread("third", 3_000),
  ];
  const result = splitProjectThreads(threads, now);
  assert.deepEqual(result.visible.map(value => value.threadId), ["first", "second", "third", "fourth"]);
  assert.deepEqual(result.hidden.map(value => value.threadId), ["fifth", "sixth", "old"]);
});

test("week boundary is visible while older and undated conversations remain hidden", () => {
  const boundary = thread("boundary", projectThreadRecentWindowMs);
  const older = thread("older", projectThreadRecentWindowMs + 1);
  const result = splitProjectThreads([{ threadId: "missing" }, older, boundary, { threadId: "invalid", updatedAt: "not-a-date" }], now);
  assert.deepEqual(result.visible, [boundary]);
  assert.deepEqual(result.hidden.map(value => value.threadId), ["older", "missing", "invalid"]);
});

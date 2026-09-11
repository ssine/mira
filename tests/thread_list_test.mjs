import assert from "node:assert/strict";
import test from "node:test";
import { buildThreadTree, projectThreadRecentWindowMs, splitProjectThreads } from "../server/public/thread-list.js";

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

test("subagents with missing source kinds form a tree before project grouping", () => {
  const root = { ...thread("root", 10_000), cwd: "/project", runtimeNodeId: "parent-node" };
  const child = { ...thread("child", 2_000), parentThreadId: "root", sourceKind: null, cwd: "/other", runtimeNodeId: "child-node" };
  const grandchild = { ...thread("grandchild", 1_000), parentThreadId: "child" };
  const fork = { ...thread("fork", 3_000), forkedFromId: "root" };
  const original = structuredClone([grandchild, child, root, fork]);
  const { roots, parentById } = buildThreadTree([grandchild, child, root, fork]);
  assert.deepEqual(roots.map(entry => entry.threadId), ["root", "fork"]);
  assert.equal(roots[0].thread, root);
  assert.equal(roots[0].children[0].thread, child);
  assert.equal(roots[0].children[0].children[0].thread, grandchild);
  assert.equal(roots[0].descendantCount, 2);
  assert.equal(roots[0].children[0].descendantCount, 1);
  assert.equal(parentById.get("grandchild"), "child");
  assert.deepEqual([grandchild, child, root, fork], original, "canonical metadata stays unchanged");
});

test("child activity keeps the family recent without consuming root conversation slots", () => {
  const roots = Array.from({ length: 5 }, (_, index) => thread(`root-${index}`, (index + 1) * 1_000));
  roots[0].updatedAt = new Date(now - projectThreadRecentWindowMs - 1).toISOString();
  const children = Array.from({ length: 6 }, (_, index) => ({ ...thread(`child-${index}`, index), parentThreadId: "root-0" }));
  const result = splitProjectThreads(buildThreadTree([...children, ...roots]).roots, now);
  assert.deepEqual(result.visible.map(entry => entry.threadId), ["root-0", "root-1", "root-2", "root-3"]);
  assert.deepEqual(result.hidden.map(entry => entry.threadId), ["root-4"]);
  assert.equal(result.visible[0].children.length, 6);
});

test("missing or archived parents leave a reachable subtree and cycles cannot hide rows", () => {
  const rows = [
    { ...thread("orphan", 1_000), parentThreadId: "not-in-this-list" },
    { ...thread("orphan-child", 2_000), parentThreadId: "orphan" },
    { ...thread("self", 3_000), parentThreadId: "self" },
    { ...thread("cycle-a", 4_000), parentThreadId: "cycle-b" },
    { ...thread("cycle-b", 5_000), parentThreadId: "cycle-c" },
    { ...thread("cycle-c", 6_000), parentThreadId: "cycle-a" },
  ];
  const { roots, parentById } = buildThreadTree(rows);
  assert.equal(roots.length, 3);
  assert.equal(roots[0].threadId, "orphan");
  assert.equal(roots[0].children[0].threadId, "orphan-child");
  const reachable = [...roots];
  for (let index = 0; index < reachable.length; index++) reachable.push(...reachable[index].children);
  assert.deepEqual(reachable.map(entry => entry.threadId).sort(), rows.map(row => row.threadId).sort());
  assert.equal(parentById.has("self"), false);
});

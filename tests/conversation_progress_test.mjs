import assert from "node:assert/strict";
import test from "node:test";
import { ReplyProgress } from "../server/public/conversation-progress.js";

test("reply hint starts synchronously, survives empty reasoning/tools, ends at first prose", () => {
  const progress = new ReplyProgress();
  const entry = progress.begin(null, null, 100);
  assert.equal(progress.current(null), entry);
  progress.update(entry, { threadId: "a" });
  for (const method of ["turn/started", "item/started", "item/reasoning/summaryTextDelta", "item/commandExecution/outputDelta"]) {
    progress.observe(method, { threadId: "a", turnId: "1", item: { type: "agentMessage", text: "" } });
    assert.equal(progress.current("a"), entry);
  }
  progress.observe("item/agentMessage/delta", { threadId: "a", turnId: "1", delta: "正文" });
  assert.equal(progress.current("a"), null);
  progress.update(entry, { phase: "late acknowledgement" });
  assert.equal(progress.current("a"), null);
});

test("turn and thread identity isolate hints, including completion before RPC acknowledgement", () => {
  const progress = new ReplyProgress();
  const a = progress.begin("a", "old");
  const b = progress.begin("b");
  progress.observe("turn/completed", { threadId: "a", turn: { id: "old" } });
  assert.equal(progress.current("a"), a);
  progress.observe("turn/started", { threadId: "a", turn: { id: "new" } });
  progress.observe("turn/completed", { threadId: "a", turn: { id: "new" } });
  progress.update(a, { turnId: "new" });
  assert.equal(progress.current("a"), null);
  assert.equal(progress.current("b"), b);
  progress.observe("turn/started", { threadId: "b", turn: { id: "2" } });
  progress.observe("error", { threadId: "b", turnId: "2", willRetry: true });
  assert.match(b.phase, /重试/);
  progress.observe("error", { threadId: "b", turnId: "2", willRetry: false });
  assert.equal(progress.current("b"), null);
});

test("connection setup preserves only its submission; disconnect clears it", () => {
  const progress = new ReplyProgress();
  progress.begin("old");
  const entry = progress.begin(null);
  progress.clear(entry);
  assert.equal(progress.entries.size, 1);
  progress.clear();
  assert.equal(progress.current(null), null);
});

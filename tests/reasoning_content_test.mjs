import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { reasoningParts, reasoningText } from "../server/public/trace-activity.js";

test("readable reasoning content is used only when the summary is empty", () => {
  assert.equal(reasoningText({ summary: [], content: [{type: "reasoning_text", text: "Thinking."}] }), "Thinking.");
  assert.equal(reasoningText({ summary: ["Summary."], content: ["Thinking."] }), "Summary.");
  assert.deepEqual(reasoningParts({ summary: [], encrypted_content: "opaque" }), []);
  assert.equal(reasoningText({ summary: [], content: ["First", "Second"] }), "First\n\nSecond");
});

test("live content streams immediately, keeps part order, and yields to summaries", () => {
  const source = fs.readFileSync(new URL("../server/public/app.js", import.meta.url), "utf8");
  const start = source.indexOf("function appendReasoningSummary(");
  const fn = source.slice(start, source.indexOf("\nfunction renderThread", start));
  let card, body;
  const context = vm.createContext({
    liveTraceKey: () => "key", CSS: { escape: x => x },
    $: () => ({ querySelector: () => card }), traceNearBottom: () => false,
    upsertTrace: () => card ??= {},
    queueTraceStreamRender: (_card, value) => { body = value; },
  });
  vm.runInContext(fn, context);
  const send = (delta, index, content = true) => context.appendReasoningSummary({delta, contentIndex:index, summaryIndex:index}, content);
  send("Second", 1); send("First", 0); send("!", 0);
  assert.equal(body, "First!\n\nSecond");
  send("Summary", 0, false); send(" hidden", 1);
  assert.equal(body, "Summary");
  send("!", 0, false);
  assert.equal(body, "Summary!");
  assert.match(source, /method === "item\/reasoning\/textDelta"[\s\S]*?appendReasoningSummary\(params, true\)/);
});

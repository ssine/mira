import test from "node:test";
import assert from "node:assert/strict";
import { priceUsage, usageCounts } from "../server/model-pricing.mjs";
import { applyCostRecord, costEstimate, newCostProjection, turnCostEstimates } from "../server/thread-cost-estimate.mjs";
import { formatEstimatedCost, compactCost, threadTimestamp } from "../server/public/thread-usage.js";

const usage = (input, cached, output, write = 0) => ({ input_tokens: input, cached_input_tokens: cached, output_tokens: output, cache_write_input_tokens: write });
const context = (model, turnId = null) => ({ type: "turn_context", payload: { model, ...(turnId ? {turn_id: turnId} : {}) } });
const event = (total, last = total, turnId = null) => ({ type: "event_msg", payload: { type: "token_count",
  ...(turnId ? {turn_id: turnId} : {}), info: { total_token_usage: total, last_token_usage: last } } });
const dollars = price => Number(price.input + price.cached + price.write + price.output) / 1e9;

test("Standard estimate prices uncached input, cache reads/writes and output separately", () => {
  assert.equal(dollars(priceUsage("gpt-6-astra", usageCounts(usage(100000, 80000, 10000)))), 0.78);
  assert.equal(dollars(priceUsage("gpt-6-astra", usageCounts(usage(100000, 80000, 10000, 10000)))), 0.805);
  assert.equal(dollars(priceUsage("gpt-5.6-sol", usageCounts(usage(100000, 80000, 10000)))), 0.312);
  assert.equal(usageCounts(usage(10, 8, 1, 3)), null);
  assert.equal(usageCounts(usage(-1, 0, 0)), null);
  assert.equal(usageCounts({ input_tokens: 10, output_tokens: 1 }), null);
  assert.equal(priceUsage("unpriced-model", [10, 8, 0, 1]), null);
  assert.equal(priceUsage("gpt-5.5", [10, 8, 1, 1]), null);
});

test("long context is determined per request at the documented strict threshold", () => {
  assert.equal(dollars(priceUsage("gpt-6-astra", [272000, 200000, 0, 1000])), 0.97);
  assert.equal(dollars(priceUsage("gpt-6-astra", [272001, 200000, 0, 1000])), 1.91502);
  const state = newCostProjection(); applyCostRecord(state, context("gpt-6-astra"));
  for (let i = 1; i <= 4; i++) applyCostRecord(state, event(usage(100000 * i, 80000 * i, 1000 * i), usage(100000, 80000, 1000)));
  const estimate = costEstimate(state, {});
  assert.equal(estimate.amount, 1.32); assert.equal(estimate.longRequests, 0);
});

test("model changes and repeated cumulative snapshots never double-count", () => {
  const state = newCostProjection();
  applyCostRecord(state, context("gpt-6-astra"));
  const first = event(usage(100000, 80000, 1000));
  applyCostRecord(state, first); applyCostRecord(state, first);
  applyCostRecord(state, context("gpt-5.6-sol")); applyCostRecord(state, first);
  applyCostRecord(state, event(usage(150000, 120000, 1500), usage(50000, 40000, 500)));
  applyCostRecord(state, { type: "function_call_output", payload: first.payload });
  const estimate = costEstimate(state, {});
  assert.equal(estimate.amount, 0.396); assert.equal(estimate.status, "complete");
  assert.equal(estimate.pricedRequests, 2); assert.deepEqual(estimate.models, ["gpt-6-astra", "gpt-5.6-sol"]);
});

test("turn estimates attribute each request to its durable turn and actual model", () => {
  const state = newCostProjection();
  applyCostRecord(state, context("gpt-6-astra", "turn-a"));
  applyCostRecord(state, event(usage(100000, 80000, 1000)));
  applyCostRecord(state, context("gpt-5.6-sol", "turn-b"));
  applyCostRecord(state, event(usage(150000, 120000, 1500), usage(50000, 40000, 500)));
  applyCostRecord(state, event(usage(150000, 120000, 1500), usage(50000, 40000, 500)), "ignored-thread-id");
  const turns = turnCostEstimates(state);
  assert.equal(turns["turn-a"].amount, 0.33);
  assert.deepEqual(turns["turn-a"].models, ["gpt-6-astra"]);
  assert.equal(turns["turn-b"].amount, 0.066);
  assert.deepEqual(turns["turn-b"].models, ["gpt-5.6-sol"]);
  assert.equal(turns["turn-b"].pricedRequests, 1, "repeated usage snapshots are not charged twice");
});

test("turn estimates preserve honest zero, partial and unavailable states", () => {
  const state = newCostProjection();
  applyCostRecord(state, context("gpt-6-astra", "zero"));
  applyCostRecord(state, event(usage(0, 0, 0)));
  applyCostRecord(state, context("unknown-model", "unknown"));
  applyCostRecord(state, event(usage(100, 80, 10), usage(100, 80, 10)));
  applyCostRecord(state, context("gpt-6-astra", "partial"));
  applyCostRecord(state, event(usage(300, 240, 30), usage(100, 80, 10)));
  const turns = turnCostEstimates(state);
  assert.equal(turns.zero.amount, 0); assert.equal(turns.zero.status, "complete");
  assert.equal(turns.unknown.amount, null); assert.equal(turns.unknown.status, "unavailable");
  assert.deepEqual(turns.unknown.reasons, ["unknown_model"]);
  assert.equal(turns.partial.amount, 0.00078); assert.equal(turns.partial.status, "partial");
  assert.deepEqual(turns.partial.reasons, ["missing_request_usage"]);
});

test("cached per-turn projections retain a bounded recent window", () => {
  const state = newCostProjection();
  for (let index = 0; index < 520; index++) {
    applyCostRecord(state, context("gpt-6-astra", `turn-${index}`));
    applyCostRecord(state, event(usage(0, 0, 0)));
  }
  const turns = turnCostEstimates(state);
  assert.equal(Object.keys(turns).length, 512);
  assert.equal(turns["turn-0"], undefined);
  assert.equal(turns["turn-519"].amount, 0);
});

test("missing history/model/usage remains partial or unavailable; zero is explicit", () => {
  const state = newCostProjection();
  assert.equal(costEstimate(state, {}).amount, null);
  applyCostRecord(state, context("gpt-6-astra"));
  applyCostRecord(state, event(usage(200000, 160000, 2000), usage(100000, 80000, 1000)));
  assert.equal(costEstimate(state, {}).amount, 0.33); assert.equal(costEstimate(state, {}).status, "partial");
  const unknown = newCostProjection(); applyCostRecord(unknown, event(usage(10, 8, 1)));
  assert.equal(costEstimate(unknown, {}).amount, null);
  const empty = newCostProjection(); applyCostRecord(empty, event(usage(0, 0, 0)));
  assert.equal(costEstimate(empty, {}).amount, 0);
  assert.equal(costEstimate(empty, { tokenUsage: { inputTokens: 10, cachedInputTokens: 8, outputTokens: 1 } }).status, "unavailable");
});

test("context-full sentinels and fresh counter epochs do not fabricate costs", () => {
  const state = newCostProjection(); applyCostRecord(state, context("gpt-6-astra"));
  applyCostRecord(state, event(usage(100000, 80000, 1000)));
  applyCostRecord(state, event({ ...usage(0, 0, 0), total_tokens: 272000 }));
  applyCostRecord(state, event(usage(100000, 80000, 1000)));
  assert.equal(costEstimate(state, {}).amount, 0.66);
  assert.equal(formatEstimatedCost(0), "$0.00");
  assert.equal(formatEstimatedCost(0.00000002), "< $0.0001");
  assert.equal(formatEstimatedCost(null), "暂无法估算");
});


test("persisted settings capture model switches before the next context item", () => {
  const state = newCostProjection(); applyCostRecord(state, context("gpt-6-astra"));
  applyCostRecord(state, event(usage(100000, 80000, 1000)));
  applyCostRecord(state, {type:"event_msg",payload:{type:"thread_settings_applied",thread_settings:{model:"gpt-5.6-sol"}}});
  applyCostRecord(state, event(usage(150000, 120000, 1500), usage(50000, 40000, 500)));
  assert.equal(costEstimate(state, {}).amount, 0.396);
});

test("a fork prices only requests after its child-owned settings boundary", () => {
  const threadId = "20000000-0000-4000-8000-000000000002";
  const sourceId = "20000000-0000-4000-8000-000000000001";
  const state = newCostProjection(true);
  applyCostRecord(state, context("gpt-6-astra"), threadId);
  applyCostRecord(state, event(usage(100000, 80000, 1000)), threadId);
  applyCostRecord(state, {type:"event_msg",payload:{type:"thread_settings_applied",thread_id:sourceId,
    thread_settings:{model:"gpt-6-astra"}}}, threadId);
  applyCostRecord(state, event(usage(150000, 120000, 1500), usage(50000, 40000, 500)), threadId);
  assert.equal(state.scopeStarted, false, "copied source settings do not start fork billing");
  applyCostRecord(state, {type:"event_msg",payload:{type:"thread_settings_applied",thread_id:threadId,
    thread_settings:{model:"gpt-5.6-sol"}}}, threadId);
  const emptyFork = costEstimate(state, {tokenUsage:{inputTokens:150000,cachedInputTokens:120000,outputTokens:1500}});
  assert.equal(emptyFork.amount, 0); assert.equal(emptyFork.status, "complete"); assert.equal(emptyFork.scope, "fork");
  applyCostRecord(state, event(usage(200000, 160000, 2000), usage(50000, 40000, 500)), threadId);
  assert.deepEqual(costEstimate(state, {}).models, ["gpt-5.6-sol"]);
  assert.equal(costEstimate(state, {}).amount, 0.066);
  const missing = newCostProjection(true);
  applyCostRecord(missing, context("gpt-6-astra"), threadId);
  applyCostRecord(missing, event(usage(100000, 80000, 1000)), threadId);
  const unavailable = costEstimate(missing, {});
  assert.equal(unavailable.amount, null); assert.equal(unavailable.status, "unavailable");
  assert.deepEqual(unavailable.reasons, ["fork_boundary_missing"]); assert.equal(unavailable.scope, "fork");
});


test("sidebar prices and calendar timestamps stay compact", () => {
  assert.equal(compactCost({amount:114.82,status:"complete"}), "$114.82");
  assert.equal(compactCost({amount:0.007,status:"partial"}), "$0.007*");
  assert.equal(compactCost({amount:0.0001,status:"partial"}), "<$0.001*");
  assert.equal(compactCost({amount:null,status:"unavailable"}), "—");
  const now = new Date(2026,8,9,15,0);
  assert.equal(threadTimestamp(new Date(2026,8,9,9,5),now), "09:05");
  assert.equal(threadTimestamp(new Date(2026,8,2,9,5),now), "星期三");
  assert.equal(threadTimestamp(new Date(2026,8,1,9,5),now), "09/01");
  assert.equal(threadTimestamp(new Date(2025,8,1,9,5),now), "2025/09/01");
  assert.equal(threadTimestamp("invalid",now), "—");
});

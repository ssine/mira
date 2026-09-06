import { modelPrices, priceUsage, pricingDate, pricingSource, usageCounts } from "./model-pricing.mjs";
import { costEventPredicate } from "./thread-cost-migration.mjs";

const equal = (a, b) => a && b && a.every((value, index) => value === b[index]);
const zero = [0, 0, 0, 0];
const maximumCachedTurnCosts = 512;
export function newCostProjection(awaitForkBoundary = false, turnFilter = null) {
  return { model: null, last: zero, observed: false, pricedRequests: 0, unpricedRequests: 0,
    longRequests: 0, models: [], reasons: [], input: 0n, cached: 0n, write: 0n, output: 0n,
    turnId: null, turns: new Map(), turnFilter, turnLimit: turnFilter?.size ?? maximumCachedTurnCosts,
    awaitForkBoundary, scopeStarted: !awaitForkBoundary };
}
function incomplete(state, reason, turn = null) {
  if (!state.reasons.includes(reason)) state.reasons.push(reason);
  if (turn && !turn.reasons.includes(reason)) turn.reasons.push(reason);
}
function turnProjection(state, turnId, create = false) {
  if (typeof turnId !== "string" || !turnId || state.turnFilter && !state.turnFilter.has(turnId)) return null;
  let turn = state.turns.get(turnId);
  if (!turn && create) {
    turn = { observed: false, pricedRequests: 0, unpricedRequests: 0, longRequests: 0,
      models: [], reasons: [], input: 0n, cached: 0n, write: 0n, output: 0n };
    while (state.turns.size >= state.turnLimit && state.turns.size) state.turns.delete(state.turns.keys().next().value);
    if (!state.turnLimit) return null;
    state.turns.set(turnId, turn);
  }
  return turn ?? null;
}

export function applyCostRecord(state, record, threadId = null) {
  const payload = record?.payload;
  if (record?.type === "turn_context") {
    state.model = typeof payload?.model === "string" ? payload.model : null;
    if (typeof payload?.turn_id === "string" && payload.turn_id) state.turnId = payload.turn_id;
    return;
  }
  if (record?.type !== "event_msg") return;
  if (payload?.type === "thread_settings_applied") {
    state.model = payload.thread_settings?.model ?? null;
    // A copied fork ends with settings owned by the new child. Inherited
    // settings retain the source thread ID, so this is a durable local boundary
    // even when the source thread is later deleted.
    if (state.awaitForkBoundary && !state.scopeStarted && payload.thread_id === threadId) {
      state.scopeStarted = true;
      state.turnId = typeof payload.turn_id === "string" && payload.turn_id ? payload.turn_id : null;
    }
    return;
  }
  if (payload?.type !== "token_count" || !payload.info) return;
  const turnId = typeof payload.turn_id === "string" && payload.turn_id ? payload.turn_id : state.turnId;
  if (typeof turnId === "string" && turnId) state.turnId = turnId;
  const turn = state.scopeStarted ? turnProjection(state, turnId, true) : null;
  const total = usageCounts(payload.info.total_token_usage), last = usageCounts(payload.info.last_token_usage);
  if (!total) { if (state.scopeStarted) incomplete(state, "invalid_usage", turn); return; }
  state.observed = true;
  if (turn) turn.observed = true;
  if (equal(total, state.last)) return; // repeated snapshots/rate-limit-only notifications
  const delta = total.map((value, index) => value - state.last[index]);
  state.last = total;
  // Read copied counters to establish the baseline, but never price them.
  if (!state.scopeStarted) return;
  // Codex can reset counters to a full-context sentinel, or start a new usage
  // epoch after restoration. Sentinels have no input/output and cost nothing.
  if (equal(total, zero)) return;
  if (!last || last.some((value, index) => value > total[index])) {
    state.unpricedRequests++;
    if (turn) turn.unpricedRequests++;
    incomplete(state, "missing_request_usage", turn);
    return;
  }
  if (!equal(total, last) && delta.some((value, index) => value < last[index])) {
    state.unpricedRequests++;
    if (turn) turn.unpricedRequests++;
    incomplete(state, "inconsistent_usage", turn);
    return;
  }
  if (!equal(total, last) && !equal(delta, last)) incomplete(state, "missing_request_usage", turn);
  // The first cumulative snapshot can contain older requests whose model and
  // per-request context length are unknown. Only its final request is priced.
  if (state.pricedRequests === 0 && state.unpricedRequests === 0 && !equal(delta, last)) incomplete(state, "missing_request_usage", turn);
  if (equal(last, zero)) return;
  const price = priceUsage(state.model, last);
  if (!price) {
    state.unpricedRequests++;
    if (turn) turn.unpricedRequests++;
    incomplete(state, state.model && Object.hasOwn(modelPrices, state.model) ? "unsupported_cache_write" : "unknown_model", turn);
    return;
  }
  for (const kind of ["input", "cached", "write", "output"]) {
    state[kind] += price[kind];
    if (turn) turn[kind] += price[kind];
  }
  state.pricedRequests++;
  if (turn) turn.pricedRequests++;
  if (price.long) state.longRequests++;
  if (price.long && turn) turn.longRequests++;
  if (!state.models.includes(state.model)) state.models.push(state.model);
  if (turn && !turn.models.includes(state.model)) turn.models.push(state.model);
}

function pricedEstimate(state) {
  const available = state.pricedRequests > 0 || state.observed && state.unpricedRequests === 0 && !state.reasons.length;
  const breakdown = Object.fromEntries(["input", "cached", "write", "output"].map(kind => [kind, Number(state[kind]) / 1e9]));
  return {
    currency: "USD", basis: "standard", pricingDate, pricingSource,
    status: available ? state.reasons.length ? "partial" : "complete" : "unavailable",
    amount: available ? Number(state.input + state.cached + state.write + state.output) / 1e9 : null,
    breakdown, models: state.models, pricedRequests: state.pricedRequests, unpricedRequests: state.unpricedRequests,
    longRequests: state.longRequests, reasons: [...new Set(state.reasons)],
  };
}

export function turnCostEstimates(state) {
  return Object.fromEntries([...state.turns].map(([turnId, turn]) => [turnId, pricedEstimate(turn)]));
}

export function costEstimate(state, thread) {
  const reasons = [...state.reasons];
  if (state.awaitForkBoundary && !state.scopeStarted) reasons.push("fork_boundary_missing");
  const usage = thread.tokenUsage;
  if (usage && (!state.observed || usage.inputTokens !== state.last[0] || usage.cachedInputTokens !== state.last[1] || usage.outputTokens !== state.last[3])) reasons.push("usage_pending");
  const knownZero = !reasons.length && state.scopeStarted &&
    ((state.awaitForkBoundary && state.pricedRequests === 0 && state.unpricedRequests === 0) ||
      (state.observed && equal(state.last, zero)));
  const available = state.pricedRequests > 0 || knownZero;
  const breakdown = Object.fromEntries(["input", "cached", "write", "output"].map(kind => [kind, Number(state[kind]) / 1e9]));
  return { currency: "USD", basis: "standard", pricingDate, pricingSource,
    scope: state.awaitForkBoundary ? "fork" : "thread",
    status: available ? reasons.length ? "partial" : "complete" : "unavailable",
    amount: available ? Number(state.input + state.cached + state.write + state.output) / 1e9 : null,
    breakdown, models: state.models, pricedRequests: state.pricedRequests, unpricedRequests: state.unpricedRequests,
    longRequests: state.longRequests, reasons: [...new Set(reasons)] };
}

const pools = new WeakMap();
async function applyCostRows(pool, storeId, thread, state, after = 0) {
  let cursor = after;
  while (cursor < thread.itemCount) {
    const result = await pool.query(`SELECT item_seq,payload FROM codex_thread_events
      WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 AND item_seq<=$5 AND ${costEventPredicate}
      ORDER BY item_seq LIMIT 256`, [storeId, thread.threadId, thread.generation, cursor, thread.itemCount]);
    for (const row of result.rows) applyCostRecord(state, row.payload, thread.threadId);
    if (result.rows.length < 256) break;
    cursor = Number(result.rows.at(-1).item_seq);
  }
  return state;
}

async function getCostProjection(pool, storeId, thread) {
  let cache = pools.get(pool);
  if (!cache) { cache = { entries: new Map(), jobs: new Map() }; pools.set(pool, cache); }
  const key = JSON.stringify([storeId, thread.threadId, thread.generation, thread.forkedFromId ?? null]);
  const jobKey = `${key}:${thread.itemCount}`;
  let job = cache.jobs.get(jobKey);
  if (!job) {
    job = (async () => {
      const previous = cache.entries.get(key);
      const entry = previous && previous.itemCount <= thread.itemCount
        ? { itemCount: previous.itemCount, state: structuredClone(previous.state) }
        : { itemCount: 0, state: newCostProjection(Boolean(thread.forkedFromId)) };
      await applyCostRows(pool, storeId, thread, entry.state, entry.itemCount);
      entry.itemCount = thread.itemCount;
      if (!cache.entries.has(key) || cache.entries.get(key).itemCount <= entry.itemCount) cache.entries.set(key, entry);
      while (cache.entries.size > 100) cache.entries.delete(cache.entries.keys().next().value);
      return entry.state;
    })();
    cache.jobs.set(jobKey, job);
    void job.finally(() => { if (cache.jobs.get(jobKey) === job) cache.jobs.delete(jobKey); }).catch(() => {});
  }
  return job;
}

export async function getThreadCostEstimate(pool, storeId, thread) {
  return costEstimate(await getCostProjection(pool, storeId, thread), thread);
}

export async function getThreadTurnCostEstimates(pool, storeId, thread, turnIds = null) {
  const state = await getCostProjection(pool, storeId, thread);
  if (!turnIds) return turnCostEstimates(state);
  const wanted = [...new Set(turnIds.filter(turnId => typeof turnId === "string" && turnId))];
  const estimates = turnCostEstimates(state);
  const missing = wanted.filter(turnId => !Object.hasOwn(estimates, turnId));
  if (missing.length) {
    const targeted = newCostProjection(Boolean(thread.forkedFromId), new Set(missing));
    await applyCostRows(pool, storeId, thread, targeted);
    Object.assign(estimates, turnCostEstimates(targeted));
  }
  return Object.fromEntries(wanted.filter(turnId => Object.hasOwn(estimates, turnId)).map(turnId => [turnId, estimates[turnId]]));
}

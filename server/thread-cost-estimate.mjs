import { modelPrices, priceUsage, pricingDate, pricingSource, usageCounts } from "./model-pricing.mjs";
import { costEventPredicate } from "./thread-cost-migration.mjs";

const equal = (a, b) => a && b && a.every((value, index) => value === b[index]);
const zero = [0, 0, 0, 0];
export function newCostProjection() {
  return { model: null, last: zero, observed: false, pricedRequests: 0, unpricedRequests: 0,
    longRequests: 0, models: [], reasons: [], input: 0n, cached: 0n, write: 0n, output: 0n };
}
function incomplete(state, reason) {
  if (!state.reasons.includes(reason)) state.reasons.push(reason);
}

export function applyCostRecord(state, record) {
  const payload = record?.payload;
  if (record?.type === "turn_context") { state.model = typeof payload?.model === "string" ? payload.model : null; return; }
  if (record?.type !== "event_msg") return;
  if (payload?.type === "thread_settings_applied") { state.model = payload.thread_settings?.model ?? null; return; }
  if (payload?.type !== "token_count" || !payload.info) return;
  const total = usageCounts(payload.info.total_token_usage), last = usageCounts(payload.info.last_token_usage);
  if (!total) { incomplete(state, "invalid_usage"); return; }
  state.observed = true;
  if (equal(total, state.last)) return; // repeated snapshots/rate-limit-only notifications
  const delta = total.map((value, index) => value - state.last[index]);
  state.last = total;
  // Codex can reset counters to a full-context sentinel, or start a new usage
  // epoch after restoration. Sentinels have no input/output and cost nothing.
  if (equal(total, zero)) return;
  if (!last || last.some((value, index) => value > total[index])) {
    state.unpricedRequests++; incomplete(state, "missing_request_usage"); return;
  }
  if (!equal(total, last) && delta.some((value, index) => value < last[index])) {
    state.unpricedRequests++; incomplete(state, "inconsistent_usage"); return;
  }
  if (!equal(total, last) && !equal(delta, last)) incomplete(state, "missing_request_usage");
  // The first cumulative snapshot can contain older requests whose model and
  // per-request context length are unknown. Only its final request is priced.
  if (state.pricedRequests === 0 && state.unpricedRequests === 0 && !equal(delta, last)) incomplete(state, "missing_request_usage");
  if (equal(last, zero)) return;
  const price = priceUsage(state.model, last);
  if (!price) {
    state.unpricedRequests++; incomplete(state, state.model && Object.hasOwn(modelPrices, state.model) ? "unsupported_cache_write" : "unknown_model"); return;
  }
  for (const kind of ["input", "cached", "write", "output"]) state[kind] += price[kind];
  state.pricedRequests++;
  if (price.long) state.longRequests++;
  if (!state.models.includes(state.model)) state.models.push(state.model);
}

export function costEstimate(state, thread) {
  const reasons = [...state.reasons];
  const usage = thread.tokenUsage;
  if (usage && (!state.observed || usage.inputTokens !== state.last[0] || usage.cachedInputTokens !== state.last[1] || usage.outputTokens !== state.last[3])) reasons.push("usage_pending");
  const knownZero = state.observed && equal(state.last, zero) && !reasons.length;
  const available = state.pricedRequests > 0 || knownZero;
  const breakdown = Object.fromEntries(["input", "cached", "write", "output"].map(kind => [kind, Number(state[kind]) / 1e9]));
  return { currency: "USD", basis: "standard", pricingDate, pricingSource,
    status: available ? reasons.length ? "partial" : "complete" : "unavailable",
    amount: available ? Number(state.input + state.cached + state.write + state.output) / 1e9 : null,
    breakdown, models: state.models, pricedRequests: state.pricedRequests, unpricedRequests: state.unpricedRequests,
    longRequests: state.longRequests, reasons: [...new Set(reasons)] };
}

const pools = new WeakMap();
export async function getThreadCostEstimate(pool, storeId, thread) {
  let cache = pools.get(pool);
  if (!cache) { cache = { entries: new Map(), jobs: new Map() }; pools.set(pool, cache); }
  const key = JSON.stringify([storeId, thread.threadId, thread.generation]);
  const jobKey = `${key}:${thread.itemCount}`;
  let job = cache.jobs.get(jobKey);
  if (!job) {
    job = (async () => {
      const previous = cache.entries.get(key);
      const entry = previous && previous.itemCount <= thread.itemCount
        ? { itemCount: previous.itemCount, state: structuredClone(previous.state) } : { itemCount: 0, state: newCostProjection() };
      let cursor = entry.itemCount;
      while (cursor < thread.itemCount) {
        const result = await pool.query(`SELECT item_seq,payload FROM codex_thread_events
          WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 AND item_seq<=$5 AND ${costEventPredicate}
          ORDER BY item_seq LIMIT 256`, [storeId, thread.threadId, thread.generation, cursor, thread.itemCount]);
        for (const row of result.rows) applyCostRecord(entry.state, row.payload);
        if (result.rows.length < 256) break;
        cursor = Number(result.rows.at(-1).item_seq);
      }
      entry.itemCount = thread.itemCount;
      if (!cache.entries.has(key) || cache.entries.get(key).itemCount <= entry.itemCount) cache.entries.set(key, entry);
      while (cache.entries.size > 100) cache.entries.delete(cache.entries.keys().next().value);
      return entry.state;
    })();
    cache.jobs.set(jobKey, job);
    void job.finally(() => { if (cache.jobs.get(jobKey) === job) cache.jobs.delete(jobKey); }).catch(() => {});
  }
  return costEstimate(await job, thread);
}

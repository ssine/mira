import { randomUUID } from "node:crypto";
import { loadTranscript } from "./session-cache.mjs";

export function serverClient({
  endpoint,
  credential,
  sessionId,
  turnId,
  revision,
  cache,
  eventBatchV1,
}) {
  const base = new URL(endpoint);
  if (
    !["http:", "https:"].includes(base.protocol) ||
    base.username ||
    base.password ||
    base.search ||
    base.hash
  )
    throw new Error("Invalid Mira endpoint");
  const root = `/v1/claude/sessions/${sessionId}`;
  async function request(path, body, { raw = false } = {}) {
    // Freeze the envelope once: a lost response must never create another write.
    const options = {
      method: body === undefined ? "GET" : "POST",
      redirect: "error",
      headers: {
        authorization: `Bearer ${credential}`,
        "content-type": raw ? "application/x-ndjson" : "application/json",
        "x-mira-claude-turn": turnId,
        "x-mira-claude-revision": String(revision),
      },
      body: body === undefined ? undefined : raw ? body : JSON.stringify(body),
    };
    // One deadline for all attempts, below the SDK's 60s append timeout. Quick
    // gateway failures during a Server restart must not exhaust retries in <1s.
    const deadline = performance.now() + 45_000;
    for (let attempt = 0; ; attempt++) {
      let retryable = true;
      try {
        const response = await fetch(new URL(root + path, base), {
          ...options,
          signal: AbortSignal.timeout(Math.max(1, Math.ceil(deadline - performance.now()))),
        });
        if (response.ok) return response;
        retryable = [408, 429, 500, 502, 503, 504].includes(response.status);
        await response.body?.cancel();
        throw new Error(`Mira Claude storage failed (HTTP ${response.status})`);
      } catch (error) {
        const delay = Math.min(250 * 2 ** Math.min(attempt, 3), 2000);
        if (body === undefined || !retryable || performance.now() + delay >= deadline)
          throw error;
        await new Promise(resolve => setTimeout(resolve, delay));
      }
    }
  }
  // SDK retries reuse the same entries array. Retain the exact operation and body,
  // including UUID-less entries, until the batch has been acknowledged.
  const batches = new WeakMap();
  const store = {
    async append(key, entries) {
      if (key.sessionId !== sessionId)
        throw new Error("Unexpected native Claude session identity");
      let batch = batches.get(entries);
      if (!batch) {
        const operationId = randomUUID();
        batch = {
          path: `/entries?subpath=${encodeURIComponent(key.subpath ?? "")}&operationId=${operationId}`,
          body: entries.map((e) => JSON.stringify(e)).join("\n") + "\n",
        };
        batches.set(entries, batch);
      }
      const response = await request(batch.path, batch.body, { raw: true });
      await response.body?.cancel();
    },
    async load(key) {
      if (key.sessionId !== sessionId)
        throw new Error("Unexpected native Claude session identity");
      const entries = await loadTranscript({ cache, endpoint, sessionId, subpath: key.subpath, request });
      // Never let managed resume silently fall back to a local transcript.
      if (!entries.length && !key.subpath)
        throw new Error(
          "No acknowledged Claude transcript is available to resume",
        );
      return entries.length ? entries : null;
    },
    async listSubkeys() {
      return (await (await request("/subkeys")).json()).subkeys;
    },
  };
  const batched = eventBatchV1 === true;
  const queue = batched ? eventQueue(request) : null;
  async function event(
    payload,
    eventId = payload.type === "mira_question" ? payload.questionId : randomUUID(),
  ) {
    if (queue) return queue.push({ eventId, payload });
    // Retrying this exact storage envelope cannot replay model/tool execution.
    const response = await request("/events", { eventId, payload });
    await response.body?.cancel();
  }
  // SDK events: streaming deltas and token counters are relayed live without
  // being stored when Server supports batches; everything else is durable.
  function emit(payload) {
    if (!queue) return event(payload);
    if (ephemeral(payload)) return queue.push({ payload, ephemeral: true });
    return event(payload);
  }
  return {
    store, event, emit,
    backlog: () => queue?.bytes() ?? 0,
    failure: () => queue?.failure() ?? null,
    close: () => queue?.close(),
  };
}

export function ephemeral(payload) {
  return payload?.type === "stream_event" ||
    payload?.type === "system" && payload.subtype === "thinking_tokens";
}

// eventQueue sends events in order, many per request. Each batch is frozen once
// and retried byte-for-byte; durable items carry stable event IDs, so a lost
// response cannot duplicate them. A failed batch rejects only its own items and
// is remembered so the SDK loop stops, as with the former per-event writes.
// An idle queue sends an empty batch every few seconds as a liveness heartbeat.
export function eventQueue(request, { maxBatchBytes = 2 << 20, heartbeatMs = 5000 } = {}) {
  const pending = [];
  let bytes = 0, running = null, failed = null, closed = false, last = Date.now();
  const heartbeat = setInterval(() => {
    if (!closed && !running && !pending.length && Date.now() - last >= heartbeatMs) drain([]).catch(() => {});
  }, Math.min(heartbeatMs, 1000));
  heartbeat.unref?.();
  async function drain(batch) {
    last = Date.now();
    const response = await request("/events", { items: batch.map(entry => entry.item) });
    await response.body?.cancel();
  }
  async function run() {
    while (pending.length) {
      const batch = [];
      let size = 0;
      while (pending.length && (!batch.length || size + pending[0].size <= maxBatchBytes)) {
        const entry = pending.shift();
        size += entry.size;
        batch.push(entry);
      }
      bytes -= size;
      try {
        await drain(batch);
        for (const entry of batch) entry.resolve();
      } catch (error) {
        failed ??= error;
        for (const entry of batch) entry.reject(error);
      }
    }
    running = null;
  }
  return {
    push(item) {
      const size = JSON.stringify(item.payload).length + 64;
      const done = new Promise((resolve, reject) => {
        pending.push({ item, size, resolve, reject });
      });
      bytes += size;
      // Callers that do not await (live SDK events) must not surface unhandled rejections.
      done.catch(() => {});
      running ??= Promise.resolve().then(run);
      return done;
    },
    bytes: () => bytes,
    failure: () => failed,
    close() { closed = true; clearInterval(heartbeat); },
  };
}

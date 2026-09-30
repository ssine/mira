import { randomUUID } from "node:crypto";
import { loadTranscript } from "./session-cache.mjs";

export function serverClient({
  endpoint,
  credential,
  sessionId,
  turnId,
  revision,
  cache,
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
  async function event(
    payload,
    eventId = payload.type === "mira_question" ? payload.questionId : randomUUID(),
  ) {
    // Retrying this exact storage envelope cannot replay model/tool execution.
    const response = await request("/events", { eventId, payload });
    await response.body?.cancel();
  }
  return { store, event };
}

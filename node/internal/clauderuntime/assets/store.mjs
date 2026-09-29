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
    const response = await fetch(new URL(root + path, base), {
      method: body === undefined ? "GET" : "POST",
      redirect: "error",
      headers: {
        authorization: `Bearer ${credential}`,
        "content-type": raw ? "application/x-ndjson" : "application/json",
        "x-mira-claude-turn": turnId,
        "x-mira-claude-revision": String(revision),
      },
      body: body === undefined ? undefined : raw ? body : JSON.stringify(body),
      signal: AbortSignal.timeout(45_000),
    });
    if (!response.ok)
      throw new Error(`Mira Claude storage failed (HTTP ${response.status})`);
    return response;
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
      await request(batch.path, batch.body, { raw: true });
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
  async function event(payload) {
    const body = {
      eventId:
        payload.type === "mira_question" ? payload.questionId : randomUUID(),
      payload,
    };
    // Retrying this exact storage envelope cannot replay model/tool execution.
    for (let attempt = 0; ; attempt++) {
      try {
        await request("/events", body);
        return;
      } catch (error) {
        if (attempt >= 2) throw error;
        await new Promise((r) => setTimeout(r, 200 * (attempt + 1)));
      }
    }
  }
  return { store, event };
}

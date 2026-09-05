import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { putSnapshot, getStoreHead } from "../server/thread-store.mjs";

const databaseUrl = process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL;
if (!databaseUrl) throw new Error("a disposable Server database is required");
const pool = new pg.Pool({ connectionString: databaseUrl });
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const storeId = `read-api-${crypto.randomUUID()}`, threadId = crypto.randomUUID();
const path = `/v1/codex/threads/${threadId}`;
try {
  assert.equal((await putSnapshot(pool, storeId, { expectedVersion: 0, snapshot: {
    histories: { [threadId]: [{ type: "event_msg", payload: { type: "agent_message", message: "New result" } }] },
  } }, { "x-codex-operation-id": crypto.randomUUID() })).status, 200);
  const position = JSON.stringify({ generation: 1, itemCount: 1 });
  const request = (suffix, options = {}) => fetch(`${origin}${path}${suffix}?storeId=${storeId}`, options);
  assert.equal((await request("/read", { method: "POST", body: position })).status, 401);
  const login = await fetch(`${origin}/v1/admin/login`, { method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ username: "admin", password: process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password" }) });
  assert.equal(login.status, 200);
  const session = await login.json();
  const cookie = login.headers.getSetCookie().map(value => value.split(";")[0]).join("; ");
  assert.equal((await request("/read", { method: "POST", headers: { cookie }, body: position })).status, 403);
  const headers = { cookie, "content-type": "application/json", "x-mira-csrf": session.csrfToken };
  assert.equal((await (await request("", { headers })).json()).readState.unread, true);
  const before = await getStoreHead(pool, storeId);
  assert.equal((await request("/read", { method: "POST", headers, body: position })).status, 200);
  assert.equal((await (await request("", { headers })).json()).readState.unread, false);
  assert.equal((await request("/read", { method: "POST", headers, body: JSON.stringify({ generation: 1, itemCount: 0 }) })).status, 200);
  const list = await (await fetch(`${origin}/v1/codex/threads?storeId=${storeId}`, { headers })).json();
  assert.equal(list.data[0].readState.readItemCount, 1);
  assert.equal((await request("/read", { method: "POST", headers, body: JSON.stringify({ generation: 2, itemCount: 1 }) })).status, 409);
  assert.equal((await request("/read", { method: "POST", headers, body: JSON.stringify({ generation: 1, itemCount: -1 }) })).status, 400);
  assert.deepEqual(await getStoreHead(pool, storeId), before);
  console.log("PASS: authenticated/CSRF-protected read acknowledgments, shared list/detail state, monotonic positions and unchanged canonical history");
} finally { await pool.end(); }

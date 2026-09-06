import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { accountHistory, sampleAccount, sampleIntervalMs, runtimeKey, startAccountSampler } from "../server/account-history.mjs";
import { getNode } from "../server/node-registry.mjs";

const databaseUrl = process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL;
if (!databaseUrl) throw new Error("a disposable Server database is required");
const pool = new pg.Pool({ connectionString: databaseUrl });
const nodeId = crypto.randomUUID(), origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
let stop;
try {
  await pool.query(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,channel_status,reported_app_server)
    VALUES($1::uuid,$1::text,'quota-test','linux','amd64','linux','test','{"appServer":true}','[]','approved','{"connected":true}','{"status":"running","codexHome":"/test"}')`, [nodeId]);
  const node = await getNode(pool, nodeId);
  let reads = 0, email = "first@example.test", used = 10, fail = false, gate;
  const channel = { isConnected: id => id === nodeId, accountReader: { read: async () => {
    reads++; if (gate) await gate;
    if (fail) throw new Error("sensitive upstream error must not be stored");
    return { account: { type: "chatgpt", email, planType: "pro", token: "never-persist" }, limits: {
      rateLimits: { secondary: { windowDurationMins: 10080, usedPercent: used, resetsAt: 2_000_000_000 } }, rateLimitResetCredits: { availableCount: 0 },
    } };
  } } };
  let now = Date.now() - sampleIntervalMs * 8;
  const sample = () => sampleAccount(pool, channel, node, { now: () => now });
  assert.equal(await sampleAccount(pool, channel, { ...node, status: "offline" }), false);
  assert.equal(reads, 0);
  const results = await Promise.all([sample(), sample(), sample()]);
  assert.equal(results.filter(Boolean).length, 1); assert.equal(reads, 1);
  assert.equal(await sample(), false, "persisted TTL also applies to a restarted worker");
  now += sampleIntervalMs; used = 100; await sample();
  now += sampleIntervalMs; fail = true; await sample(); fail = false;
  now += sampleIntervalMs; used = 0; await sample();
  let history = await accountHistory(pool, node, "24h", now + 1);
  assert.deepEqual(history.points.map(point => point.remaining), [90, 0, null, 100]);
  assert.equal(history.points[1].resetCount, 0);
  now += sampleIntervalMs; email = "second@example.test"; used = 50; await sample();
  history = await accountHistory(pool, node, "7d", now + 1);
  assert.equal(history.account.email, email);
  assert.deepEqual(history.points.map(point => point.remaining), [null, null, null, null, 50]);
  assert.equal(JSON.stringify(history).includes("first@example.test"), false);
  assert.equal(JSON.stringify(history).includes("never-persist"), false);
  assert.equal((await accountHistory(pool, { ...node, reportedAppServer: { codexHome: "/other" } })).points.length, 0);
  await assert.rejects(accountHistory(pool, node, "all"), { statusCode: 400 });

  const endpoint = `${origin}/v1/nodes/${nodeId}/account-history`;
  assert.equal((await fetch(endpoint)).status, 401);
  const login = await fetch(`${origin}/v1/admin/login`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ username: "admin", password: process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password" }) });
  assert.equal(login.status, 200);
  const headers = { cookie: login.headers.getSetCookie().map(value => value.split(";")[0]).join("; ") };
  let response = await fetch(`${endpoint}?range=7d`, { headers });
  assert.equal(response.status, 200); assert.equal(response.headers.get("cache-control"), "no-store");
  assert.equal((await response.json()).account.email, email);
  assert.equal((await fetch(`${endpoint}?range=constructor`, { headers })).status, 400);
  assert.equal((await fetch(`${origin}/v1/nodes/${crypto.randomUUID()}/account-history`, { headers })).status, 404);
  assert.equal((await fetch(endpoint, { headers, method: "POST" })).status, 404, "there is no browser write endpoint");

  // Prove that the timer samples without a browser request and stops cleanly.
  const before = reads;
  stop = startAccountSampler(pool, channel, { tickMs: 25 });
  for (let i = 0; reads === before && i < 150; i++) await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(reads, before + 1);
  await stop(); stop = null;
  const stored = await pool.query("SELECT * FROM mira_account_quota_samples WHERE node_id=$1", [nodeId]);
  assert.ok(stored.rowCount > 5);
  assert.equal(JSON.stringify(stored.rows).includes("sensitive"), false);
  assert.equal(JSON.stringify(stored.rows).includes("never-persist"), false);

  // Replacing a runtime in flight discards the result under the old scope.
  now = Date.now() + sampleIntervalMs;
  let release; gate = new Promise(resolve => { release = resolve; });
  const beforeChange = reads, pending = sample();
  for (let i = 0; reads === beforeChange && i < 100; i++) await new Promise(resolve => setTimeout(resolve, 10));
  await pool.query(`UPDATE codex_nodes SET reported_app_server='{"status":"running","codexHome":"/replacement"}' WHERE node_id=$1`, [nodeId]);
  release(); assert.equal(await pending, false);
  assert.equal((await pool.query("SELECT count(*)::int AS count FROM mira_account_quota_samples WHERE node_id=$1 AND runtime_key=$2", [nodeId, runtimeKey(node)])).rows[0].count, stored.rowCount);
  console.log("PASS: durable quota sampling, concurrent/restarted worker deduplication, identity/runtime isolation, unknown/zero/gaps, timer lifecycle and authenticated history API");
} finally {
  if (stop) await stop();
  await pool.query("DELETE FROM mira_account_quota_samples WHERE node_id=$1", [nodeId]);
  await pool.query("DELETE FROM codex_nodes WHERE node_id=$1", [nodeId]);
  await pool.end();
}

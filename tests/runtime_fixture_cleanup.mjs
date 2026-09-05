import assert from "node:assert/strict";
import { setTimeout as delay } from "node:timers/promises";

// pg-pool resolves end() when clients leave the pool, before their socket/end
// callbacks necessarily finish. DROP ... FORCE in that gap can emit an idle
// client's fatal error after every test assertion has passed. Observe PostgreSQL
// disconnects instead; never force away a connection or suppress its errors.
export async function closeRuntimeFixtureDatabase(pool, admin, database, {
  timeoutMs = 30_000,
  pollIntervalMs = 25,
} = {}) {
  assert.match(database, /^mira_retry_[0-9]+_[a-f0-9]{8}$/, "only an owned runtime fixture database may be removed");
  await pool?.end();
  const deadline = performance.now() + timeoutMs;
  while (true) {
    const connections = await admin.query(
      "SELECT pid FROM pg_stat_activity WHERE datname = $1",
      [database],
    );
    if (connections.rowCount === 0) break;
    if (performance.now() >= deadline) throw new Error("runtime fixture database connections did not drain");
    await delay(pollIntervalMs);
  }
  await admin.query(`DROP DATABASE ${database}`);
}

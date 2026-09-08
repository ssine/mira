import assert from "node:assert/strict";
import test from "node:test";
import { closeRuntimeFixtureDatabase } from "./runtime_fixture_cleanup.mjs";

const database = "mira_retry_123_1234abcd";

test("waits for server-side disconnects after pool.end resolves before dropping the fixture", async () => {
  const calls = [];
  const remaining = [2, 1, 0];
  const pool = { end: async () => calls.push("end") };
  const admin = { query: async (sql, params) => {
    calls.push(sql);
    if (sql.startsWith("SELECT")) {
      assert.deepEqual(params, [database]);
      return { rowCount: remaining.shift() };
    }
    assert.equal(remaining.length, 0, "must not drop before the last backend disconnects");
    assert.equal(sql, `DROP DATABASE ${database}`, "never force connections closed");
  } };
  await closeRuntimeFixtureDatabase(pool, admin, database, { pollIntervalMs: 1 });
  assert.equal(calls[0], "end");
  assert.equal(calls.filter((sql) => sql.startsWith("SELECT")).length, 3);
  assert.equal(calls.at(-1), `DROP DATABASE ${database}`);
});

test("a leaked connection fails cleanup without forcibly deleting the database", async () => {
  const admin = { query: async (sql) => {
    assert(sql.startsWith("SELECT"), "a busy fixture must not be dropped");
    return { rowCount: 1 };
  } };
  await assert.rejects(closeRuntimeFixtureDatabase({ end: async () => {} }, admin, database, {
    timeoutMs: 0,
  }), /connections did not drain/);
});

test("propagates database errors instead of swallowing them to make CI green", async () => {
  const failure = new Error("fixture connection failed");
  await assert.rejects(closeRuntimeFixtureDatabase({ end: async () => {} }, {
    query: async () => { throw failure; },
  }, database), (error) => error === failure);
});

test("rejects unrelated database names before touching any pool", async () => {
  for (const name of ["mira", "postgres", "mira_retry_123", "mira_retry_123_1234abcd; DROP DATABASE mira"]) {
    await assert.rejects(closeRuntimeFixtureDatabase({ end: () => assert.fail("must not close") }, {
      query: () => assert.fail("must not query"),
    }, name), /only an owned runtime fixture database/);
  }
});

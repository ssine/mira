import assert from "node:assert/strict";
import test from "node:test";
import { accountGroups } from "../server/public/codex-accounts.js";
import { spendingSeries } from "../server/public/account-spend.js";
import { AccountSidebar } from "../server/public/account-status.js";

test("account names combine every Node and keep the freshest valid balance, including zero", () => {
  const account = (id, name, remaining, observedAt) => ({ nodeAccountId: id, name, observedAt,
    snapshot: { limits: remaining === null ? {} : { rateLimits: { primary: { windowDurationMins: 10080, usedPercent: 100 - remaining } } } } });
  const groups = accountGroups([
    { nodeId: "one", status: "online", codexAccounts: [account("a", "Shared", 30, "2026-09-12T01:00:00Z"), account("b", "API", null)] },
    { nodeId: "two", status: "offline", codexAccounts: [account("c", " Shared ", 0, "2026-09-12T02:00:00Z")] },
    { nodeId: "three", status: "online", codexAccounts: [account("d", "Shared", null, "2026-09-12T03:00:00Z")] },
    { nodeId: "revoked", approvalStatus: "revoked", codexAccounts: [account("e", "Hidden", 100)] },
  ]);
  assert.deepEqual(groups.map(group => group.name), ["API", "Shared"]);
  assert.equal(groups[1].members.length, 3);
  assert.equal(groups[1].members[0].account.nodeAccountId, "c");
});

test("spending curves sum hourly costs inside each calendar day and reset at midnight", () => {
  const result = spendingSeries({ to: 380, days: [{ date: "day1", at: 0, end: 200, amount: 5 }, { date: "day2", at: 200, end: 400, amount: 4 }],
    points: [{ date: "day2", at: 300, amount: 4 }, { date: "day1", at: 150, amount: 3 }, { date: "day1", at: 100, amount: 2 }, { date: "day1", at: 160, amount: null }] });
  assert.deepEqual(result[0].points.map(point => [point.at, point.remaining]), [[0, 0], [100, 2], [150, 5], [200, 5]]);
  assert.deepEqual(result[1].points.map(point => [point.at, point.remaining]), [[200, 0], [300, 4], [380, 4]]);
});

test("empty default placeholders stay hidden while configured offline accounts remain", () => {
  const placeholder = { name: "默认账号", nodeAccountId: "empty", isDefault: true, authType: "chatgpt", provider: "openai", credentialRevision: 1 };
  const groups = accountGroups([{ nodeId: "offline", status: "offline", codexAccounts: [placeholder,
    { ...placeholder, nodeAccountId: "logged-in", snapshot: { account: { type: "chatgpt" } } },
    { ...placeholder, nodeAccountId: "configured", credentialRevision: 2 },
    { ...placeholder, nodeAccountId: "api", authType: "apiKey" },
    { ...placeholder, nodeAccountId: "custom", name: "Named account" },
  ] }]);
  assert.deepEqual(groups.find(group => group.name === "默认账号").members.map(member => member.account.nodeAccountId).sort(), ["api", "configured", "logged-in"]);
  assert.ok(groups.some(group => group.name === "Named account"));
  assert.deepEqual(accountGroups([{ codexAccounts: [placeholder] }]), []);
});

test("seven-day summaries bound concurrent reads, reuse totals and discard replies after logout", async () => {
  const originalFetch = globalThis.fetch, pending = [];
  globalThis.fetch = (url, { signal }) => new Promise(resolve => pending.push({ url, signal, resolve }));
  const sidebar = Object.assign(Object.create(AccountSidebar.prototype), {
    active: true, intervalMs: 300_000, summaryJobs: new Map(), summaries: new Map(),
    groups: ["A", "B", "C"].map(name => ({ name, members: [{ account: {} }] })),
    spend: { urlFor: (name, range) => `${name}:${range}`, cacheSummary() {} },
    renderOverview() { this.loadSummaries(); },
  });
  const settle = () => new Promise(resolve => setImmediate(resolve));
  try {
    sidebar.loadSummaries(); assert.equal(pending.length, 2);
    pending[0].resolve({ ok: true, json: async () => ({ estimate: { amount: 0, status: "complete" } }) });
    await settle(); assert.equal(pending.length, 3);
    assert.equal(sidebar.summaries.get("A").data.estimate.amount, 0);
    sidebar.loadSummaries(); assert.equal(pending.length, 3, "completed summaries use the cache");
    assert.ok(pending.every(request => request.url.endsWith(":7d")));
    sidebar.active = false; sidebar.stopSummaries(); sidebar.summaries.clear();
    for (const request of pending.slice(1)) {
      assert.equal(request.signal.aborted, true);
      request.resolve({ ok: true, json: async () => ({ estimate: { amount: 123 } }) });
    }
    await settle(); assert.equal(sidebar.summaries.size, 0, "stale account amounts cannot repopulate after logout");
  } finally { sidebar.active = false; sidebar.stopSummaries(); globalThis.fetch = originalFetch; }
});

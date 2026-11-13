import assert from "node:assert/strict";
import test from "node:test";
import { accountGroups, recentAccountKeys } from "../server/public/codex-accounts.js";
import { spendingSeries, spendProjectionStatus, spendCacheLifetime } from "../server/public/account-spend.js";
import { AccountSidebar } from "../server/public/account-status.js";

test("backfill is visible and follows the server's short refresh deadline", () => {
  const data = { projection: { status: "updating", processedItems: 25, totalItems: 100 },
    cache: { expiresAt: new Date(Date.now() + 5000).toISOString() } };
  assert.equal(spendProjectionStatus(data), "正在汇总用量 25%");
  assert.ok(spendCacheLifetime(data) <= 5000);
  data.projection.status = "retrying";
  assert.match(spendProjectionStatus(data), /失败.*重试/);
  data.projection.status = "ready";
  assert.equal(spendProjectionStatus(data), "");
});

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

test("the fold keeps the distinct accounts of the two most recent top-level conversations", () => {
  const node = { nodeId: "n", status: "online", codexAccounts: [{ nodeAccountId: "a", name: "Alpha", authType: "apiKey" }, { nodeAccountId: "b", name: "Beta", authType: "apiKey" }],
    claudeAccounts: [{ engine: "claude", nodeAccountId: "c", name: "Alpha" }, { engine: "claude", nodeAccountId: "d", name: "Delta" }] };
  const groups = accountGroups([node]);
  const at = minutes => new Date(Date.UTC(2026, 8, 30, 0, minutes)).toISOString();
  const threads = [
    { threadId: "old", nodeAccountId: "b", updatedAt: at(1) },
    { threadId: "claude", engine: "claude", nodeAccountId: "c", updatedAt: at(5) },
    { threadId: "child", nodeAccountId: "b", parentThreadId: "claude", updatedAt: at(9) },
    { threadId: "same", engine: "claude", nodeAccountId: "c", updatedAt: at(4) },
    { threadId: "unbound", updatedAt: at(8) },
    { threadId: "codex", nodeAccountId: "a", updatedAt: at(3) },
  ];
  assert.deepEqual(recentAccountKeys(groups, threads), [JSON.stringify(["claude", "Alpha"]), JSON.stringify(["codex", "Alpha"])]);
  const sidebar = Object.assign(Object.create(AccountSidebar.prototype), { groups, threads, expanded: false });
  let view = sidebar.visibleGroups();
  assert.deepEqual(view.visible.map(group => group.key), groups.map(group => group.key).filter(key => recentAccountKeys(groups, threads).includes(key)),
    "pinned rows keep the account name order");
  assert.equal(view.folded, 2);
  sidebar.threads = [];
  view = sidebar.visibleGroups();
  assert.equal(view.visible.length, 2, "without history the first two accounts stay visible");
  sidebar.expanded = true;
  assert.equal(sidebar.visibleGroups().visible.length, 4);
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
    active: false, summariesActive: true, intervalMs: 300_000, summaryJobs: new Map(), summaries: new Map(), threads: [], expanded: true,
    groups: ["A", "B", "C"].map(name => ({ name, key: name, members: [{ account: {} }] })),
    spend: { urlFor: (name, range) => `${name}:${range}`, cacheSummary() {} },
    renderOverview() { this.loadSummaries(); },
  });
  const settle = () => new Promise(resolve => setImmediate(resolve));
  try {
    sidebar.loadSummaries(); assert.equal(pending.length, 2, "mobile summaries load while the drawer and live account subscription are inactive");
    pending[0].resolve({ ok: true, json: async () => ({ estimate: { amount: 0, status: "complete" } }) });
    await settle(); assert.equal(pending.length, 3);
    assert.equal(sidebar.summaries.get("A").data.estimate.amount, 0);
    sidebar.loadSummaries(); assert.equal(pending.length, 3, "completed summaries use the cache");
    assert.ok(pending.every(request => request.url.endsWith(":7d")));
    sidebar.summariesActive = false; sidebar.stopSummaries(); sidebar.summaries.clear();
    for (const request of pending.slice(1)) {
      assert.equal(request.signal.aborted, true);
      request.resolve({ ok: true, json: async () => ({ estimate: { amount: 123 } }) });
    }
    await settle(); assert.equal(sidebar.summaries.size, 0, "stale account amounts cannot repopulate after logout");
    sidebar.loadSummaries(); assert.equal(pending.length, 3, "hidden or signed-out pages do not start further reads");
  } finally { sidebar.summariesActive = false; sidebar.stopSummaries(); globalThis.fetch = originalFetch; }
});

test("Claude subscription windows read like Codex quota and API accounts keep cost estimates", async () => {
  const { claudePlanQuota } = await import("../server/public/account-quota.js");
  const quota = claudePlanQuota({ fiveHour: { utilization: 12.5, resetsAt: 1_800_000_000_000 }, sevenDay: { utilization: 104, resetsAt: null },
    sevenDayOpus: null, models: [{ name: "Fable", utilization: 0, resetsAt: 1_800_000_000_000 }, { name: "Empty", utilization: null, resetsAt: null }] });
  assert.deepEqual(quota.windows.map(window => [window.label, window.remaining]), [["5 小时", 87.5], ["本周 · 全部模型", 0], ["本周 · Fable", 100]]);
  assert.equal(quota.remaining, 0, "over-limit weekly usage clamps to zero instead of disappearing");
  assert.equal(quota.session.remaining, 87.5);
  assert.deepEqual(claudePlanQuota(null).windows, []);

  const originalFetch = globalThis.fetch, pending = [];
  globalThis.fetch = (url, { signal }) => new Promise(resolve => pending.push({ url, signal, resolve }));
  const member = (id, authType = "subscription") => ({ node: { status: "online" }, account: { nodeAccountId: id, authType, configured: true, enabled: true } });
  const sidebar = Object.assign(Object.create(AccountSidebar.prototype), {
    summariesActive: true, intervalMs: 300_000, planJobs: new Map(), planUsage: new Map(), summaryJobs: new Map(), threads: [], expanded: true,
    groups: [{ key: "a", engine: "claude", members: [member("a")] }, { key: "b", engine: "claude", members: [member("b")] },
      { key: "c", engine: "claude", members: [member("c")] }, { key: "api", engine: "claude", members: [member("api", "api_key")] }],
    render() {}, renderOverview() {},
  });
  const settle = () => new Promise(resolve => setImmediate(resolve));
  try {
    sidebar.loadPlanUsages();
    assert.deepEqual(pending.map(request => request.url), ["/v1/claude/accounts/a/usage", "/v1/claude/accounts/b/usage"], "plan reads are bounded and skip API-key accounts");
    pending[0].resolve({ ok: true, json: async () => ({ usage: { available: true, fiveHour: { utilization: 40 }, sevenDay: { utilization: 10 } }, observedAt: Date.now() }) });
    pending[1].resolve({ ok: false, json: async () => ({ error: "Claude account Node is offline" }) });
    await settle(); await settle();
    assert.equal(sidebar.groupQuota(sidebar.groups[0]).remaining, 90);
    assert.equal(sidebar.planUsage.get("b").message, "Claude account Node is offline");
    assert.equal(sidebar.groupQuota(sidebar.groups[3]).remaining, null, "API-key accounts have no plan quota");
    sidebar.loadPlanUsages();
    assert.deepEqual(pending.slice(2).map(request => request.url), ["/v1/claude/accounts/c/usage"], "fresh and recently failed reads are reused");
    sidebar.summariesActive = false; sidebar.stopSummaries();
    assert.equal(pending[2].signal.aborted, true);
    pending[2].resolve({ ok: true, json: async () => ({ usage: { available: true } }) });
    await settle(); await settle();
    assert.equal(sidebar.planUsage.has("c"), false, "aborted replies cannot repopulate after logout");
  } finally { sidebar.summariesActive = false; sidebar.stopSummaries(); globalThis.fetch = originalFetch; }
});

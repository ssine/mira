export function accountOverviewFixture() {
  const ids = Array.from({ length: 8 }, (_, i) => `10000000-0000-4000-8000-${String(i + 1).padStart(12, "0")}`);
  const quota = remaining => ({ rateLimits: { secondary: { usedPercent: 100 - remaining, windowDurationMins: 10080, resetsAt: Math.floor(Date.now() / 1000) + 86400 } } });
  const account = (id, nodeId, name, remaining, time) => ({ nodeAccountId: id, accountId: id, nodeId, name, enabled: true, revision: 1,
    credentialRevision: 1, isDefault: name === "Shared", desiredAppServer: {}, reportedAppServer: { status: "stopped" }, observedAt: time,
    snapshot: remaining === null ? null : { account: { type: "chatgpt", email: "shared@example.test", planType: "pro" }, limits: quota(remaining) } });
  const nodes = [0, 1].map(i => ({ nodeId: ids[i], hostname: `Node ${i + 1}`, status: i ? "offline" : "online", platform: "linux", nodeMode: "linux",
    approvalStatus: "approved", capabilities: { appServer: true, codexAccountsV1: true }, desiredAppServer: {}, reportedAppServer: { status: "stopped" } }));
  nodes[0].codexAccounts = [account(ids[2], ids[0], "Shared", 30, "2026-09-12T01:00:00Z"), account(ids[3], ids[0], "API", null)];
  nodes[1].codexAccounts = [account(ids[4], ids[1], "Shared", 0, "2026-09-12T02:00:00Z"), account(ids[5], ids[1], "Offline API", null)];
  const threads = [6, 7].map((i, index) => ({ threadId: ids[i], title: index ? "Running conversation" : "Account overview", cwd: "/work", model: "gpt-6-astra",
    runtimeNodeId: ids[0], nodeAccountId: ids[2], generation: 1, itemCount: 1, updatedAt: new Date().toISOString(),
    activity: { state: index ? "running" : "idle", turnId: `turn-${i}`, generation: 1, itemCount: 1 } }));
  const history = (range = "7d") => {
    const to = Date.now(), today = new Date(); today.setHours(0, 0, 0, 0);
    const count = { "24h": 1, "7d": 7, "30d": 30 }[range];
    const days = Array.from({ length: count }, (_, i) => {
      const start = new Date(today); start.setDate(start.getDate() - count + 1 + i);
      const end = new Date(start); end.setDate(end.getDate() + 1);
      const date = `${start.getFullYear()}-${String(start.getMonth()+1).padStart(2,"0")}-${String(start.getDate()).padStart(2,"0")}`;
      return { date, at: +start, end: +end, amount: i % 3 + 1, status: i === count - 1 ? "partial" : "complete" };
    });
    return { from: days[0].at, to, days, estimate: { amount: days.reduce((sum, day) => sum + day.amount, 0), status: "partial" }, points: days.flatMap(day => [1, 2].map(i => ({ date: day.date, at: day.at + (Math.min(day.end, to) - day.at) * i / 3, amount: day.amount / 2 }))) };
  };
  return { nodes, threads, history, quota };
}

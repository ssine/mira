import assert from "node:assert/strict";
import fs from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await fs.readFile(new URL("../server/public/app.js", import.meta.url), "utf8");
const start = source.indexOf("async function startAgentRuntime()");
const end = source.indexOf("async function stopAgentRuntime()", start);
assert(start > 0 && end > start);

function fixture(states, { managed = true, stopAt = Infinity } = {}) {
  let now = 0, requests = 0, connected = false;
  const messages = [];
  const agent = { runtimeStartEpoch: 0 };
  const node = { nodeId: "fixture", capabilities: { codexRuntimeDownload: managed } };
  const context = vm.createContext({
    agent, dashboardNodes: new Map([["fixture", node]]),
    $: () => ({ value: "fixture" }),
    Date: { now: () => now },
    setTimeout(resolve, delay) { now += delay; if (now >= stopAt) agent.runtimeStartEpoch++; resolve(); },
    closeAgentSocket() { agent.runtimeStartEpoch++; },
    setAgentRuntimeState: (message) => messages.push(message),
    async api(url) {
      if (url.endsWith("/start")) return {};
      requests++;
      return { ...node, reportedAppServer: states(now) };
    },
    async connectAgentSocket() { connected = true; },
  });
  vm.runInContext(source.slice(start, end), context);
  return { run: () => context.startAgentRuntime(), messages, connected: () => connected, requests: () => requests };
}

test("first Codex download can exceed 30 seconds and preparation is not an error", async () => {
  const subject = fixture((now) => now < 60_000
    ? { status: "starting", runtimePreparing: true, lastError: "preparing Codex runtime" }
    : { status: "running" });
  await subject.run();
  assert(subject.connected());
  assert(subject.messages.some((message) => message.includes("首次准备 Codex")));
  assert(subject.requests() < 40, "preparation polling should be slower");
});

test("failed downloads keep their actionable error instead of waiting 21 minutes", async () => {
  const subject = fixture(() => ({ status: "stopped", lastError: "runtime checksum mismatch" }));
  await assert.rejects(subject.run(), /runtime checksum mismatch/);
  assert(!subject.connected());
});

test("stopping while preparing prevents a late connection", async () => {
  const subject = fixture(() => ({ status: "starting", runtimePreparing: true }), { stopAt: 2000 });
  await assert.rejects(subject.run(), /已取消等待/);
  assert(!subject.connected());
});

test("older nodes retain bounded normal startup timeout", async () => {
  const subject = fixture(() => ({ status: "starting" }), { managed: false });
  await assert.rejects(subject.run(), /启动超时/);
  assert.equal(subject.requests(), 60);
});

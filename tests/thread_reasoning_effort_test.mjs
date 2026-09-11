import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

const app = fs.readFileSync(new URL("../server/public/app.js", import.meta.url), "utf8");
function source(name) {
  const start = app.search(new RegExp(`(?:async )?function ${name}\\(`));
  assert.ok(start >= 0);
  const rest = app.slice(start);
  const next = rest.slice(1).search(/\n(?:async )?function /);
  return next < 0 ? rest : rest.slice(0, next + 1);
}
function fixture({ effort = "xhigh", cached = true, loaded = true } = {}) {
  const row = { threadId: "thread-a", cwd: "/work", model: "model", reasoningEffort: effort, runtimeNodeId: "node" };
  const controls = new Map();
  const calls = [];
  const agent = {
    threads: cached ? [row] : [], loadedThreadIds: new Set(loaded ? ["thread-a"] : []),
    threadId: null, socket: { readyState: 1 }, socketNodeId: "node", liveRevision: 0,
    selectionEpoch: 0, projectOpen: new Map(), modelCatalogKey: "catalog",
    activeTurns: new Map(), turnTimings: new Map(), untitledNewThreadIds: new Set(),
    modelCatalog: { defaultModel: "model", configuredReasoningEffort: "medium", models: [
      { model: "model", defaultReasoningEffort: "medium", supportedReasoningEfforts: [
        { reasoningEffort: "medium" }, { reasoningEffort: "xhigh" }, { reasoningEffort: "ultra" },
      ] },
    ] },
  };
  const context = vm.createContext({
    agent, console, clearTimeout, Date, WebSocket: { OPEN: 1 },
    $: selector => {
      if (!controls.has(selector)) controls.set(selector, {
        value: "", textContent: "", options: [{ value: "node" }],
        classList: { add() {} }, append() {},
      });
      return controls.get(selector);
    },
    api: async () => structuredClone(row),
    rpc: async (method, params) => {
      calls.push({ method, params });
      if (method === "thread/resume") return {
        thread: { id: "thread-a", status: { type: "idle" } },
        model: "model", cwd: "/work", reasoningEffort: "medium",
      };
      if (method === "turn/start") return { turn: { id: "turn", status: "completed" } };
      throw Error(method);
    },
    currentAgentThread: () => agent.threads.find(row => row.threadId === agent.threadId),
    selectedConversationModel: () => "model", conversationModelKey: () => "catalog",
    projectForThread: () => ({ key: "project" }),
    clear: value => value, element: () => ({}),
    upsertTrace: () => ({ dataset: {} }), prepareTurnInput: async () => ({ message: "Continue", inputs: [] }),
    replyProgress: { finish() {} },
    accountRecovery: { select() {} },
  });
  for (const name of [
    "writeBrowserRoute", "selectComposerDraft", "resetAgentTranscript", "setConversationTitle",
    "renderTurnDiagnostics", "acceptThreadActivity", "setConversationMeta", "loadConversationModels",
    "syncActiveTurnUi", "renderAgentThreads", "setConversationNotice", "loadAgentTranscript",
    "startAgentRuntime", "scheduleAgentHeartbeat", "updateReplyProgress", "renderReplyProgress",
    "refreshAccountChoices",
  ]) context[name] = () => {};
  for (const name of [
    "modelCatalogForConversation", "conversationModelDefinition", "conversationEffortOptions",
    "selectedConversationEffort", "resumeAgentThread", "restoreAgentThread", "sendAgentMessage",
  ]) vm.runInContext(source(name), context);
  context.resumeAgentThreadOnSocket = id => context.restoreAgentThread(id, agent.socket);
  return { context, agent, row, calls };
}

test("switching away and back restores canonical effort and submits it on a loaded thread", async () => {
  const { context, agent, calls } = fixture();
  await context.resumeAgentThread("thread-a");
  assert.equal(context.selectedConversationEffort(), "xhigh");
  agent.threadId = "thread-b";
  agent.effortChoice = "medium";
  await context.resumeAgentThread("thread-a");
  assert.equal(context.selectedConversationEffort(), "xhigh");
  await context.sendAgentMessage("Continue");
  assert.equal(calls.at(-1).params.effort, "xhigh");
  assert.equal(calls.some(call => call.method === "thread/resume"), false);
});

test("a direct reload fetches effort before sending and a fresh runtime default cannot replace it", async () => {
  const { context, agent, calls } = fixture({ cached: false, loaded: false });
  await context.resumeAgentThread("thread-a");
  assert.equal(context.selectedConversationEffort(), "xhigh");
  await context.restoreAgentThread("thread-a", agent.socket);
  assert.equal(calls.at(-1).params.config.model_reasoning_effort, "xhigh");
  assert.equal(context.selectedConversationEffort(), "xhigh");
  await context.sendAgentMessage("Continue");
  assert.equal(calls.at(-1).params.effort, "xhigh");
});

test("explicit next-turn choices win and the accepted choice survives reopening", async () => {
  const { context, agent, calls } = fixture({ loaded: false });
  await context.resumeAgentThread("thread-a");
  agent.effortChoice = "ultra";
  await context.sendAgentMessage("Change effort");
  assert.equal(calls.at(-1).params.effort, "ultra");
  await context.resumeAgentThread("thread-a");
  assert.equal(context.selectedConversationEffort(), "ultra");
});

test("legacy threads without effort retain the configured default", async () => {
  const { context } = fixture({ effort: null });
  await context.resumeAgentThread("thread-a");
  assert.equal(context.selectedConversationEffort(), "medium");
});

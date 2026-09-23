import assert from "node:assert/strict";
import fs from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const app = await fs.readFile(new URL("../server/public/app.js", import.meta.url), "utf8");
function load(context, name) {
  const start = app.search(new RegExp(`^(?:async )?function ${name}\\(`, "m"));
  assert(start >= 0, `missing ${name}`);
  vm.runInContext(app.slice(start, app.indexOf("\n}", start) + 2), context);
}
const failure = (status, code) => Object.assign(new Error("fixture request failed"), { status, code });

function startupFixture({ failAt, error } = {}) {
  const elements = new Map();
  const context = vm.createContext({
    csrfToken: "existing-token", window: { location: { origin: "https://mira.test" } },
    view: null, restored: false,
    $(id) {
      if (!elements.has(id)) {
        const classes = new Set();
        elements.set(id, { textContent: "", disabled: false, classList: {
          add: (...names) => names.forEach(name => classes.add(name)),
          remove: (...names) => names.forEach(name => classes.delete(name)),
          contains: name => classes.has(name),
        } });
      }
      return elements.get(id);
    },
    async api(path) {
      if (path === failAt) throw error;
      return path === "/healthz" ? { version: "1.0.32", adminConfigured: true } : { csrfToken: "renewed-token" };
    },
    async restoreBrowserRoute() {
      if (failAt === "route") throw error;
      context.restored = true; context.view = "agentView";
    },
    show(view) { context.view = view; },
  });
  for (const name of ["isAdminSessionExpired", "showStartupError", "bootstrap"]) load(context, name);
  return context;
}

test("only an explicit expired/missing session asks for login", async () => {
  const page = startupFixture({ failAt: "/v1/admin/session", error: failure(401, "authentication_required") });
  await page.bootstrap();
  assert.equal(page.view, "loginView");
  assert.equal(page.csrfToken, null);
});

for (const [failAt, error] of [
  ["/healthz", new TypeError("Failed to fetch")],
  ["/healthz", failure(503, "unavailable")],
  ["/v1/admin/session", failure(500, "internal_error")],
  ["/v1/admin/session", failure(403, "permission_denied")],
  ["/v1/admin/session", failure(401, "upstream_error")],
  ["route", failure(503, "node_offline")],
  ["route", failure(403, "permission_denied")],
]) test(`${failAt} ${error.status ?? "network"}/${error.code ?? "network"} keeps login state and offers retry`, async () => {
  const page = startupFixture({ failAt, error });
  await page.bootstrap();
  assert.equal(page.view, "connectionView");
  assert(page.csrfToken, "temporary failures must not discard the known session");
  assert.equal(page.$("#retryConnectionButton").disabled, false);
  assert.doesNotMatch(page.$("#connectionError").textContent, /过期|重新登录/);
  page.api = async path => path === "/healthz" ? { adminConfigured: true } : { csrfToken: "same-session" };
  page.restoreBrowserRoute = async () => { page.view = "agentView"; };
  await page.bootstrap();
  assert.equal(page.view, "agentView");
  assert.equal(page.csrfToken, "same-session");
  assert.equal(page.$("#healthDot").classList.contains("bad"), false);
});

function recoveryFixture(error) {
  const notices = [], retries = [], runtime = [];
  const context = vm.createContext({
    agent: { selectionEpoch: 1, connectionWanted: true, threadId: "thread", socketInitialized: false },
    WebSocket: { OPEN: 1 },
    agentRecoveryAllowed: () => true, traceNearBottom: () => false,
    loadAgentTranscript: async () => {}, startAgentRuntime: async () => { throw error; },
    stopAgentRecovery: () => { context.agent.connectionWanted = false; },
    setConversationNotice: message => notices.push(message),
    setAgentRuntimeState: message => runtime.push(message),
    scheduleAgentRecovery: () => retries.push(true),
  });
  for (const name of ["isAdminSessionExpired", "recoverAgentSession"]) load(context, name);
  return { page: context, notices, retries, runtime };
}

test("recovery reports an expired session only for authentication_required", async () => {
  const fixture = recoveryFixture(failure(401, "authentication_required"));
  await fixture.page.recoverAgentSession();
  assert.match(fixture.notices[0], /登录已过期/);
  assert.equal(fixture.page.agent.connectionWanted, false);
  assert.equal(fixture.retries.length, 0);
});

for (const code of ["permission_denied", "invalid_csrf"]) test(`recovery distinguishes ${code} from expiration`, async () => {
  const fixture = recoveryFixture(failure(403, code));
  await fixture.page.recoverAgentSession();
  assert.match(fixture.notices[0], /权限校验/);
  assert.doesNotMatch(fixture.notices[0], /过期|重新登录/);
  assert.equal(fixture.retries.length, 0);
});

test("recovery retries transient network failures", async () => {
  const fixture = recoveryFixture(new TypeError("Failed to fetch"));
  await fixture.page.recoverAgentSession();
  assert.equal(fixture.page.agent.connectionWanted, true);
  assert.equal(fixture.notices.length, 0);
  assert.equal(fixture.retries.length, 1);
  assert.match(fixture.runtime[0], /自动重连/);
});

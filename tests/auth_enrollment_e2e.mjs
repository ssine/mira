import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import process from "node:process";

import { requestAddress } from "../server/auth.mjs";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const serverUrl = (process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const adminPassword = process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password";
const nodeKey = `auth-e2e-${process.pid}`;
const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-auth-e2e-"));
const identityFile = path.join(temporary, "identity.json");
const nodeBinary = path.join(temporary, "mira-node");
const cliBinary = path.join(temporary, "mira");
execFileSync("go", ["build", "-o", nodeBinary, "./cmd/mira-node"], { cwd: path.join(root, "node") });
execFileSync("go", ["build", "-o", cliBinary, "./cmd/mira"], { cwd: path.join(root, "node") });

const previousTrustProxy = process.env.MIRA_TRUST_PROXY_HEADERS;
delete process.env.MIRA_TRUST_PROXY_HEADERS;
if (requestAddress({ headers: { "x-forwarded-for": "198.51.100.12" }, socket: { remoteAddress: "127.0.0.1" } }) !== "127.0.0.1") {
  throw new Error("untrusted proxy header changed the request address");
}
process.env.MIRA_TRUST_PROXY_HEADERS = "true";
if (requestAddress({ headers: { "x-forwarded-for": "198.51.100.12" }, socket: { remoteAddress: "172.18.0.1" } }) !== "198.51.100.12") {
  throw new Error("explicitly trusted proxy header was not used");
}
if (previousTrustProxy === undefined) delete process.env.MIRA_TRUST_PROXY_HEADERS;
else process.env.MIRA_TRUST_PROXY_HEADERS = previousTrustProxy;

async function request(pathname, options = {}) {
  const response = await fetch(serverUrl + pathname, options);
  let body = {};
  try { body = await response.json(); } catch { /* empty */ }
  return { response, body };
}

async function waitFor(operation, description, timeoutMs = 30_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const result = await operation();
    if (result) return result;
    await new Promise((resolve) => setTimeout(resolve, 150));
  }
  throw new Error(`timed out waiting for ${description}`);
}

const login = await request("/v1/admin/login", {
  method: "POST", headers: { "content-type": "application/json" },
  body: JSON.stringify({ username: "admin", password: adminPassword }),
});
if (!login.response.ok) throw new Error(`admin login failed: ${JSON.stringify(login.body)}`);
const session = {
  cookie: login.response.headers.get("set-cookie")?.split(";", 1)[0],
  csrf: login.body.csrfToken,
};
async function admin(pathname, options = {}) {
  return request(pathname, { ...options, headers: {
    cookie: session.cookie, "x-mira-csrf": session.csrf, "content-type": "application/json",
    ...(options.headers ?? {}),
  } });
}

const passwordAsBearer = await request("/v1/nodes", { headers: { authorization: `Bearer ${adminPassword}` } });
if (passwordAsBearer.response.status !== 401) throw new Error("administrator password was accepted as a bearer token");

const child = spawn(nodeBinary, [], {
  cwd: temporary,
  env: { ...process.env,
    MIRA_SERVER_URL: serverUrl, MIRA_NODE_KEY: nodeKey, MIRA_IDENTITY_FILE: identityFile,
    MIRA_NODE_ALLOWED_ROOTS: JSON.stringify([temporary]), MIRA_NODE_HEARTBEAT_SECONDS: "1",
    APP_SERVER_AUTO_START: "false", MIRA_NODE_TOKEN: "", CONTROL_SERVER_TOKEN: "",
  },
  stdio: ["ignore", "pipe", "pipe"],
});
const logs = [];
let childB = null;
for (const stream of [child.stdout, child.stderr]) { stream.setEncoding("utf8"); stream.on("data", (chunk) => logs.push(chunk)); }

try {
  const pending = await waitFor(async () => {
    if (child.exitCode !== null) throw new Error(`mira-node exited: ${logs.join("").slice(-2000)}`);
    const result = await admin("/v1/admin/enrollments?status=pending");
    return result.body.data?.find((item) => item.nodeKey === nodeKey);
  }, "pending enrollment");
  const pendingIdentity = JSON.parse(await fs.readFile(identityFile, "utf8"));
  if (!pendingIdentity.token?.startsWith(`mira_node_${pendingIdentity.credentialId}_`)) throw new Error("identity token format is invalid");
  if (!logs.join("").includes(`"verificationCode":"${pending.verificationCode}"`)) throw new Error("verification code was not shown by mira-node");
  if (logs.join("").includes(pendingIdentity.token)) throw new Error("Node credential leaked into logs");

  const beforeApproval = await request("/v1/nodes/register", {
    method: "POST", headers: { authorization: `Bearer ${pendingIdentity.token}`, "content-type": "application/json" }, body: "{}",
  });
  if (beforeApproval.response.status !== 403) throw new Error(`unapproved Node returned ${beforeApproval.response.status}, expected 403`);

  const missingCsrf = await request(`/v1/admin/enrollments/${pending.enrollmentId}/approve`, {
    method: "POST", headers: { cookie: session.cookie, "content-type": "application/json" }, body: "{}",
  });
  if (missingCsrf.response.status !== 403) throw new Error("admin mutation did not require CSRF");
  const approved = await admin(`/v1/admin/enrollments/${pending.enrollmentId}/approve`, { method: "POST", body: "{}" });
  if (!approved.response.ok || approved.body.status !== "approved") throw new Error(`approval failed: ${JSON.stringify(approved.body)}`);

  const active = await waitFor(async () => {
    const result = await admin("/v1/nodes?includeRevoked=true");
    return result.body.data?.find((node) => node.nodeKey === nodeKey && node.channelStatus?.connected === true);
  }, "approved Node reverse channel");
  const identity = JSON.parse(await fs.readFile(identityFile, "utf8"));
  if (identity.nodeId !== active.nodeId || identity.token !== pendingIdentity.token) throw new Error("approval did not preserve the original local credential");

  const nodeCannotAdmin = await request("/v1/admin/enrollments", { headers: { authorization: `Bearer ${identity.token}` } });
  if (nodeCannotAdmin.response.status !== 403) throw new Error("Node credential reached administrator API");
  const nodes = await request("/v1/nodes", { headers: { authorization: `Bearer ${identity.token}`, "x-mira-client-type": "cli" } });
  if (!nodes.response.ok || !nodes.body.data.some((node) => node.nodeId === active.nodeId)) throw new Error("approved Node could not list trusted Nodes");

  const status = await request(`/v1/nodes/${active.nodeId}/invoke`, {
    method: "POST", headers: { authorization: `Bearer ${identity.token}`, "x-mira-client-type": "cli", "content-type": "application/json" },
    body: JSON.stringify({ capability: "status", params: {} }),
  });
  if (!status.response.ok || status.body.result.hostname !== active.hostname) throw new Error(`status invocation failed: ${JSON.stringify(status.body)}`);
  const roots = await request(`/v1/nodes/${active.nodeId}/invoke`, {
    method: "POST", headers: { authorization: `Bearer ${identity.token}`, "content-type": "application/json" },
    body: JSON.stringify({ capability: "file", params: { action: "roots" } }),
  });
  if (!roots.response.ok || roots.body.result.roots[0].configured !== temporary) throw new Error("Node local allowed roots were not retained");
  const escaped = await request(`/v1/nodes/${active.nodeId}/invoke`, {
    method: "POST", headers: { authorization: `Bearer ${identity.token}`, "content-type": "application/json" },
    body: JSON.stringify({ capability: "file", params: { action: "stat", path: "/etc/passwd" } }),
  });
  if (escaped.response.status !== 400) throw new Error("target Node allowed-root boundary was bypassed");

  const storeId = `auth-e2e-${process.pid}`;
  const storeWrite = await request(`/v1/stores/${storeId}`, {
    method: "PUT", headers: { authorization: `Bearer ${identity.token}`, "x-mira-client-type": "codex", "content-type": "application/json" },
    body: JSON.stringify({ expectedVersion: 0, snapshot: { histories: { parent: [{ role: "user", content: "preserve me" }] } } }),
  });
  if (!storeWrite.response.ok) throw new Error(`Node credential could not write ThreadStore: ${JSON.stringify(storeWrite.body)}`);

  const cli = execFileSync(cliBinary, ["nodes", "get", "--node", nodeKey, "--json"], {
    env: { ...process.env, MIRA_IDENTITY_FILE: identityFile }, encoding: "utf8",
  });
  const cliResult = JSON.parse(cli);
  if (cliResult.schemaVersion !== 1 || cliResult.data.nodeId !== active.nodeId) throw new Error("mira CLI did not reuse the Node identity");
  const unsafeOverride = spawnSync(cliBinary, ["--server", "https://credential-sink.invalid", "nodes", "list"], {
    env: { ...process.env, MIRA_IDENTITY_FILE: identityFile }, encoding: "utf8",
  });
  if (unsafeOverride.status !== 64 || !unsafeOverride.stderr.includes("credential is bound")) {
    throw new Error("mira CLI accepted a Server override for a durable Node credential");
  }

  const rootB = path.join(temporary, "node-b");
  const identityFileB = path.join(rootB, "identity.json");
  const nodeKeyB = `${nodeKey}-b`;
  await fs.mkdir(rootB);
  childB = spawn(nodeBinary, [], {
    cwd: rootB,
    env: { ...process.env,
      MIRA_SERVER_URL: serverUrl, MIRA_NODE_KEY: nodeKeyB, MIRA_IDENTITY_FILE: identityFileB,
      MIRA_NODE_ALLOWED_ROOTS: JSON.stringify([rootB]), MIRA_NODE_HEARTBEAT_SECONDS: "1",
      APP_SERVER_AUTO_START: "false", MIRA_NODE_TOKEN: "", CONTROL_SERVER_TOKEN: "",
    }, stdio: ["ignore", "pipe", "pipe"],
  });
  const pendingB = await waitFor(async () => {
    const result = await admin("/v1/admin/enrollments?status=pending");
    return result.body.data?.find((item) => item.nodeKey === nodeKeyB);
  }, "second Node enrollment");
  await admin(`/v1/admin/enrollments/${pendingB.enrollmentId}/approve`, { method: "POST", body: "{}" });
  const activeB = await waitFor(async () => {
    const result = await request("/v1/nodes", { headers: { authorization: `Bearer ${identity.token}` } });
    return result.body.data?.find((node) => node.nodeKey === nodeKeyB && node.channelStatus?.connected === true);
  }, "second Node reverse channel");
  const remotePath = path.join(rootB, "from-node-a.txt");
  const writeCLI = spawnSync(cliBinary, ["--json", "file", "write", "--node", nodeKeyB, "--path", remotePath, "--stdin", "--overwrite"], {
    env: { ...process.env, MIRA_IDENTITY_FILE: identityFile }, input: "NODE_A_TO_B", encoding: "utf8",
  });
  if (writeCLI.status !== 0) throw new Error(`cross-Node CLI write failed: ${writeCLI.stderr}`);
  if (await fs.readFile(remotePath, "utf8") !== "NODE_A_TO_B") throw new Error("Node A CLI did not write through Node B");
  const processCLI = spawnSync(cliBinary, ["--json", "--timeout", "10s", "process", "run", "--node", activeB.nodeId, "--", "/bin/sh", "-c", "printf NODE_B_PROCESS_OK"], {
    env: { ...process.env, MIRA_IDENTITY_FILE: identityFile }, encoding: "utf8",
  });
  if (processCLI.status !== 0 || !processCLI.stdout.includes("NODE_B_PROCESS_OK")) throw new Error(`cross-Node CLI process failed: ${processCLI.stderr}`);

  const fakeNodeId = "00000000-0000-4000-8000-000000000001";
  const impersonation = new WebSocket(
    `${serverUrl.replace(/^http/, "ws")}/v1/nodes/${fakeNodeId}/connect`,
    ["mira-node-v1", `auth.${Buffer.from(identity.token).toString("base64url")}`],
  );
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("impersonation websocket was not rejected")), 5000);
    impersonation.addEventListener("open", () => { clearTimeout(timer); reject(new Error("Node A impersonated Node B")); }, { once: true });
    impersonation.addEventListener("error", () => { clearTimeout(timer); resolve(); }, { once: true });
  });

  const revoked = await admin(`/v1/admin/nodes/${active.nodeId}/revoke`, {
    method: "POST", body: JSON.stringify({ reason: "authentication E2E" }),
  });
  if (!revoked.response.ok) throw new Error(`revoke failed: ${JSON.stringify(revoked.body)}`);
  const afterRevoke = await waitFor(async () => {
    const result = await request("/v1/nodes", { headers: { authorization: `Bearer ${identity.token}` } });
    return result.response.status === 403 ? result : null;
  }, "revoked credential denial");
  if (afterRevoke.body.code !== "node_revoked") throw new Error("revoked request did not use stable error code");

  const preserved = await admin(`/v1/stores/${storeId}`);
  if (!preserved.response.ok || preserved.body.snapshot?.histories?.parent?.length !== 1) throw new Error("revocation deleted ThreadStore history");

  const freshEnrollment = await waitFor(async () => {
    const result = await admin("/v1/admin/enrollments?status=pending");
    return result.body.data?.find((item) => item.nodeKey === nodeKey);
  }, "fresh enrollment after revocation");
  const freshLocalIdentity = JSON.parse(await fs.readFile(identityFile, "utf8"));
  if (freshLocalIdentity.token === identity.token) throw new Error("re-enrollment did not rotate the credential");
  const restored = await admin(`/v1/admin/nodes/${active.nodeId}/restore`, {
    method: "POST", body: JSON.stringify({ enrollmentId: freshEnrollment.enrollmentId, note: "restore E2E" }),
  });
  if (!restored.response.ok || restored.body.nodeId !== active.nodeId) throw new Error(`credential-rotating restore failed: ${JSON.stringify(restored.body)}`);
  await waitFor(async () => {
    const value = JSON.parse(await fs.readFile(identityFile, "utf8"));
    if (value.nodeId !== active.nodeId || value.token !== freshLocalIdentity.token) return null;
    const result = await request("/v1/nodes", { headers: { authorization: `Bearer ${value.token}` } });
    return result.response.ok ? value : null;
  }, "restored Node with rotated credential");
  const oldAfterRestore = await request("/v1/nodes", { headers: { authorization: `Bearer ${identity.token}` } });
  if (oldAfterRestore.response.status !== 403) throw new Error("old credential became valid after restore");

  const audit = await admin("/v1/admin/audit-events?limit=100");
  const actions = new Set(audit.body.data.map((event) => event.action));
  for (const action of ["node.enrollment.requested", "node.enrollment.approved", "capability.invoked", "capability.failed", "node.revoked"]) {
    if (!actions.has(action)) throw new Error(`missing audit action ${action}`);
  }
  if (JSON.stringify(audit.body).includes(identity.token)) throw new Error("Node token leaked into audit output");

  console.log(JSON.stringify({ ok: true, nodeId: active.nodeId, sharedIdentity: true, adminSession: true,
    approvalRequired: true, reverseChannelBound: true, capabilityService: true, threadPreserved: true,
    revocationImmediate: true, credentialRotatingRestore: true,
    cliSchemaVersion: cliResult.schemaVersion, crossNodeCLI: true, serverCredentialBinding: true,
    trustedProxyOptIn: true }));
} catch (error) {
  console.error(JSON.stringify({ ok: false, error: error.message, logs: logs.join("").slice(-4000) }));
  process.exitCode = 1;
} finally {
  if (child.exitCode === null) { const exited = new Promise((resolve) => child.once("exit", resolve)); child.kill("SIGTERM"); await exited; }
  if (childB?.exitCode === null) { const exited = new Promise((resolve) => childB.once("exit", resolve)); childB.kill("SIGTERM"); await exited; }
  await fs.rm(temporary, { recursive: true, force: true });
}

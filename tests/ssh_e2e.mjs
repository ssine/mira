// Self-contained local acceptance: isolated database, Server, two approved Nodes.
// Requires the development PostgreSQL service; never uses production identities.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn, execFileSync } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { hashPassword } from "../server/auth.mjs";
import { loginAdmin, approvePendingNode, adminRequest } from "./auth_helpers.mjs";

const repo = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const { Pool } = pg;
const directory = await fs.mkdtemp(path.join(os.tmpdir(), "mira-ssh-e2e-"));
const database = `mira_ssh_test_${process.pid}_${crypto.randomBytes(3).toString("hex")}`;
const baseURL = process.env.MIRA_TEST_DATABASE_URL ?? "postgresql://mira:mira-local@127.0.0.1:55432/mira";
const connection = new URL(baseURL); connection.pathname = "/"+database;
const rootPool = new Pool({ connectionString: baseURL });
let pool;
const processes = [];
const url = `http://127.0.0.1:${process.env.MIRA_SSH_TEST_PORT ?? 8879}`;
const nodeBinary = path.join(repo, "tests/bin/mira-node-ssh-e2e");
const cliBinary = path.join(repo, "tests/bin/mira-ssh-e2e");
const logs = [];
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const wait = async (fn, label) => { for (let i=0; i<150; i++) { const value = await fn(); if (value) return value; await sleep(200); } throw new Error(`timeout: ${label}`); };
function launch(executable, args, env) {
  const child = spawn(executable, args, { cwd: repo, env: { ...process.env, ...env }, stdio: ["ignore", "pipe", "pipe"] });
  for (const stream of [child.stdout, child.stderr]) stream.on("data", chunk => { logs.push(chunk.toString()); if (logs.length > 100) logs.shift(); });
  processes.push(child); return child;
}
function cli(identity, args, { input = "", timeout = 15_000 } = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(cliBinary, args, { env: { ...process.env, MIRA_IDENTITY_FILE: identity } });
    const timer = setTimeout(() => { child.kill("SIGKILL"); reject(new Error("CLI timeout: "+args.join(" "))); }, timeout);
    const stdout = [], stderr = [];
    child.stdout.on("data", c => stdout.push(c)); child.stderr.on("data", c => stderr.push(c));
    child.on("error", reject); child.on("close", code => { clearTimeout(timer); resolve({ code, stdout: Buffer.concat(stdout), stderr: Buffer.concat(stderr).toString() }); });
    child.stdin.end(input);
  });
}
try {
  await rootPool.query(`CREATE DATABASE ${database}`);
  pool = new Pool({ connectionString: connection.toString() });
  await initializeDatabase(pool);
  await pool.query("INSERT INTO mira_admin_users (username, password_hash) VALUES ('admin', $1)", [await hashPassword("mira-local-admin-password")]);
  await fs.mkdir(path.join(repo, "tests/bin"), { recursive: true });
  for (const [binary, command] of [[nodeBinary,"mira-node"], [cliBinary,"mira"]]) execFileSync("go", ["build", "-o", binary, `./cmd/${command}`], { cwd: path.join(repo, "node") });
  launch(process.execPath, ["server/server.mjs"], { DATABASE_URL: connection.toString(), LISTEN_HOST: process.env.MIRA_SSH_TEST_LISTEN ?? "127.0.0.1", LISTEN_PORT: new URL(url).port, MIRA_SECURE_COOKIES: "false" });
  await wait(async () => { try { return (await fetch(url+"/healthz")).ok; } catch { return false; } }, "Server");
  const admin = await loginAdmin(url);
  const nodes = [];
  for (const name of ["source", "target"]) {
    const root = path.join(directory, name); await fs.mkdir(root);
    const identity = path.join(directory, name+"-identity.json"); const key = `ssh-e2e-${process.pid}-${name}`;
    launch(nodeBinary, [], { MIRA_SERVER_URL: url, MIRA_IDENTITY_FILE: identity, MIRA_NODE_KEY: key,
      MIRA_NODE_ALLOWED_ROOTS: JSON.stringify([root]), MIRA_NODE_TOKEN: "", CONTROL_SERVER_TOKEN: "", APP_SERVER_AUTO_START: "false", MIRA_NODE_HEARTBEAT_SECONDS: "1" });
    await approvePendingNode(url, admin, key);
    const state = await wait(async () => { try { const state = JSON.parse(await fs.readFile(identity)); return state.nodeId ? state : null; } catch { return null; } }, "approval");
    await wait(async () => { const list = await adminRequest(url, admin, "/v1/nodes"); return list.data.find(n => n.nodeId === state.nodeId)?.channelStatus?.connected; }, "Node channel");
    nodes.push({ root, identity, key, state });
  }
  const [a,b] = nodes;
  // Optional local acceptance hook for a physical device; never enabled in CI.
  if (process.env.MIRA_SSH_DEVICE_TEST) {
    const hook = await import(pathToFileURL(path.resolve(process.env.MIRA_SSH_DEVICE_TEST)));
    await hook.default({ url, nodes, cli, admin });
  }
  if (process.env.MIRA_SSH_WINDOWS_BIN) {
    const binaryDir = process.env.MIRA_SSH_WINDOWS_BIN;
    await fs.copyFile(path.join(repo, "tests/windows_ssh_e2e.ps1"), path.join(binaryDir, "windows_ssh_e2e.ps1"));
    const winPath = input => execFileSync("wslpath", ["-w", input], { encoding: "utf8" }).trim();
    const out = execFileSync("/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe", ["-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", winPath(path.join(binaryDir, "windows_ssh_e2e.ps1")), "-BinaryDirectory", winPath(binaryDir), "-ServerUrl", url, "-LinuxNode", b.key], { encoding: "utf8", timeout: 180_000 });
    console.log(out.trim());
  }
  let result = await cli(a.identity, ["ssh", b.key, "--", "printf OUT; printf ERR >&2; exit 7"]);
  assert.equal(result.code, 7, result.stderr); assert.equal(result.stdout.toString(), "OUT"); assert.equal(result.stderr, "ERR");
  result = await cli(a.identity, ["ssh", b.key, "--", "cat"], { input: Buffer.from([0, 1, 255, 128, 13, 10]) });
  assert.equal(result.code, 0, result.stderr); assert.deepEqual(result.stdout, Buffer.from([0, 1, 255, 128, 13, 10]));
  result = await cli(a.identity, ["ssh", "-t", b.key, "--", "stty size; printf PTY_OK"]);
  assert.equal(result.code, 0, result.stderr); assert.match(result.stdout.toString(), /24 80/); assert.match(result.stdout.toString(), /PTY_OK/);
  const original = path.join(a.root, "original.bin"); const restored = path.join(a.root, "restored.bin");
  const payload = crypto.randomBytes(9*1024*1024+37); await fs.writeFile(original, payload);
  result = await cli(a.identity, ["scp", original, `${b.key}:${b.root}/remote.bin`]); assert.equal(result.code, 0, result.stderr);
  result = await cli(a.identity, ["scp", `${b.key}:${b.root}/remote.bin`, restored]); assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(await fs.readFile(restored), payload);
  result = await cli(a.identity, ["scp", original, `${b.key}:${b.root}/remote.bin`]); assert.notEqual(result.code, 0, "overwrite must require opt-in");
  result = await cli(a.identity, ["sftp", b.key, "ls", b.root]); assert.equal(result.code, 0, result.stderr); assert.match(result.stdout.toString(), /remote.bin/);
  result = await cli(a.identity, ["sftp", b.key, "stat", a.root]); assert.notEqual(result.code, 0, "SFTP roots escaped");
  // Full duplex session revocation must stop promptly, including worker children.
  const pidFile = path.join(b.root, "ssh-child.pid");
  const alive = cli(a.identity, ["ssh", b.key, "--", `echo $$ > '${pidFile}'; exec sleep 60`], { timeout: 10_000 });
  const childPID = await wait(async () => { try { return Number((await fs.readFile(pidFile)).toString().trim()); } catch { return null; } }, "SSH child PID");
  await adminRequest(url, admin, `/v1/admin/nodes/${a.state.nodeId}/revoke`, { method: "POST", body: "{}" });
  result = await alive; assert.notEqual(result.code, 0);
  await wait(async () => { try { process.kill(childPID, 0); return false; } catch (err) { return err.code === "ESRCH"; } }, "SSH child reaped after revocation");
  result = await cli(a.identity, ["ssh", b.key, "--", "echo forbidden"]); assert.notEqual(result.code, 0);
  const audits = await pool.query("SELECT action, metadata FROM mira_audit_events WHERE action LIKE 'ssh.%'");
  assert(audits.rows.some(r => r.action === "ssh.requested")); assert(audits.rows.some(r => r.action === "ssh.closed"));
  assert(!JSON.stringify(audits.rows).includes(a.state.token));
  assert(!JSON.stringify(audits.rows).includes("printf OUT"));
  console.log("SSH E2E passed: approved reverse relay, worker exec/PTY, binary stdin/EOF, separate stderr/exit, 9 MiB SFTP roundtrip, roots, no-overwrite, live revocation, metadata-only audit.");
} catch (error) {
  // Node logs contain enrollment metadata, never tokens; do not dump identity files.
  console.error(logs.join("").slice(-12000)); throw error;
} finally {
  for (const child of processes.reverse()) {
    if (child.exitCode !== null) continue;
    child.kill("SIGTERM"); await Promise.race([new Promise(resolve => child.once("close", resolve)), sleep(3000)]);
    if (child.exitCode === null) child.kill("SIGKILL");
  }
  await pool?.end();
  await rootPool.query(`DROP DATABASE IF EXISTS ${database} WITH (FORCE)`); await rootPool.end();
  await fs.rm(directory, { recursive: true, force: true });
}

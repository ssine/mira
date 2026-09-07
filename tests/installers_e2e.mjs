import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const version = (await fs.readFile(path.join(root, "VERSION"), "utf8")).trim();
const windowsInstaller = await fs.readFile(path.join(root, "scripts", "install.ps1"), "utf8");
const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-installers-e2e-"));
const releases = path.join(temporary, "releases");
const fakeBin = path.join(temporary, "fake-bin");

assert.equal(windowsInstaller.includes("api.github.com"), false,
  "the public Windows bootstrap must not consume the unauthenticated GitHub API quota");
assert.match(windowsInstaller, /github\.com\/ssine\/mira\/releases\/latest/,
  "the public Windows bootstrap must resolve the latest release through GitHub's redirect");

function command(program, args, options = {}) {
  return execFileSync(program, args, { cwd: root, encoding: "utf8", ...options });
}

function digest(value) {
  return createHash("sha256").update(value).digest("hex");
}

function isolatedEnvironment(home, identity) {
  return {
    ...Object.fromEntries(Object.entries(process.env).filter(([name]) =>
      !/^(MIRA_|NODE_AGENT_|APP_SERVER_|ANDROID_NATIVE_|CONTROL_SERVER_)/.test(name))),
    HOME: home,
    PATH: `${fakeBin}:${process.env.PATH}`,
    MIRA_IDENTITY_FILE: identity,
    CODEX_BINARY: path.join(temporary, "no-codex"),
  };
}

async function installRole(role) {
  const home = path.join(temporary, `home-${role}`);
  const state = path.join(temporary, `state-${role}`);
  const identity = path.join(home, ".config", "mira", "identity.json");
  const environment = isolatedEnvironment(home, identity);
  await fs.mkdir(home, { recursive: true });
  const args = [path.join(root, "scripts/install.sh"), "--version", version,
    "--release-directory", releases, "--state-dir", state,
    "--role", role, "--service-owner", "mira", "--service-manager", "systemd",
    "--service-scope", "user"];
  if (role === "node") args.push("--server", "http://127.0.0.1:9");
  command("sh", args, { env: environment });

  const executable = path.join(state, "current", "mira");
  assert.match(command(executable, ["--version"], { env: environment }), new RegExp(version.replaceAll(".", "\\.")));
  const installState = JSON.parse(await fs.readFile(path.join(state, "install-state.json"), "utf8"));
  assert.equal(installState.role, role);
  assert.equal(installState.serviceOwner, "mira");
  assert.equal(installState.serviceManager, "systemd");
  assert.equal(installState.serviceScope, "user");
  const service = await fs.readFile(path.join(home, ".config", "systemd", "user", "mira.service"), "utf8");
  assert.match(service, / supervisor --state-dir /);
  assert.equal(service.includes(" --server"), role === "server");
  assert.equal(await fs.stat(path.join(state, "versions", version, "mira-codex-package")).catch(() => null), null,
    "the unified Mira image must not bundle the optional Codex runtime");
  assert.equal(await fs.stat(path.join(state, "versions", version, "mira-node")).catch(() => null), null,
    "Mira roles must not be selected through a mira-node alias");
  assert.equal(await fs.stat(path.join(home, ".local", "bin", "mira-node")).catch(() => null), null,
    "the installer must expose only the canonical mira launcher");
  return { args, environment, executable, state };
}

try {
  await fs.mkdir(releases);
  await fs.mkdir(fakeBin);
  await fs.writeFile(path.join(fakeBin, "systemctl"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  const releaseSource = process.env.MIRA_TEST_RELEASE_DIRECTORY ?? path.join(root, "dist");
  for (const file of await fs.readdir(releaseSource)) {
    await fs.copyFile(path.join(releaseSource, file), path.join(releases, file));
  }

  const node = await installRole("node");
  const original = digest(await fs.readFile(node.executable));
  const originalState = digest(await fs.readFile(path.join(node.state, "install-state.json")));
  command("sh", node.args, { env: node.environment });
  assert.equal(digest(await fs.readFile(node.executable)), original, "idempotent bootstrap changed the selected image");
  assert.equal(digest(await fs.readFile(path.join(node.state, "install-state.json"))), originalState,
    "idempotent bootstrap changed installation ownership");

  assert.throws(() => command("sh", [path.join(root, "scripts/install.sh"), "--update"], {
    env: node.environment, stdio: "pipe",
  }), (error) => error.status === 2);
  assert.equal(digest(await fs.readFile(node.executable)), original,
    "the rejected legacy update path changed the installed image");

  await installRole("server");

  const corrupt = path.join(temporary, "corrupt-releases");
  await fs.cp(releases, corrupt, { recursive: true });
  const archive = path.join(corrupt, `mira_${version}_linux_${process.arch === "arm64" ? "arm64" : "amd64"}.tar.gz`);
  await fs.appendFile(archive, "corruption");
  assert.throws(() => command("sh", [path.join(root, "scripts/install.sh"), "--version", version,
    "--release-directory", corrupt, "--state-dir", path.join(temporary, "corrupt-state"),
    "--role", "node", "--service-scope", "user"], { env: node.environment, stdio: "pipe" }));

  console.log(JSON.stringify({
    ok: true,
    initialNodeBootstrap: true,
    initialServerBootstrap: true,
    idempotentBootstrap: true,
    legacyInstallerUpdateRejected: true,
    checksumFailureRejected: true,
    windowsLatestReleaseRedirect: true,
    supervisorUpdateCoveredByGoTests: true,
  }));
} finally {
  await fs.rm(temporary, { recursive: true, force: true });
}

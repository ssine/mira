import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const current = (await fs.readFile(path.join(root, "VERSION"), "utf8")).trim();
const previous = "0.8.999";
const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-installers-e2e-"));
const releases = path.join(temporary, "releases");
const prefix = path.join(temporary, "prefix");
const identity = path.join(temporary, "state", "identity.json");
const environment = { ...process.env, MIRA_IDENTITY_FILE: identity, CODEX_BINARY: path.join(temporary, "no-codex") };
let child;

function command(program, args, options = {}) {
  return execFileSync(program, args, { cwd: root, encoding: "utf8", ...options });
}
function digest(value) { return createHash("sha256").update(value).digest("hex"); }
async function waitFile(file) {
  for (let attempt = 0; attempt < 100; attempt++) {
    try { return await fs.readFile(file); } catch { await new Promise((resolve) => setTimeout(resolve, 100)); }
  }
  throw new Error(`file was not created: ${file}`);
}

try {
  await fs.mkdir(releases);
  for (const file of await fs.readdir(path.join(root, "dist"))) await fs.copyFile(path.join(root, "dist", file), path.join(releases, file));
  for (const platform of ["linux", "windows"]) {
    const packageName = `mira_${previous}_${platform}_amd64`;
    const packageDirectory = path.join(temporary, packageName);
    await fs.mkdir(packageDirectory);
    for (const name of ["mira", "mira-node"]) {
      command("go", ["build", "-trimpath", "-ldflags", `-s -w -X github.com/ssine/mira/node/internal/node.Version=${previous}`, "-o", path.join(packageDirectory, name + (platform === "windows" ? ".exe" : "")), `./cmd/${name}`], {
        cwd: path.join(root, "node"), env: { ...process.env, CGO_ENABLED: "0", GOOS: platform, GOARCH: "amd64" },
      });
    }
    const filename = packageName + (platform === "windows" ? ".zip" : ".tar.gz");
    if (platform === "windows") command("zip", ["-qr", path.join(releases, filename), packageName], { cwd: temporary });
    else command("tar", ["-czf", path.join(releases, filename), packageName], { cwd: temporary });
    await fs.appendFile(path.join(releases, "SHA256SUMS"), `${digest(await fs.readFile(path.join(releases, filename)))}  ${filename}\n`);
  }
  const install = [path.join(root, "scripts/install.sh"), "--prefix", prefix, "--release-directory", releases];
  command("sh", [...install, "--version", previous, "--server", "http://127.0.0.1:9"], { env: environment });
  child = spawn(path.join(prefix, "bin/mira-node"), [], { env: environment, stdio: "ignore" });
  const identityBefore = digest(await waitFile(identity));
  const configuration = path.join(path.dirname(identity), "node.json");
  const configBefore = digest(await fs.readFile(configuration));
  child.kill("SIGTERM");
  await new Promise((resolve) => child.once("exit", resolve));
  child = null;
  command("sh", [...install, "--version", current, "--update"], { env: environment });
  assert.equal(digest(await fs.readFile(identity)), identityBefore);
  assert.equal(digest(await fs.readFile(configuration)), configBefore);
  const version = JSON.parse(command(path.join(prefix, "bin/mira"), ["--json", "version"], { env: environment }));
  assert.equal(version.data.build.version, current);
  const codexPackage = path.join(prefix, "share/mira/versions", current, "mira-codex-package");
  if (await fs.stat(codexPackage).catch(() => null)) {
    await fs.access(path.join(codexPackage, "bin/codex"));
    await fs.access(path.join(codexPackage, "codex-resources/bwrap"));
    await fs.access(path.join(codexPackage, "codex-package.json"));
  }
  await fs.access(path.join(prefix, "share/mira/versions", previous, "mira-node"));
  const protectedInstall = await fs.readFile(path.join(prefix, "bin/mira-node"));
  await fs.appendFile(path.join(releases, `mira_${current}_linux_amd64.tar.gz`), "corruption");
  assert.throws(() => command("sh", [...install, "--version", current, "--update"], { env: environment, stdio: "pipe" }));
  assert.equal(digest(await fs.readFile(path.join(prefix, "bin/mira-node"))), digest(protectedInstall));

  let windows = false;
  const powershell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe";
  try { await fs.access(powershell); } catch { /* Linux CI has no Windows interop. */ }
  if (await fs.stat(powershell).catch(() => null)) {
    const windowsPath = (value) => command("wslpath", ["-w", value]).trim();
    const result = command(powershell, ["-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", windowsPath(path.join(root, "tests/windows_install_e2e.ps1")), "-Installer", windowsPath(path.join(root, "scripts/install.ps1")), "-ReleaseDirectory", windowsPath(releases), "-CurrentVersion", current, "-PreviousVersion", previous, "-TestPath"], { timeout: 120_000 });
    assert.match(result, /WINDOWS_INSTALL_UPDATE_OK/);
    if (process.env.MIRA_TEST_WINDOWS_SERVICE === "1") {
      const serviceResult = command(powershell, ["-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", windowsPath(path.join(root, "tests/windows_install_e2e.ps1")), "-Installer", windowsPath(path.join(root, "scripts/install.ps1")), "-ReleaseDirectory", windowsPath(releases), "-CurrentVersion", current, "-PreviousVersion", previous, "-TestService"]);
      assert.match(serviceResult, /WINDOWS_SERVICE_UPDATE_OK/);
    }
    windows = true;
  }
  console.log(JSON.stringify({ ok: true, linuxInstall: true, windowsInstall: windows, windowsServiceUpgrade: windows && process.env.MIRA_TEST_WINDOWS_SERVICE === "1", previousVersion: previous, currentVersion: current, identityPreserved: true, configurationPreserved: true, previousBinaryRetained: true, checksumFailureRejected: true }));
} finally {
  if (child) child.kill("SIGTERM");
  await fs.rm(temporary, { recursive: true, force: true });
}

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import { createReadStream } from "node:fs";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { codexRuntime } from "./check-codex-runtime.mjs";

const [outputArgument, ...sources] = process.argv.slice(2);
assert(outputArgument && sources.length, "Usage: node scripts/build-codex-release.mjs OUTPUT linux-amd64=PACKAGE windows-amd64=PACKAGE");
const output = path.resolve(outputArgument);
await fs.mkdir(output, { recursive: true });
assert.equal((await fs.readdir(output)).length, 0, "Use a fresh Codex release directory; published runtimes are immutable");
const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-codex-release-"));
const manifest = { ...codexRuntime, targets: {} };
const digests = [];

async function sha256(file) {
  const hash = crypto.createHash("sha256");
  for await (const chunk of createReadStream(file)) hash.update(chunk);
  return hash.digest("hex");
}

async function inventory(directory, relative = "") {
  const files = [];
  for (const entry of (await fs.readdir(path.join(directory, relative), { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name))) {
    assert(!/[\\:\x00]/.test(entry.name) && !/[. ]$/.test(entry.name), `Unsafe package path: ${entry.name}`);
    const name = relative ? `${relative}/${entry.name}` : entry.name;
    const file = path.join(directory, name);
    if (entry.isDirectory()) files.push(...await inventory(directory, name));
    else {
      assert(entry.isFile(), `Runtime symlinks/special files are not supported: ${name}`);
      // GitHub workflow artifacts lose mode bits; restore canonical executables.
      const executable = /^(bin\/|codex-path\/)|^codex-resources\/(bwrap$|zsh\/bin\/)/.test(name);
      const mode = executable ? 0o755 : 0o644;
      await fs.chmod(file, mode);
      const size = (await fs.stat(file)).size;
      assert(size <= 1024 ** 3, `Runtime file too large: ${name}`);
      files.push({ path: name, size, sha256: await sha256(file), mode });
    }
  }
  return files;
}

try {
  for (const source of sources) {
    const equal = source.indexOf("=");
    assert(equal > 0, "Expected platform=canonical-package-directory");
    const platform = source.slice(0, equal);
    assert(["linux-amd64", "windows-amd64"].includes(platform) && !manifest.targets[platform], `Invalid/duplicate platform: ${platform}`);
    const prefix = `mira-codex_${codexRuntime.version}_${platform.replaceAll("-", "_")}`;
    const stage = path.join(temporary, prefix);
    const input = path.resolve(source.slice(equal + 1));
    await fs.cp(input, stage, { recursive: true, verbatimSymlinks: true });
    const canonical = JSON.parse(await fs.readFile(path.join(stage, "codex-package.json"), "utf8"));
    assert.equal(canonical.version, codexRuntime.upstreamVersion);
    assert.equal(canonical.layoutVersion, 1);
    assert.equal(canonical.variant, "codex");
    assert.equal(canonical.target, platform === "windows-amd64" ? "x86_64-pc-windows-msvc" : "x86_64-unknown-linux-musl");
    assert.equal(canonical.entrypoint, platform === "windows-amd64" ? "bin/codex.exe" : "bin/codex");
    const files = await inventory(stage);
    const suffix = platform === "windows-amd64" ? ".exe" : "";
    const required = ["codex-package.json", `bin/codex${suffix}`, `bin/codex-code-mode-host${suffix}`, `codex-path/rg${suffix}`];
    required.push(...(suffix ? ["codex-resources/codex-command-runner.exe", "codex-resources/codex-windows-sandbox-setup.exe"] : ["codex-resources/bwrap"]));
    for (const name of required) assert(files.some((file) => file.path === name && file.size > 0), `Missing/empty canonical file: ${name}`);
    assert(files.length <= 4096 && files.reduce((sum, file) => sum + file.size, 0) <= 2 * 1024 ** 3);
    const archive = prefix + (suffix ? ".zip" : ".tar.gz");
    if (suffix) execFileSync("zip", ["-qr", path.join(output, archive), prefix], { cwd: temporary });
    else execFileSync("tar", ["-czf", path.join(output, archive), "-C", temporary, prefix]);
    const digest = await sha256(path.join(output, archive));
    const size = (await fs.stat(path.join(output, archive))).size;
    assert(size > 0 && size <= 1024 ** 3);
    manifest.targets[platform] = { archive, size, sha256: digest, files };
    digests.push(`${digest}  ${archive}`);
  }
  const manifestFile = path.join(output, "codex-runtime.json");
  await fs.writeFile(manifestFile, JSON.stringify(manifest, null, 2) + "\n", { flag: "wx" });
  digests.push(`${await sha256(manifestFile)}  codex-runtime.json`);
  await fs.writeFile(path.join(output, "SHA256SUMS"), digests.sort().join("\n") + "\n", { flag: "wx" });
  console.log(`Built independent Codex ${codexRuntime.version}: ${Object.keys(manifest.targets).join(", ")}`);
} finally {
  await fs.rm(temporary, { recursive: true, force: true });
}

import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";
import "./check-codex-runtime.mjs";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const version = fs.readFileSync(path.join(root, "VERSION"), "utf8").trim();
const codexVersion = fs.readFileSync(path.join(root, "CODEX_VERSION"), "utf8").trim();
if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version)) throw new Error(`VERSION is not a stable semantic version: ${version}`);
const [major, minor, patch] = version.split(".").map(Number);
if (minor >= 1000 || patch >= 1000 || major * 1_000_000 + minor * 1000 + patch > 2_100_000_000) {
  throw new Error("VERSION does not fit the monotonic Android versionCode layout");
}

const goVersion = fs.readFileSync(path.join(root, "node/internal/version.go"), "utf8");
const nodeDockerfile = fs.readFileSync(path.join(root, "node/Dockerfile"), "utf8");
const installationDocuments = ["README.md", "INSTALL.md"].map((name) => [
  name, fs.readFileSync(path.join(root, name), "utf8"),
]);

if (!goVersion.includes(`Version   = "${version}"`)) throw new Error("Go default version does not match VERSION");
if (!nodeDockerfile.includes(`ARG MIRA_VERSION=${version}`)) throw new Error("Node Docker default version does not match VERSION");
for (const [name, content] of installationDocuments) {
  if (!content.includes(`--version ${version}`) || !content.includes(`-Version '${version}'`)) {
    throw new Error(`${name} one-line installers do not match VERSION`);
  }
}
if (!/^\d+\.\d+\.\d+$/.test(codexVersion)) throw new Error("CODEX_VERSION is not a semantic version");

process.stdout.write(`Mira version ${version} and Codex baseline ${codexVersion} are consistent.\n`);

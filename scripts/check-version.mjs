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

const serverPackage = JSON.parse(fs.readFileSync(path.join(root, "server/package.json"), "utf8"));
const serverLock = JSON.parse(fs.readFileSync(path.join(root, "server/package-lock.json"), "utf8"));
const goVersion = fs.readFileSync(path.join(root, "node/internal/version.go"), "utf8");
const nodeDockerfile = fs.readFileSync(path.join(root, "node/Dockerfile"), "utf8");

const mirrors = [
  ["server/package.json", serverPackage.version],
  ["server/package-lock.json", serverLock.version],
  ["server/package-lock.json root package", serverLock.packages?.[""]?.version],
];
for (const [name, value] of mirrors) {
  if (value !== version) throw new Error(`${name} has ${value}; expected ${version}`);
}
if (!goVersion.includes(`Version   = "${version}"`)) throw new Error("Go default version does not match VERSION");
if (!nodeDockerfile.includes(`ARG MIRA_VERSION=${version}`)) throw new Error("Node Docker default version does not match VERSION");
if (!/^\d+\.\d+\.\d+$/.test(codexVersion)) throw new Error("CODEX_VERSION is not a semantic version");

process.stdout.write(`Mira version ${version} and Codex baseline ${codexVersion} are consistent.\n`);

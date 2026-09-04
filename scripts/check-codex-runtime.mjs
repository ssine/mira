import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";

const root = new URL("../", import.meta.url);
export const codexRuntime = JSON.parse(fs.readFileSync(new URL("node/internal/codex-runtime.json", root)));
const upstream = fs.readFileSync(new URL("CODEX_VERSION", root), "utf8").trim();
const patch = fs.readFileSync(new URL("patches/codex/0001-feat-thread-store-add-remote-PostgreSQL-adapter.patch", root));
assert.equal(codexRuntime.schemaVersion, 1);
assert.equal(codexRuntime.upstreamVersion, upstream);
assert.match(codexRuntime.version, new RegExp(`^${upstream.replaceAll(".", "\\.")}-mira\\.[1-9][0-9]*$`));
assert.equal(codexRuntime.patchSHA256, crypto.createHash("sha256").update(patch).digest("hex"), "Codex patch changed: update the runtime lock and increment its Mira revision");
assert.deepEqual(fs.readdirSync(new URL("patches/codex/", root)).filter((name) => name.endsWith(".patch")), ["0001-feat-thread-store-add-remote-PostgreSQL-adapter.patch"], "Update runtime source validation before adding another patch");

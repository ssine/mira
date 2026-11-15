// Keeps resumed Claude CLI processes on the account's current login.
//
// When a turn resumes through Mira's SessionStore, the Agent SDK materializes
// the transcript into a private `<tmpdir>/claude-resume-<id>/` directory and
// starts the CLI with CLAUDE_CONFIG_DIR pointing there. It copies the
// account's `.credentials.json` into that directory once, with the refresh
// token removed. The CLI then reads only that copy: it never sees a renewal
// written to the account directory, and without a refresh token it cannot
// renew itself. After the next renewal, its next request fails with "OAuth
// token revoked" even though the account is signed in.
//
// The mirror watches the account's credentials and copies each renewed access
// token into every resume copy that still holds one of this account's earlier
// tokens, without the refresh token (as the SDK does). The CLI notices the
// changed file before its next request and re-reads it after a 401, so it
// continues with the current login. Only the Node renews the login.
import { readdirSync, readFileSync, renameSync, statSync, unwatchFile, watchFile, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

function readOAuth(path) {
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch {
    return null;
  }
}

// Copies the account's current access token into resume copies of this
// account. Returns the number of copies updated.
export function syncResumeCredentials(accountFile, known, root = tmpdir()) {
  const current = readOAuth(accountFile);
  const token = current?.claudeAiOauth?.accessToken;
  if (!token) return 0;
  let updated = 0;
  let entries = [];
  try {
    entries = readdirSync(root).filter((name) => name.startsWith("claude-resume-"));
  } catch {
    return 0;
  }
  for (const name of entries) {
    const path = join(root, name, ".credentials.json");
    const copy = readOAuth(path);
    const held = copy?.claudeAiOauth?.accessToken;
    // Only copies made from this account's login, and only when stale.
    if (!held || held === token || !known.has(held)) continue;
    const next = { ...copy, claudeAiOauth: { ...current.claudeAiOauth } };
    delete next.claudeAiOauth.refreshToken;
    try {
      const temp = `${path}.mira-${process.pid}`;
      writeFileSync(temp, JSON.stringify(next), { mode: 0o600 });
      renameSync(temp, path);
      updated++;
    } catch {
      // The CLI finished and its directory is gone.
    }
  }
  known.add(token);
  return updated;
}

// Starts mirroring for one worker; returns a stop function.
export function mirrorResumeCredentials(configDir, { intervalMs = 1000, root } = {}) {
  if (!configDir) return () => {};
  const accountFile = join(configDir, ".credentials.json");
  const known = new Set();
  const initial = readOAuth(accountFile)?.claudeAiOauth?.accessToken;
  if (initial) known.add(initial);
  let last = 0;
  const check = () => {
    let mtime = 0;
    try {
      mtime = statSync(accountFile).mtimeMs;
    } catch {
      return;
    }
    if (mtime === last) return;
    last = mtime;
    syncResumeCredentials(accountFile, known, root);
  };
  check();
  watchFile(accountFile, { interval: intervalMs, persistent: false }, check);
  return () => unwatchFile(accountFile, check);
}

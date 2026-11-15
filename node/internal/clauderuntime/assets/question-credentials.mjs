// A turn waiting on AskUserQuestion keeps one Claude CLI process and the OAuth
// tokens it read at startup. Refresh tokens rotate: another process on the same
// account (a turn, a title or a plan-usage read) refreshing meanwhile leaves the
// waiting process with tokens the provider then rejects as revoked. A stale
// answer is therefore not handed to that process; the worker ends it and
// resumes the session in a fresh one that reads the current credentials.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

// Also refresh after a long wait when credentials cannot be compared, for
// example when they live in an OS keychain instead of the account directory.
export const staleQuestionMs = 60 * 60 * 1000;

export function credentialsPath(env = process.env) {
  const dir = env.MIRA_NODE_CLAUDE_CONFIG_DIR || env.CLAUDE_CONFIG_DIR || join(homedir(), ".claude");
  return join(dir, ".credentials.json");
}

// A digest of the stored OAuth tokens, or null without a readable file. The
// tokens themselves never leave this function.
export function credentialFingerprint(path = credentialsPath()) {
  try {
    const oauth = JSON.parse(readFileSync(path, "utf8"))?.claudeAiOauth;
    if (!oauth?.accessToken && !oauth?.refreshToken) return null;
    return createHash("sha256").update(`${oauth.accessToken ?? ""}\n${oauth.refreshToken ?? ""}`).digest("hex");
  } catch {
    return null;
  }
}

// Captured when a question is asked; stale() decides when it is answered.
export function questionCredentials({ path = credentialsPath(), now = Date.now, limitMs = staleQuestionMs } = {}) {
  const askedAt = now();
  const fingerprint = credentialFingerprint(path);
  return {
    stale() {
      if (now() - askedAt >= limitMs) return true;
      return fingerprint !== null && credentialFingerprint(path) !== fingerprint;
    },
  };
}

// The answer as the next user message of the resumed session, worded like the
// native AskUserQuestion result so the model continues from it.
export function answerContinuation(questions, answers) {
  const pairs = (questions ?? [])
    .map((item) => [item?.question, answers?.[item?.question]])
    .filter(([question, answer]) => question && answer !== undefined && answer !== "")
    .map(([question, answer]) => `"${question}"="${Array.isArray(answer) ? answer.join(", ") : answer}"`);
  const body = pairs.length ? pairs.join(", ") : JSON.stringify(answers ?? {});
  return `Your questions have been answered: ${body}. You can now continue with these answers in mind.`;
}

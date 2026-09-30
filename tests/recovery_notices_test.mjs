import assert from "node:assert/strict";
import test from "node:test";
import { encryptedContextError, recoveryNoticeLabel, RecoveryNotices } from "../server/public/recovery-notices.js";

test("only known encrypted-context failures receive recovery notices", () => {
  assert(encryptedContextError("The encrypted content for item rs_example could not be verified. Reason: Encrypted content could not be decrypted or parsed. (request id: example)"));
  assert(encryptedContextError('unexpected status 409 Conflict: {"error":{"code":"unknown_reasoning_pool"}}'));
  assert(encryptedContextError("encrypted history has no known compatibility pool"));
  assert(!encryptedContextError("Permission denied"));
  assert(!encryptedContextError("HTTP 409: unrelated conflict"));
});

test("counts describe actual retries and distinguish recovery from exhaustion", () => {
  assert.equal(recoveryNoticeLabel({ retryCount: 3, status: "dispatching" }), "加密内容不匹配 · 已自动重试 3 次，正在继续");
  assert.equal(recoveryNoticeLabel({ retryCount: 2, status: "completed" }), "上下文恢复记录 · 已自动重试 2 次 · 本次重试已结束");
  assert.equal(recoveryNoticeLabel({ retryCount: 2, status: "applying" }), "加密内容不匹配 · 正在自动处理");
  assert.equal(recoveryNoticeLabel({ resolved: true }), "不兼容内容已处理");
  assert.match(recoveryNoticeLabel({ retryCount: 19, status: "stopped", reason: "同一份上下文已连续失败 20 次" }), /自动重试已停止/);
  assert(!recoveryNoticeLabel({ retryCount: 3, status: "stopped", reason: "本次自动重试未完成；兼容错误会继续恢复" }).includes("已停止"));
  assert.equal(recoveryNoticeLabel(undefined, false), "加密内容不匹配 · 自动恢复已关闭");
});

test("a late recovery response cannot overwrite a different conversation", async () => {
  const label = { textContent: "加密内容不匹配" };
  const card = { dataset: { turnId: "turn" }, querySelector: () => label };
  let resolve, current = { threadId: "first", generation: 1 };
  const notices = new RecoveryNotices({ querySelectorAll: () => [card] }, {
    current: () => current, api: () => new Promise(done => { resolve = done; }),
  });
  const request = notices.refresh();
  notices.reset(); current = { threadId: "second", generation: 1 };
  resolve({ generation: 1, enabled: true, turns: { turn: { retryCount: 8, status: "completed" } } });
  await request;
  assert.equal(label.textContent, "加密内容不匹配");
  assert.equal(notices.cache.size, 0);
});

import assert from "node:assert/strict";
import test from "node:test";
import { normalizeTokenUsage, tokenCount, compactTokenCount, compactTokenUsage } from "../server/public/thread-usage.js";

test("token counts preserve unknown and zero and treat cached input as a subset", () => {
  assert.deepEqual(normalizeTokenUsage({ input_tokens: 1000, cached_input_tokens: 800, output_tokens: 0 }),
    { inputTokens: 1000, cachedInputTokens: 800, outputTokens: 0 });
  assert.equal(normalizeTokenUsage({}), null);
  assert.equal(normalizeTokenUsage({ input_tokens: -1, output_tokens: "500", cached_input_tokens: Number.MAX_SAFE_INTEGER + 1 }), null);
  assert.deepEqual(normalizeTokenUsage({ input_tokens: 0 }), { inputTokens: 0, outputTokens: null, cachedInputTokens: null });
  assert.equal(tokenCount(null), "未提供");
  assert.equal(tokenCount(0), "0");
  assert.equal(tokenCount(1234567), "1,234,567");
});

test("sidebar summaries stay short, round across unit boundaries and omit partial data", () => {
  assert.equal(compactTokenCount(999), "999");
  assert.equal(compactTokenCount(125000), "125k");
  assert.equal(compactTokenCount(999999), "1M");
  assert.equal(compactTokenCount(1250000), "1.3M");
  assert.equal(compactTokenUsage({ inputTokens: 125000, outputTokens: 8000 }), "125k in · 8k out");
  assert.equal(compactTokenUsage({ inputTokens: 0, outputTokens: 0 }), "0 in · 0 out");
  assert.equal(compactTokenUsage({ inputTokens: 100 }), "");
  assert.equal(compactTokenUsage(null), "");
});

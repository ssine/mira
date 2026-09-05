import assert from 'node:assert/strict';
import test from 'node:test';
import { weeklyQuota, resetTime } from '../server/public/account-status.js';

test('weekly quota follows the Codex bucket and window duration, not primary/secondary position or Spark', () => {
  const week = { windowDurationMins: 10080, usedPercent: 52, resetsAt: 1789125976 };
  const spark = { limitId: 'codex_bengalfox', primary: { ...week, usedPercent: 0 } };
  for (const position of ['primary', 'secondary']) {
    assert.deepEqual(weeklyQuota({ rateLimits: spark, rateLimitsByLimitId: {
      codex_bengalfox: spark, codex: { limitId: 'codex', [position]: week },
    }, rateLimitResetCredits: { availableCount: 3, credits: [{}] } }), {
      remaining: 48, resetsAt: 1789125976000, resetCount: 3,
    });
  }
  assert.equal(weeklyQuota({ rateLimits: spark }).remaining, null);
  assert.equal(weeklyQuota({ rateLimits: { primary: week } }).remaining, 48, 'legacy default bucket is supported');
});

test('unknown quotas and reset credits remain unknown, while real zero values remain zero', () => {
  assert.deepEqual(weeklyQuota({}), { remaining: null, resetsAt: null, resetCount: null });
  const empty = { rateLimits: { primary: { windowDurationMins: 10080, usedPercent: null, resetsAt: null } } };
  assert.deepEqual(weeklyQuota(empty), { remaining: null, resetsAt: null, resetCount: null });
  assert.deepEqual(weeklyQuota({ rateLimits: { primary: { windowDurationMins: 10080, usedPercent: 100 } }, rateLimitResetCredits: { availableCount: 0 } }), { remaining: 0, resetsAt: null, resetCount: 0 });
  assert.equal(weeklyQuota({ rateLimits: { primary: { windowDurationMins: 300, usedPercent: 75 } } }).remaining, null);
  assert.equal(resetTime(null), '未提供');
  assert.equal(resetTime(1000, 2000), '等待更新', 'an expired snapshot must not imply a refreshed quota');
});

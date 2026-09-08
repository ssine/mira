import assert from 'node:assert/strict';
import { quotaReferences } from '../server/public/account-history.js';
const day = 86400_000, week = 7 * day;
const p = (at, remaining, resetsAt = week) => ({ at, remaining, resetsAt });
const lines = points => quotaReferences(points, 0, 14 * day);
assert.deepEqual(lines([p(0, 100), p(day, 100), p(2 * day, 80)]),
  [{ from: 0, to: week, remainingFrom: 100, remainingTo: 0 }], 'full plateau keeps original anchor');
const extra = lines([p(0, 100), p(day, 40), p(2 * day, 100), p(3 * day, 100)]);
assert.equal(extra.length, 2);
assert.equal(extra[0].to, 2 * day, 'early reset cuts off old line');
assert.equal(extra[1].from, 2 * day, 'unchanged deadline does not override observed refill');
assert.equal(extra[1].to, 9 * day);
assert.equal(extra[1].remainingFrom, 100);
assert.equal(extra[1].remainingTo, 0);
assert.equal(lines([p(0, 100), p(day, 20), p(2 * day, 95)])[1].from, 2 * day, 'refill can be sampled below 100');
assert.equal(lines([p(6 * day, 50), p(8 * day, 40, 14 * day)])[1].from, week, 'changed window detects a reset even after heavy use');
assert.equal(lines([p(2 * day, 60)])[0].from, 0, 'deadline reconstructs missed start');
assert.equal(lines([p(0, 100), p(day, null), p(2 * day, 70)]).length, 1);
assert.deepEqual(lines([p(0, 80, null)]), [], 'no fabricated cycle without evidence');
assert.deepEqual(lines([]), []);
for (const span of [day, week, 30 * day]) {
 const [line] = quotaReferences([p(0, 100)], day / 2, span);
 assert.equal(line.from, day / 2);
 assert.equal(line.to, Math.min(week, span));
 assert.ok(Math.abs((line.remainingFrom - line.remainingTo) / (line.to - line.from) - 100 / week) < 1e-12);
}
assert.equal(lines([p(0, 100), p(day, 80, week + 1000), p(2 * day, 60, week - 1000), p(3 * day, 40, week + 3000)]).length, 1, 'deadline jitter does not restart references');
const plateau = lines([p(0, 100), p(day, 20), p(2 * day, 100, 9 * day), p(3 * day, 100, 10 * day), p(4 * day, 90, 10 * day + 1000)]);
assert.equal(plateau.length, 2, 'advancing full-quota deadline is still one refill');
assert.equal(plateau[1].from, 2 * day);
assert.equal(plateau[1].to, 9 * day, 'full plateau must retain the first refill anchor');
assert.equal(lines([p(0, 100), p(day, 80), p(2 * day, 60, 9 * day)]).length, 1, 'deadline correction without refill or expiry cannot start a cycle');
console.log('quota reference tests passed');

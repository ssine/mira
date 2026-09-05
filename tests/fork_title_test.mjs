import assert from 'node:assert/strict';
import test from 'node:test';
import { nextForkTitle } from '../server/thread-management.mjs';

test('fork titles inherit the source, skip occupied numbers and increment existing suffixes', () => {
  assert.equal(nextForkTitle('修复 Mira 标题', []), '修复 Mira 标题 (1)');
  assert.equal(nextForkTitle('修复 Mira 标题', ['修复 Mira 标题 (1)', '修复 Mira 标题 (2)']), '修复 Mira 标题 (3)');
  assert.equal(nextForkTitle('修复 Mira 标题 (2)', ['修复 Mira 标题 (1)', '修复 Mira 标题 (2)']), '修复 Mira 标题 (3)');
  assert.equal(nextForkTitle('修复 Mira 标题', ['修复 Mira 标题 (1)', '修复 Mira 标题 (3)']), '修复 Mira 标题 (2)');
  assert.equal(nextForkTitle(null, []), '新会话 (1)');
});

test('long Unicode titles keep the number and remain valid for the rename API', () => {
  const source = 'a' + '🙂'.repeat(100);
  const first = nextForkTitle(source, []);
  assert.ok(first.length <= 200);
  assert.ok(first.isWellFormed());
  assert.ok(first.endsWith(' (1)'));
  const second = nextForkTitle(source, [first]);
  assert.ok(second.endsWith(' (2)'));
  assert.ok(second.length <= 200);
  assert.ok(second.isWellFormed());
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { ThreadPager, mergeThreadPages } from '../server/public/thread-pages.js';
import { buildThreadTree } from '../server/public/thread-list.js';

const page = (ids, nextCursor = null) => ({ paged: true, data: ids.map(threadId => ({ threadId, generation: 1, itemCount: 1 })), nextCursor, total: 120 });
test('pages merge; head discovery preserves cursor and existing pages', async () => {
  let rows = [], calls = [];
  const pager = new ThreadPager(async query => { calls.push(Object.fromEntries(query)); return query.has('head') ? page(['new']) : query.has('cursor') ? page(['old']) : page(['first'], 'cursor'); }, result => rows = mergeThreadPages(rows, result.data));
  await pager.load('roots', 'project');
  await pager.load('roots', 'project', true);
  assert.equal(pager.state('roots', 'project').cursor, 'cursor');
  await pager.load('roots', 'project');
  assert.deepEqual(rows.map(row => row.threadId), ['first', 'new', 'old']);
  assert.equal(calls[2].cursor, 'cursor');
});
test('expired cursors restart without discarding already loaded rows', async () => {
  let rows = [], count = 0;
  const pager = new ThreadPager(async query => {
    if (query.has('cursor')) throw Object.assign(new Error('expired'), { code: 'thread_cursor_expired' });
    return ++count === 1 ? page(['first'], 'expired') : page(['new', 'first'], 'fresh');
  }, result => rows = mergeThreadPages(rows, result.data));
  await pager.load('children', 'parent');
  await pager.load('children', 'parent');
  assert.deepEqual(rows.map(row => row.threadId), ['first', 'new']);
  assert.equal(pager.state('children', 'parent').cursor, 'fresh');
});
test('duplicate loads coalesce and old-filter responses are ignored', async () => {
  let resolve, accepted = 0;
  const pager = new ThreadPager(() => new Promise(done => resolve = done), () => accepted++);
  const first = pager.load('roots');
  assert.equal(pager.load('roots'), first);
  pager.reset(true);
  resolve(page(['old-filter']));
  assert.equal(await first, undefined);
  assert.equal(accepted, 0);
  assert.equal(pager.state('roots').started, false);
});
test('failed loads retain cursor and can be retried', async () => {
  let fail = false;
  const pager = new ThreadPager(async () => { if (fail) throw new Error('offline'); return page(['row'], 'next'); }, () => {});
  await pager.load('children', 'parent');
  fail = true;
  await assert.rejects(pager.load('children', 'parent'), /offline/);
  assert.equal(pager.state('children', 'parent').cursor, 'next');
  assert.equal(pager.state('children', 'parent').loading, false);
  fail = false;
  await pager.load('children', 'parent');
  assert.equal(pager.state('children', 'parent').error, null);
});
test('refresh removes explicit rows and refuses older history generations', () => {
  const before = [{ threadId: 'parent', generation: 2, itemCount: 1, childCount: 100 }, {threadId:'removed'}];
  const after = mergeThreadPages(before, [{threadId:'parent',generation:1,itemCount:100}], ['removed']);
  assert.deepEqual(after, [before[0]]);
  const partial = mergeThreadPages(after, [{threadId:'parent',generation:2,itemCount:2}]);
  assert.equal(partial[0].childCount, 100);
});
test('unloaded descendants retain server counts and canonical cycle roots', () => {
  const { roots } = buildThreadTree([
    {threadId:'root', parentThreadId:'child', listRoot:true, subagentCount:100},
    {threadId:'child', parentThreadId:'root', subagentCount:50},
  ]);
  assert.equal(roots.length, 1);
  assert.equal(roots[0].threadId, 'root');
  assert.equal(roots[0].descendantCount, 100);
  assert.equal(roots[0].children[0].descendantCount, 50);
});
test('a background head read neither consumes nor blocks a user page request', async () => {
  let resolveHead, changed = 0;
  const pager = new ThreadPager(query => query.has('head') ? new Promise(done => resolveHead = done) : Promise.resolve(page(['row'], 'next')), () => false, () => changed++);
  await pager.load('roots', 'project');
  const background = pager.load('roots', 'project', true);
  assert.equal(pager.state('roots', 'project').loading, false);
  await pager.load('roots', 'project');
  assert.equal(pager.state('roots', 'project').cursor, 'next');
  const renders = changed;
  resolveHead(page(['row']));
  await background;
  assert.equal(pager.state('roots', 'project').cursor, 'next');
  assert.equal(changed, renders, 'unchanged head reads preserve DOM and focused controls');
});

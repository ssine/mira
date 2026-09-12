import assert from 'node:assert/strict';
import test from 'node:test';
import { AccountRecovery } from '../server/public/account-recovery.js';

function element() {
  return {
    hidden: true, children: [], listeners: {},
    replaceChildren() { this.children = []; },
    append(child) { this.children.push(child); },
    addEventListener(name, callback) { this.listeners[name] = callback; },
  };
}
globalThis.document = { createElement: element };
const settle = () => new Promise(resolve => setImmediate(resolve));
const plan = { failureId: 'failure', generation: 1, itemCount: 5, policy: 'omitReasoning', recoverable: true };
const resolved = () => Object.assign(new Error('no pending failure'), { code: 'no_context_failure', status: 409 });

function fixture() {
  const root = element(), requests = [];
  const recovery = new AccountRecovery(root, {
    api: (path, options) => new Promise((resolve, reject) => requests.push({ path, options, resolve, reject })),
    beforeApply: async () => {}, afterApply: async () => {}, notice: () => {}, confirm: () => true,
  });
  return { root, recovery, requests };
}

test('resolved recovery clears the notice and stays clear when reopening', async () => {
  const { root, recovery, requests } = fixture();
  recovery.select('thread', 'account'); requests.shift().resolve(plan); await settle();
  assert.equal(root.hidden, false);
  recovery.observeTurn('other-thread'); assert.equal(requests.length, 0);
  recovery.observeTurn('thread'); requests.shift().reject(resolved()); await settle();
  assert.equal(root.hidden, true);
  assert.equal(root.children.length, 0);
  recovery.select('thread', 'account'); requests.shift().reject(resolved()); await settle();
  assert.equal(root.hidden, true);
});

test('transient refresh failure preserves the notice and a new failure shows again', async () => {
  const { root, recovery, requests } = fixture();
  recovery.select('thread', 'account'); requests.shift().resolve(plan); await settle();
  const refresh = recovery.refresh(); requests.shift().reject(new Error('offline')); await refresh;
  assert.equal(root.hidden, false);
  const clear = recovery.refresh(); requests.shift().reject(resolved()); await clear;
  recovery.observe({ threadId: 'thread', nodeAccountId: 'account' });
  requests.shift().resolve({ ...plan, failureId: 'new-failure' }); await settle();
  assert.equal(root.hidden, false);
});

test('a delayed clear for another account cannot hide the current failure', async () => {
  const { root, recovery, requests } = fixture();
  recovery.select('thread', 'old-account'); const stale = requests.shift();
  recovery.select('thread', 'new-account'); requests.shift().resolve(plan); await settle();
  stale.reject(resolved()); await settle();
  assert.equal(root.hidden, false);
});

test('a pre-confirmation refresh cannot resurrect the notice after confirmation', async () => {
  const { root, recovery, requests } = fixture();
  recovery.select('thread', 'account'); requests.shift().resolve(plan); await settle();
  const refresh = recovery.refresh(), stale = requests.shift();
  const apply = root.children[1].listeners.click(); await settle();
  const post = requests.shift();
  assert.equal(post.options.method, 'POST');
  assert.equal(JSON.parse(post.options.body).confirm, true);
  post.resolve({ reloadedThreadId: 'thread' }); await apply;
  stale.resolve(plan); await refresh;
  assert.equal(root.hidden, true);
  assert.equal(requests.length, 0, 'confirmation does not replay a turn');
});

import assert from 'node:assert/strict';
import test from 'node:test';
import { interruptThread } from '../server/public/thread-interrupt.js';

function runtime(t, { turn = { id: 'turn-1', status: 'inProgress' }, failAt, holdAt } = {}) {
  const originalSocket = globalThis.WebSocket, originalLocation = globalThis.location;
  const requests = [], sockets = [];
  class Socket extends EventTarget {
    static OPEN = 1;
    readyState = 0;
    constructor(url) {
      super(); this.url = url; sockets.push(this);
      if (holdAt !== 'open') queueMicrotask(() => { this.readyState = 1; this.dispatchEvent(new Event('open')); });
    }
    send(data) {
      const request = JSON.parse(data); requests.push(request);
      if (request.id === undefined || holdAt === request.method) return;
      queueMicrotask(() => {
        if (failAt === request.method) { this.close(); return; }
        const result = request.method === 'thread/turns/list' ? { data: turn ? [turn] : [] } : {};
        this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify({ id: request.id, result }) }));
      });
    }
    close() { this.readyState = 3; this.dispatchEvent(new Event('close')); }
  }
  globalThis.WebSocket = Socket;
  globalThis.location = { host: 'mira.test', protocol: 'https:' };
  t.after(() => { globalThis.WebSocket = originalSocket; globalThis.location = originalLocation; });
  return { requests, sockets, stop: () => interruptThread({ nodeId: 'original-node', nodeAccountId: 'original-account', threadId: 'thread-1', turnId: 'turn-1', timeoutMs: 100 }) };
}

test('a reader can stop the recorded runtime without loading or resuming a thread', async t => {
  const { stop, requests, sockets } = runtime(t);
  assert.deepEqual(await stop(), { interrupted: true });
  assert.match(sockets[0].url, /\/nodes\/original-node\/app-server\?.*nodeAccountId=original-account/);
  assert.deepEqual(requests.map(r => r.method), ['initialize', 'initialized', 'thread/turns/list', 'turn/interrupt']);
  assert.deepEqual(requests.at(-1).params, { threadId: 'thread-1', turnId: 'turn-1' });
  assert.equal(requests[2].params.itemsView, 'notLoaded');
  assert.equal(sockets[0].readyState, 3);
});

for (const status of ['completed', 'failed', 'interrupted']) test(`already ${status} turns are not interrupted again`, async t => {
  const { stop, requests } = runtime(t, { turn: { id: 'turn-1', status } });
  assert.deepEqual(await stop(), { interrupted: false });
  assert.equal(requests.some(r => r.method === 'turn/interrupt'), false);
});

test('a stale stop never targets a newer turn', async t => {
  const { stop, requests, sockets } = runtime(t, { turn: { id: 'turn-2', status: 'inProgress' } });
  await assert.rejects(stop(), /轮次已变化/);
  assert.equal(requests.some(r => r.method === 'turn/interrupt'), false);
  assert.equal(sockets[0].readyState, 3);
});

for (const phase of ['open', 'initialize', 'thread/turns/list', 'turn/interrupt']) test(`timeout during ${phase} closes the connection`, async t => {
  const { stop, sockets } = runtime(t, { holdAt: phase });
  await assert.rejects(stop(), /超时/);
  assert.equal(sockets[0].readyState, 3);
});

test('a disconnected request fails instead of remaining pending', async t => {
  const { stop } = runtime(t, { failAt: 'turn/interrupt' });
  await assert.rejects(stop(), /断开/);
});

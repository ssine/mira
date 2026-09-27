import assert from 'node:assert/strict';
import test from 'node:test';
import { AutomaticRecovery } from '../server/public/automatic-recovery.js';

function fixture() {
  const requests = [], notices = [];
  const input = { addEventListener() {} }, status = {};
  const control = new AutomaticRecovery(input, status, {
    api: (path, options) => new Promise((resolve, reject) => requests.push({ path, options, resolve, reject })),
    notice: text => notices.push(text),
  });
  return { input, status, requests, notices, control };
}
test('new conversations default on; preferences persist through Server without starting turns', async () => {
  const f = fixture();
  await f.control.select(null);
  assert.equal(f.input.checked, true);
  assert.equal(f.requests.length, 0);
  const load = f.control.select('thread');
  f.requests.shift().resolve({ enabled: true, generation: 2 }); await load;
  f.input.checked = false;
  const save = f.control.save(), request = f.requests.shift();
  assert.equal(request.options.method, 'PUT');
  assert.deepEqual(JSON.parse(request.options.body), { enabled: false, generation: 2 });
  request.resolve({ enabled: false, generation: 2 }); await save;
  assert.equal(f.input.checked, false);
  assert.equal(f.input.disabled, false);
  assert.equal(f.requests.length, 0);
});
test('switching conversations ignores stale reads and saves', async () => {
  const f = fixture(), old = f.control.select('old'), oldRequest = f.requests.shift();
  const current = f.control.select('current');
  f.requests.shift().resolve({ enabled: false, generation: 3 }); await current;
  oldRequest.resolve({ enabled: true, generation: 1 }); await old;
  assert.equal(f.input.checked, false);
  f.input.checked = true;
  const save = f.control.save(), saveRequest = f.requests.shift();
  const next = f.control.select('next');
  f.requests.shift().resolve({ enabled: false, generation: 4 }); await next;
  saveRequest.resolve({ enabled: true, generation: 3 }); await save;
  assert.equal(f.input.checked, false);
  assert.equal(f.control.saved.generation, 4);
});
test('failed preference writes restore the saved value and report the error', async () => {
  const f = fixture(), load = f.control.select('thread');
  f.requests.shift().resolve({ enabled: true, generation: 1 }); await load;
  f.input.checked = false;
  const save = f.control.save(); f.requests.shift().reject(new Error('offline')); await save;
  assert.equal(f.input.checked, true);
  assert.deepEqual(f.notices, ['offline']);
});

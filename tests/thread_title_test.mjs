import assert from 'node:assert/strict';
import test from 'node:test';
import { titlePrompt, parseGeneratedTitle, generateThreadTitle } from '../server/public/thread-title.js';

test('title context excludes tools and commentary, bounds recent Unicode text, validates structured output', () => {
  const prompt = titlePrompt([
    { kind: 'user', body: 'old '.repeat(1000) },
    ...Array.from({ length: 8 }, (_, i) => ({ kind: i % 2 ? 'assistant' : 'user', body: `${i}标题${'🙂'.repeat(1000)}` })),
    { kind: 'tool', body: 'secret tool output' }, { kind: 'assistant', phase: 'commentary', body: 'progress only' },
  ]);
  assert.doesNotMatch(prompt, /old|secret tool output|progress only/);
  const data = JSON.parse(prompt.split('Conversation data:\n')[1]);
  assert.equal(data.length, 8);
  assert.ok(data.every(item => [...item.text].length === 600));
  assert.equal(parseGeneratedTitle('{"title":"  修复  Mira 标题  "}'), '修复 Mira 标题');
  assert.equal([...parseGeneratedTitle(JSON.stringify({ title: '🙂'.repeat(50) }))].length, 36);
  for (const text of ['text', '{}', '{"title":""}', '{"title":"ok","extra":1}', '{"title":"\\u0000"}']) {
    assert.throws(() => parseGeneratedTitle(text));
  }
  assert.throws(() => titlePrompt([{ kind: 'tool', body: 'no conversation' }]));
});

function runtime(t, { hold = false, readonly = true, account = 'chatgpt', output = '{"title":"生成的标题"}', tools = false } = {}) {
  const originalSocket = globalThis.WebSocket, originalLocation = globalThis.location;
  const requests = [], sockets = [];
  class Socket extends EventTarget {
    static OPEN = 1;
    readyState = 0;
    constructor() { super(); sockets.push(this); queueMicrotask(() => { this.readyState = 1; this.dispatchEvent(new Event('open')); }); }
    message(message) { this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(message) })); }
    send(data) {
      const request = JSON.parse(data); requests.push(request);
      const reply = result => this.message({ id: request.id, result });
      if (request.id === undefined) return;
      queueMicrotask(() => {
        if (request.method === 'initialize') reply({});
        else if (request.method === 'config/read') reply({ config: { model: 'current-model', model_provider: 'openai', mcp_servers: { dangerous: { command: 'must-not-start' } } } });
        else if (request.method === 'account/read') reply({ account: { type: account } });
        else if (request.method === 'model/list') reply({ data: [{ model: 'gpt-5.6-luna' }] });
        else if (request.method === 'thread/start') reply({ thread: { id: 'temporary', ephemeral: true }, sandbox: { type: readonly ? 'readOnly' : 'dangerFullAccess' } });
        else if (request.method === 'turn/start') {
          this.message({ method: 'turn/started', params: { threadId: 'temporary', turn: { id: 'title-turn' } } });
          if (tools) this.message({ id: 'tool', method: 'item/tool/call', params: { threadId: 'temporary' } });
          else if (!hold) {
            this.message({ method: 'item/completed', params: { threadId: 'temporary', item: { type: 'agentMessage', text: output } } });
            this.message({ method: 'turn/completed', params: { threadId: 'temporary', turn: { id: 'title-turn', status: 'completed' } } });
          }
          reply({ turn: { id: 'title-turn' } });
        } else if (request.method) reply({});
      });
    }
    close() { this.readyState = 3; }
  }
  globalThis.WebSocket = Socket;
  globalThis.location = { host: 'mira.test', protocol: 'https:' };
  t.after(() => { globalThis.WebSocket = originalSocket; globalThis.location = originalLocation; });
  const generate = (options = {}) => generateThreadTitle({ node: { nodeId: 'node-1', status: 'online', reportedAppServer: { status: 'running' } }, cwd: '/workspace', prompt: 'task data', ...options });
  return { generate, requests, sockets };
}

test('isolated title request accepts completion before acknowledgement and cleans up', async t => {
  const { generate, requests, sockets } = runtime(t);
  assert.equal(await generate(), '生成的标题');
  const start = requests.find(r => r.method === 'thread/start').params;
  assert.equal(start.ephemeral, true);
  assert.equal(start.sandbox, 'read-only');
  assert.equal(start.model, 'gpt-5.6-luna');
  assert.deepEqual(start.dynamicTools, []);
  assert.deepEqual(start.config.mcp_servers, { dangerous: { enabled: false } });
  for (const key of ['features.shell_tool', 'features.hooks', 'features.code_mode', 'orchestrator.skills.enabled']) assert.equal(start.config[key], false);
  assert.equal(requests.at(-1).method, 'thread/unsubscribe');
  assert.equal(sockets[0].readyState, 3);
});

test('API accounts use their configured model', async t => {
  const { generate, requests } = runtime(t, { account: 'apiKey' });
  await generate();
  assert.equal(requests.find(r => r.method === 'thread/start').params.model, 'current-model');
});

test('unsupported permissions fail before sampling', async t => {
  const { generate, requests } = runtime(t, { readonly: false });
  await assert.rejects(generate(), /隔离/);
  assert.equal(requests.some(r => r.method === 'turn/start'), false);
  assert.equal(requests.at(-1).method, 'thread/unsubscribe');
});

for (const mode of ['cancel', 'timeout', 'tool']) test(`${mode} interrupts the temporary turn and releases its subscription`, async t => {
  const { generate, requests, sockets } = runtime(t, { hold: true, tools: mode === 'tool' });
  const controller = new AbortController();
  const promise = generate({ signal: controller.signal, timeoutMs: mode === 'timeout' ? 20 : 1000 });
  const rejected = assert.rejects(promise);
  if (mode === 'cancel') {
    while (!requests.some(r => r.method === 'turn/start')) await new Promise(resolve => setTimeout(resolve, 1));
    controller.abort();
  }
  await rejected;
  assert.ok(requests.some(r => r.method === 'turn/interrupt'));
  assert.equal(requests.at(-1).method, 'thread/unsubscribe');
  assert.equal(sockets[0].readyState, 3);
});

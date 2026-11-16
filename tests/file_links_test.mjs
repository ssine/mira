import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// Exercise the reference grammar in isolation: the module's DOM and network
// parts need a browser and are covered by file_links_browser.mjs.
const source = fs.readFileSync(new URL('../server/public/file-links.js', import.meta.url), 'utf8')
  .replace(/^import .*;\n/gm, '').replace(/^export /gm, '');
const context = vm.createContext({ URL, setTimeout });
vm.runInContext(source, context);
// Classes are lexical in a script and are not properties of its global.
const FileProbe = vm.runInContext('FileProbe', context);
const call = (name, ...args) => JSON.parse(JSON.stringify(context[name](...args) ?? null));

test('references split into a path and a source position', () => {
  assert.deepEqual(call('parseFileReference', 'src/app.js:12:3'), { input: 'src/app.js', line: 12, column: 3 });
  assert.deepEqual(call('parseFileReference', 'docs/guide.md#L40'), { input: 'docs/guide.md', line: 40, column: null });
  assert.deepEqual(call('parseFileReference', '~/notes/todo.md'), { input: '~/notes/todo.md', line: null, column: null });
  assert.deepEqual(call('parseFileReference', 'C:\\Reports\\q3.json:7'), { input: 'C:\\Reports\\q3.json', line: 7, column: null });
  assert.equal(call('parseFileReference', '/C:/Reports/q3.json').input, 'C:/Reports/q3.json');
  assert.equal(call('parseFileReference', 'file:///home/me/a%20b.txt').input, '/home/me/a b.txt');
  assert.equal(call('parseFileReference', 'file://server/share/a.txt').input, '\\\\server\\share\\a.txt');
  // Only a Markdown target is percent-encoded; code and prose are literal.
  assert.equal(call('parseFileReference', 'my%20docs/a.md', { url: true }).input, 'my docs/a.md');
  assert.equal(call('parseFileReference', 'my%20docs/a.md').input, 'my%20docs/a.md');
  for (const other of ['', '#section', 'https://example.test/a.md', 'mailto:me@example.test', 'javascript:alert(1)', 'localhost:3000/a', 'a\nb'])
    assert.equal(call('parseFileReference', other), null, other);
});

test('prose candidates need path evidence, and keep their boundaries beside CJK text', () => {
  const found = text => call('scanTextForPaths', text).map(match => [text.slice(match.start, match.end), match.input, match.line]);
  assert.deepEqual(found('改动在src/app.js:12，另见 ~/notes/todo.md。'), [['src/app.js:12', 'src/app.js', 12], ['~/notes/todo.md', '~/notes/todo.md', null]]);
  assert.deepEqual(found('see ./run.sh, ../shared/lib and /etc/hosts.'), [['./run.sh', './run.sh', null], ['../shared/lib', '../shared/lib', null], ['/etc/hosts', '/etc/hosts', null]]);
  assert.deepEqual(found('Open C:\\Users\\me\\report.docx or docs/ (server/public/vendor).'),
    [['C:\\Users\\me\\report.docx', 'C:\\Users\\me\\report.docx', null], ['docs/', 'docs/', null], ['server/public/vendor', 'server/public/vendor', null]]);
  assert.deepEqual(found('error at pkg/mod/file.go:88:5: unexpected'), [['pkg/mod/file.go:88:5', 'pkg/mod/file.go', 88]]);
  for (const prose of ['and/or, TCP/IP, I/O and client/server', 'on 2026/11/16 at 24/7', 'branch feat/file-links or origin/main', 'see https://example.test/a/b.html',
    'ratio 1/2 and km/h', '和/或', '//cdn.example.test/lib.js', 'me@example.test'])
    assert.deepEqual(found(prose), [], prose);
});

test('written references resolve without a Node only when unambiguous', () => {
  assert.equal(call('resolveWithoutNode', '/abs/a.md', ''), '/abs/a.md');
  assert.equal(call('resolveWithoutNode', 'docs/a.md', '/work'), '/work/docs/a.md');
  assert.equal(call('resolveWithoutNode', 'docs/a.md', 'C:\\work'), 'C:\\work\\docs\\a.md');
  assert.equal(call('resolveWithoutNode', 'docs/a.md', ''), null);
  // The home directory belongs to the Node's user; the console cannot know it.
  assert.equal(call('resolveWithoutNode', '~/a.md', '/work'), null);
});

test('the default action follows the confirmed type, then the name', () => {
  const action = (...args) => context.defaultFileAction(...args);
  assert.equal(action('/w/a.go', 'file', 12), 'preview');
  assert.equal(action('/w/index.html', 'file', 12), 'preview');
  assert.equal(action('/w/index.html', 'file', null), 'site');
  assert.equal(action('/w/bundle.zip', 'file', null), 'workspace');
  assert.equal(action('/w/Makefile', 'file', null), 'preview');
  assert.equal(action('/w/v1.2', 'directory', null), 'workspace');
  // Unconfirmed: a trailing separator or a name without an extension reads as a directory.
  assert.equal(action('/w/docs/', '', null), 'workspace');
  assert.equal(action('/w/Makefile', '', null), 'workspace');
  assert.equal(action('/w/a.md', '', null), 'preview');
});

test('one probe request carries a whole message and answers are remembered', async () => {
  const requests = [];
  const probe = new FileProbe(async (path, body) => {
    requests.push(body);
    return { entries: body.paths.map(input => ({ input, exists: input !== 'missing.md', path: `/work/${input}`, type: 'file' })) };
  });
  const origin = { nodeId: 'node', cwd: '/work' };
  const answers = await Promise.all(['a.md', 'b.md', 'a.md', 'missing.md'].map(input => probe.request(origin, input)));
  assert.deepEqual(answers.map(entry => entry.exists), [true, true, true, false]);
  assert.equal(requests.length, 1);
  assert.deepEqual([...requests[0].paths], ['a.md', 'b.md', 'missing.md']);
  assert.equal(requests[0].cwd, '/work');
  assert.equal(probe.cached(origin, 'a.md').path, '/work/a.md');
  assert.equal(probe.cached({ nodeId: 'node', cwd: '/other' }, 'a.md'), undefined, 'the same text elsewhere is another question');
  probe.forget(origin, 'missing.md');
  assert.equal(probe.cached(origin, 'missing.md'), undefined);
  // More references than one probe allows are split, never dropped.
  const many = Array.from({ length: 70 }, (_, index) => `f${index}.md`);
  await Promise.all(many.map(input => probe.request(origin, input)));
  assert.deepEqual(requests.slice(1).map(request => request.paths.length), [64, 6]);
});

test('a Node that cannot answer is left alone and references fall back', async () => {
  let calls = 0;
  const probe = new FileProbe(async () => { calls++; throw Object.assign(new Error('old Node'), { code: 'capability_unavailable' }); });
  assert.equal(await probe.request({ nodeId: 'old' }, 'a.md'), null);
  assert.equal(await probe.request({ nodeId: 'old' }, 'b.md'), null);
  assert.equal(calls, 1);
});

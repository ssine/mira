import assert from 'node:assert/strict';
import test from 'node:test';
import {
  collectCodexTranscript, collectImages, embedDiagrams, exportBlocks, exportedImages, exportFilename, renderConversationHtml,
} from '../node/internal/webassets/web/conversation-export.js';

const markdown = text => `<p>${String(text).replace(/[&<>]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' })[c])}</p>`;
const png = 'data:image/png;base64,iVBORw0KGgo=';
const items = [
  { key: 'u1', kind: 'user', body: '帮我看看 <b>日志</b>', turnId: 't1' },
  { key: 'r1', kind: 'reasoning', title: '思考', body: '**先读取**日志文件', turnId: 't1' },
  { key: 'k1', kind: 'tool', title: 'Shell', body: 'cat /var/log/secret.log\n\nSECRET-OUTPUT', turnId: 't1',
    activity: { status: 'completed', durationMs: 1200, actions: [{ kind: 'run', label: 'cat /var/log/secret.log' }] } },
  { key: 'i1', kind: 'image', turnId: 't1', image: { href: '/v1/image/1' } },
  { key: 'a1', kind: 'assistant', body: '日志正常。', turnId: 't1', turnCompletedAt: '2026-11-16T08:00:00Z' },
  { key: 'u2', kind: 'user', body: '看图', turnId: 't2' },
  { key: 'i2', kind: 'image', turnId: 't2', image: { url: png } },
  { key: 'e1', kind: 'error', title: '运行错误', body: 'boom', turnId: 't2' },
];

test('tools, thinking and tool output images fold into one group per turn', () => {
  const blocks = exportBlocks(items);
  assert.deepEqual(blocks.map(block => block.kind), ['user', 'group', 'assistant', 'user', 'image', 'error']);
  assert.deepEqual(blocks[1].items.map(item => item.key), ['r1', 'k1', 'i1']);
  assert.deepEqual(exportedImages(items, false).map(item => item.key), ['i2']);
  assert.deepEqual(exportedImages(items, true).map(item => item.key), ['i1', 'i2']);
});

test('the default export keeps messages and summarizes tool calls without details', () => {
  const html = renderConversationHtml({ title: '排查 <日志>', engine: 'codex', threadId: 'thread-1', items, markdown,
    images: new Map([['i1', png]]) });
  assert.match(html, /^<!doctype html>/);
  assert.match(html, /<title>排查 &lt;日志&gt;<\/title>/);
  assert.match(html, /帮我看看 &lt;b&gt;日志&lt;\/b&gt;/);
  assert.match(html, /日志正常。/);
  assert.match(html, /工具调用 · 1 次 · 执行 × 1/);
  assert.match(html, /不含工具调用详情/);
  assert.doesNotMatch(html, /SECRET-OUTPUT|secret\.log|先读取/);
  // The tool's screenshot stays out; the user's image is inlined.
  assert.equal(html.match(/<img /g).length, 1);
  assert.match(html, /<section class="message error">/);
  // The file loads nothing and runs nothing.
  assert.doesNotMatch(html, /<script|<link /i);
  assert.match(html, /default-src 'none'; img-src data:/);
});

test('the detailed export expands tool input, output, thinking and images', () => {
  const html = renderConversationHtml({ title: 't', engine: 'claude', items, markdown, includeTools: true,
    images: new Map([['i1', png]]) });
  assert.match(html, /<details class="group">/);
  assert.match(html, /<details class="entry tool"><summary><span>已执行 cat \/var\/log\/secret\.log<\/span>/);
  assert.match(html, /SECRET-OUTPUT/);
  assert.match(html, /先读取/);
  assert.equal(html.match(/<img /g).length, 2);
  assert.match(html, /Claude · /);
});

test('images other than inline raster data URLs never reach the file', () => {
  const html = renderConversationHtml({ title: 't', items: [
    { key: 'x', kind: 'image', image: { url: 'https://example.test/a.png' } },
    { key: 'y', kind: 'image', image: { url: 'data:image/svg+xml;base64,PHN2Zz4=' } },
  ], markdown });
  assert.doesNotMatch(html, /<img /);
  assert.equal(html.match(/图片无法导出/g).length, 2);
});

test('file names drop path separators and carry the date', () => {
  assert.equal(exportFilename('a/b: c?', new Date(2026, 10, 16)), 'a b c 2026-11-16.html');
  assert.equal(exportFilename('', new Date(2026, 0, 2)), 'conversation 2026-01-02.html');
});

test('Codex pages are read from the tail until the beginning', async () => {
  const requests = [];
  const pages = {
    '': { generation: 3, trace: [{ key: 'b', sourceItemSeq: 2 }], nextCursor: 't2:3:2:2' },
    't2:3:2:2': { generation: 3, trace: [{ key: 'a', sourceItemSeq: 1 }], nextCursor: null },
  };
  const api = async path => {
    const query = new URL(path, 'https://mira.test').searchParams;
    requests.push(query);
    return pages[query.get('cursor') ?? ''];
  };
  const merge = (older, newer) => [...older, ...newer];
  const result = await collectCodexTranscript(api, 'thread/1', { includeTools: false, merge });
  assert.deepEqual(result.map(item => item.key), ['a', 'b']);
  assert.equal(requests.length, 2);
  assert.equal(requests[0].get('toolDetails'), '0');
  assert.equal(requests[0].get('tail'), '1');
});

test('a generation change while paging restarts once from the latest history', async () => {
  let calls = 0;
  const api = async path => {
    calls++;
    const cursor = new URL(path, 'https://mira.test').searchParams.get('cursor');
    if (calls === 1) return { generation: 1, trace: [{ key: 'old' }], nextCursor: 'c' };
    if (calls === 2) return { generation: 2, trace: [], nextCursor: null };
    return { generation: 2, trace: [{ key: cursor ?? 'new' }], nextCursor: null };
  };
  const result = await collectCodexTranscript(api, 't', { includeTools: true, merge: (a, b) => [...a, ...b] });
  assert.deepEqual(result.map(item => item.key), ['new']);
});

test('image references resolve to data URLs and skip failures', async () => {
  const api = async path => {
    if (path === '/bad') throw new Error('gone');
    return { url: path === '/ok' ? png : 'https://example.test/x.png' };
  };
  const images = await collectImages(api, [
    { key: 'a', kind: 'image', image: { href: '/ok' } },
    { key: 'b', kind: 'image', image: { href: '/bad' } },
    { key: 'c', kind: 'image', image: { href: '/remote' } },
    { key: 'd', kind: 'image', image: { url: png } },
  ]);
  assert.deepEqual([...images], [['a', png]]);
});

test('Mermaid placeholders become light and dark images; failures keep the source', async () => {
  const token = 'mira-diagram-test';
  const html = `<!--${token}:0--><pre><code class="language-mermaid">graph TD; A--&gt;B</code></pre>` +
    `<!--${token}:1--><pre><code class="language-mermaid">broken</code></pre>`;
  const diagrams = new Map([['graph TD; A-->B', 0], ['broken', 1]]);
  const rendered = [];
  const render = async (source, theme) => {
    rendered.push([source, theme.darkMode]);
    if (source === 'broken') throw new Error('parse error');
    return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(`<svg data-dark='${theme.darkMode}'/>`)}`;
  };
  const result = await embedDiagrams(html, { diagrams, token }, render);
  assert.deepEqual(rendered, [['graph TD; A-->B', false], ['graph TD; A-->B', true], ['broken', false]]);
  assert.match(result, /<figure class="diagram"><img class="diagram-light" src="data:image\/svg\+xml;charset=utf-8,[^"]*false[^"]*"/);
  assert.match(result, /<img class="diagram-dark" src="[^"]*true[^"]*"/);
  assert.match(result, /<details><summary>查看图表源码<\/summary><pre><code class="language-mermaid">graph TD; A--&gt;B<\/code><\/pre><\/details>/);
  assert.match(result, /<\/figure><pre><code class="language-mermaid">broken<\/code><\/pre>$/, 'a failed diagram stays as visible source');
  assert.doesNotMatch(result, /<!--/);
});

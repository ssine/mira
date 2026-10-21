import assert from 'node:assert/strict';
import test from 'node:test';
import { marked } from '../node/internal/webassets/web/markdown.js';
import { marked as commonmark } from '../node/internal/webassets/web/vendor/marked.js';

test('Chinese punctuation closes bold before adjacent Chinese or Latin text', () => {
  for (const [label, body] of [
    ['Review 时还需注意一处：', '你贴出的回复尚未明确。'],
    ['主送：', 'vincesyao；mikowang（王宇璐）'],
    ['抄送：', 'nicocheng（程思敏）；sophiahe（贺菲娅）'],
    ['说明。', '继续正文'],
    ['说明（新增）', '继续正文'],
    ['“中文”', '继续正文'],
  ]) {
    assert.equal(marked.parse(`**${label}**${body}`), `<p><strong>${label}</strong>${body}</p>\n`);
  }
  assert.equal(marked.parse('前文**“中文”**正文'), '<p>前文<strong>“中文”</strong>正文</p>\n');
});

test('nested emphasis and code retain their tokens inside CJK bold', () => {
  assert.equal(marked.parse('**说明 *重点* `a**b`：**正文'),
    '<p><strong>说明 <em>重点</em> <code>a**b</code>：</strong>正文</p>\n');
  assert.equal(marked.parse('***说明：***正文'), '<p><em><strong>说明：</strong></em>正文</p>\n');
});

test('literal asterisks and ordinary GFM keep their existing interpretation', () => {
  for (const source of [
    '`**主送：**账号`',
    '```md\n**主送：**账号\n```',
    '    **主送：**账号',
    '\\*\\*主送：\\*\\*账号',
    '**未闭合：正文',
    '** 空白：**正文',
    '**主送： **正文',
    '**Label:**value',
    'An **important** word, *emphasis*, and snake_case_identifiers.',
    '- [x] Done\n- [ ] Pending\n\n~~old~~ new',
    '| First | Second |\n|---|---|\n| **Bold** | `code` |',
  ]) assert.equal(marked.parse(source), commonmark.parse(source), source);
});

test('document blocks and per-render link rewriting work with CJK bold', () => {
  const source = [
    '# 邮件草稿', '**主送：**account', '**抄送：**reviewer',
    '> **说明：**新增权限', '---',
    '| 部门 | 接口人 |\n|---|---|\n| **部门：**AI Infra | account |',
    '**Review 时还需注意一处：**正文',
    '**附件 [文档](notes.md)：**查看',
  ].join('\n\n');
  const links = [];
  const html = marked.parse(source, { walkTokens(token) {
    if (token.type === 'link') { links.push(token.href); token.href = '#file-reference'; }
  } });
  for (const label of ['主送：', '抄送：', '说明：', '部门：', 'Review 时还需注意一处：']) {
    assert.ok(html.includes(`<strong>${label}</strong>`), label);
  }
  assert.match(html, /<h1>邮件草稿<\/h1>/);
  assert.match(html, /<blockquote>/);
  assert.match(html, /<table>/);
  assert.deepEqual(links, ['notes.md']);
  assert.match(html, /<strong>附件 <a href="#file-reference">文档<\/a>：<\/strong>查看/);
  assert.match(marked.parse('[文档](notes.md)'), /href="notes.md"/);
});

// Exercise the shipped parser and the actual file reader in a real browser.
export async function runMarkdownChecks() {
  const { marked } = await import('/markdown.js');
  const { FileReader } = await import('/file-preview.js');
  const { default: purify } = await import('/vendor/dompurify.js');
  const assert = (ok, message) => { if (!ok) throw new Error(message); };
  const source = [
    '# 邮件草稿', '**主送：**account；reviewer', '**抄送：**finance',
    '**Review 时还需注意一处：**你贴出的回复。',
    '| 部门 | 接口人 |\n|---|---|\n| **部门：**AI Infra | account |',
    '`**主送：**account`', '```md\n**抄送：**finance\n```',
    '<img src="data:," onerror="alert(1)">',
    '[unsafe](javascript:alert(1))',
  ].join('\n\n');
  const conversation = document.createElement('article');
  conversation.className = 'markdown-body';
  const host = document.createElement('section');
  document.body.append(conversation, host);
  const reader = new FileReader(host);
  reader.body = document.createElement('div');
  host.append(reader.body);
  reader.text = source;
  reader.markdown = true;
  reader.baseLine = 1;
  reader.ref = { nodeId: 'fixture', path: '/work/draft.md' };
  try {
    conversation.innerHTML = purify.sanitize(marked.parse(source));
    reader.renderText();
    for (const root of [conversation, host.querySelector('.file-markdown')]) {
      const bold = [...root.querySelectorAll('strong')].map(el => el.textContent);
      assert(JSON.stringify(bold) === JSON.stringify(['主送：', '抄送：', 'Review 时还需注意一处：', '部门：']), 'all CJK labels render bold');
      assert(Number(getComputedStyle(root.querySelector('strong')).fontWeight) > Number(getComputedStyle(root).fontWeight), 'bold is visually emphasized');
      assert(root.querySelector('table'), 'GFM table survives');
      assert(root.querySelector('pre code').textContent.trim() === '**抄送：**finance', 'fenced code stays literal');
      assert(root.querySelector('p code').textContent === '**主送：**account', 'inline code stays literal');
      assert(!root.querySelector('[onerror], a[href^="javascript:"]'), 'sanitization remains active');
    }
    reader.source = true;
    reader.renderText();
    assert(!host.querySelector('.file-markdown'), 'source view hides rendered output');
    assert(host.querySelector('.file-source').textContent.includes('**主送：**account'), 'source preserves original Markdown');
    reader.split = true;
    reader.renderText();
    assert(host.querySelector('.file-markdown strong').textContent === '主送：', 'split view also renders CJK bold');
    assert(reader.text === source, 'rendering never rewrites the file source');
    return { passed: true, cases: ['conversation Markdown', 'file reader', 'CJK bold', 'GFM table', 'literal code', 'sanitization', 'source and split views'] };
  } finally {
    conversation.remove();
    host.remove();
  }
}

// Standalone runner: node tests/markdown_browser.mjs [playwright-module]
if (typeof window === 'undefined') {
  const { default: assert } = await import('node:assert/strict');
  const { readFile } = await import('node:fs/promises');
  const { createServer } = await import('node:http');
  const root = new URL('../', import.meta.url);
  const source = await readFile(new URL('node/internal/webassets/webassets.go', root), 'utf8');
  const csp = source.match(/const contentSecurityPolicy = "([^"]+)"/)[1];
  const server = createServer(async (request, response) => {
    try {
      const path = new URL(request.url, 'http://localhost').pathname;
      const file = path === '/tests/markdown_browser.mjs' ? new URL('.' + path, root)
        : new URL('node/internal/webassets/web' + path, root);
      const data = path === '/' ? '<!doctype html><html data-theme="light"><meta charset="utf-8"><link rel="stylesheet" href="/styles.css"><body></body></html>' : await readFile(file);
      response.writeHead(200, { 'Content-Type': path === '/' ? 'text/html' : path.endsWith('.css') ? 'text/css' : 'text/javascript', 'Content-Security-Policy': csp });
      response.end(data);
    } catch { response.writeHead(404); response.end(); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let browser;
  try {
    const { chromium } = await import(process.argv[2] ?? 'playwright');
    browser = await chromium.launch({ headless: true });
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => {
      if (message.type() === 'error' && /content security policy/i.test(message.text())) errors.push(message.text());
    });
    await page.goto(`http://127.0.0.1:${server.address().port}/`);
    const result = await page.evaluate(async () => (await import('/tests/markdown_browser.mjs')).runMarkdownChecks());
    assert.equal(result.passed, true);
    assert.deepEqual(errors, []);
    console.log('Markdown browser checks passed:', result.cases.join(', '));
  } finally {
    await browser?.close();
    await new Promise(resolve => server.close(resolve));
  }
}

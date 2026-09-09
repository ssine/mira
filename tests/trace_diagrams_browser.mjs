// Run in a real browser on the Mira origin, e.g. import this module from a
// disposable static fixture with the production CSP and call runDiagramChecks().
export async function runDiagramChecks() {
  const { decorateTraceDiagrams } = await import('/trace-diagrams.js');
  const { marked } = await import('/vendor/marked.js');
  const { default: purify } = await import('/vendor/dompurify.js');
  const assert = (ok, message) => { if (!ok) throw new Error(message); };
  const waitFor = async (predicate) => {
    const end = Date.now() + 20000;
    while (!predicate()) {
      if (Date.now() > end) throw new Error('Diagram rendering timed out');
      await new Promise(resolve => setTimeout(resolve, 50));
    }
  };
  const root = document.createElement('article');
  root.className = 'markdown-body';
  root.style.width = '320px';
  document.body.append(root);
  const originalTheme = document.documentElement.dataset.theme;
  const graph = 'flowchart TD\n A["服务组：生命周期"] --> B["按服务执行、传文件、查状态"]\n A -.未来扩展.-> C["其他 Provider"]';
  const markdown = ['Normal prose.', '```mermaid\n' + graph + '\n```',
    '```flowchart\nflowchart LR\n A[Start] --> B[End]\n```',
    '```mermaid\nflowchart TD\n A[broken\n```',
    '```js\nconst ordinary = true;\n```',
    '```mermaid\nflowchart TD\n A["<img src=x onerror=alert(1)>"] --> B[Safe]\n click B "javascript:alert(1)"\n```',
    '```mermaid\nflowchart TD\n X[After error] --> Y[Still works]\n```'].join('\n\n');
  try {
    root.innerHTML = purify.sanitize(marked.parse(markdown));
    decorateTraceDiagrams(root);
    decorateTraceDiagrams(root); // Idempotent on an already decorated body.
    assert(root.querySelectorAll('.trace-diagram').length === 5, 'fence selection / duplicate rendering');
    await waitFor(() => root.querySelectorAll('.trace-diagram-preview img').length === 4);
    assert(root.querySelector('.language-js').textContent.includes('ordinary'), 'ordinary code is preserved');
    const figures = root.querySelectorAll('.trace-diagram');
    assert(figures[0].querySelector('code').textContent.trim() === graph, 'original source is preserved');
    assert(!figures[0].querySelector('details').open, 'successful diagram source starts collapsed');
    assert(figures[2].querySelector('details').open, 'broken diagram exposes its source');
    assert(figures[2].textContent.includes('暂时无法渲染'), 'broken diagram has a readable fallback');
    assert(root.querySelectorAll('svg, script, iframe, a[href^="javascript:"]').length === 0, 'diagram active content stays outside app DOM');
    assert(!document.querySelector('[id^="dmira-diagram"]'), 'no Mermaid scratch DOM remains');
    assert(root.scrollWidth <= 322, 'wide graphs scroll inside the diagram');
    figures[0].querySelector('.trace-diagram-zoom').click();
    assert(figures[0].querySelector('.trace-diagram-zoom').getAttribute('aria-pressed') === 'true', 'original size can be selected');
    assert(root.scrollWidth <= 322, 'original size scrolls locally');
    figures[0].querySelector('.trace-diagram-zoom').click();
    const firstImage = figures[0].querySelector('img');
    document.documentElement.dataset.theme = originalTheme === 'dark' ? 'light' : 'dark';
    await waitFor(() => figures[0].querySelector('img') !== firstImage);
    assert(figures[0].querySelector('img').naturalWidth > 0, 'theme change renders a decoded image');
    const stale = document.createElement('div');
    stale.innerHTML = purify.sanitize(marked.parse('```mermaid\n' + graph + '\n```'));
    root.append(stale);
    decorateTraceDiagrams(stale);
    stale.replaceChildren(); // Simulate a streamed update/thread switch before render completes.
    await new Promise(resolve => setTimeout(resolve, 100));
    assert(stale.childNodes.length === 0, 'stale asynchronous render does not restore old content');
    return { passed: true, cases: ['Chinese flowchart', 'flowchart fence', 'ordinary code', 'error recovery', 'source', 'SVG isolation', 'narrow layout', 'theme change', 'stale render'] };
  } finally {
    root.remove();
    if (originalTheme === undefined) delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = originalTheme;
  }
}

// Standalone runner: node tests/trace_diagrams_browser.mjs [playwright-module]
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
      const file = path === '/tests/trace_diagrams_browser.mjs' ? new URL('.' + path, root)
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
    const result = await page.evaluate(async () => (await import('/tests/trace_diagrams_browser.mjs')).runDiagramChecks());
    assert.equal(result.passed, true);
    assert.deepEqual(errors, []);
    console.log('Diagram browser checks passed:', result.cases.join(', '));
  } finally {
    await browser?.close();
    await new Promise(resolve => server.close(resolve));
  }
}

import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// The UI module imports browser vendor assets. Exercise its path boundary in
// isolation without replacing DOM, network, decoding or rendering behavior.
const source = fs.readFileSync(new URL('../server/public/file-preview.js', import.meta.url), 'utf8')
  .replace(/^import .*;\n/gm, '').replace(/^export /gm, '');
const context = vm.createContext({ URL, URLSearchParams });
vm.runInContext(source, context);

test('document-relative paths preserve native roots and decode URL escapes once', () => {
  for (const [path, link, expected] of [
    ['/work/a #b.md', 'img%2520.png', '/work/img%20.png'],
    ['/work/docs/a.md', '../图片.png', '/work/图片.png'],
    ['C:\\work\\a.md', 'img.png', 'C:\\work\\img.png'],
    ['C:\\work\\a.md', '/img.png', 'C:\\img.png'],
    ['\\\\server\\share\\docs\\a.md', 'img.png', '\\\\server\\share\\docs\\img.png'],
    ['\\\\server\\share\\docs\\a.md', '/img.png', '\\\\server\\share\\img.png'],
  ]) assert.equal(context.relativeResource({ nodeId:'original', path }, link).path, expected);
  assert.equal(vm.runInContext("parentPath('C:\\\\a.md')", context), 'C:\\');
});

test('line references and ZIP document navigation retain the resource origin', () => {
  const ref = context.relativeResource({ nodeId:'original', path:'/work/a.md' }, 'code.go:42:3');
  assert.equal(ref.path, '/work/code.go');
  assert.equal(ref.line, 42);
  assert.equal(ref.nodeId, 'original');
  const entry = context.relativeResource({ nodeId:'original', path:'/work/bundle.zip', archive:true,
    archiveEntry:'docs/a.md', entryId:7, archiveVersion:'version' }, '../assets/pixel.png');
  assert.equal(entry.path, '/work/bundle.zip');
  assert.equal(entry.archiveEntry, 'assets/pixel.png');
  assert.equal(entry.entryId, undefined);
  assert.equal(entry.archiveVersion, 'version');
  for (const external of ['https://example.test/a.md', '//example.test/a.md', '#title', 'javascript:alert(1)']) {
    assert.equal(context.relativeResource(ref, external), null);
  }
});

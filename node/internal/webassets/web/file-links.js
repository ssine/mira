// File references in Agent output. Text that looks like a path becomes a link
// only after the Node that produced the output confirms the path exists there;
// the Node also resolves "~" and paths relative to the turn's working directory.
import { fileActions, fileName, runFileAction, workspaceURL } from '/file-preview.js';

const probeBatch = 64;          // matches the Node and Server bound for one probe
const positiveLifetime = 10 * 60_000;
const negativeLifetime = 2 * 60_000;
const cacheLimit = 4000;

const absolutePath = input => input.startsWith('/') || /^[A-Za-z]:[\\/]/.test(input) || input.startsWith('\\\\');
const homePath = input => input === '~' || /^~[\\/]/.test(input);

// Split a written reference into the path and an optional source position.
// `url` marks Markdown link targets, which are percent-encoded; code and prose
// are taken literally. Web addresses and in-page anchors are not files.
export function parseFileReference(value, { url = false } = {}) {
  let reference = String(value ?? '').trim();
  if (!reference || reference.startsWith('#') || /^(?:https?|mailto|tel|data|blob|javascript):/i.test(reference)) return null;
  if (url) { try { reference = decodeURIComponent(reference); } catch { /* retain malformed percent escapes */ } }
  if (reference.startsWith('<') && reference.endsWith('>')) reference = reference.slice(1, -1).trim();
  const lineMatch = reference.match(/(?:#L|:)(\d+)(?::(\d+))?$/);
  const line = lineMatch ? Number(lineMatch[1]) : null;
  const column = lineMatch?.[2] ? Number(lineMatch[2]) : null;
  if (lineMatch) reference = reference.slice(0, -lineMatch[0].length);
  if (/^[a-z][a-z0-9+.-]*:/i.test(reference) && !/^file:/i.test(reference) && !/^[A-Za-z]:[\\/]/.test(reference)) return null;
  if (/^file:/i.test(reference)) {
    try {
      const parsed = new URL(reference);
      reference = decodeURIComponent(parsed.pathname);
      if (parsed.hostname && parsed.hostname !== 'localhost') reference = `\\\\${parsed.hostname}${reference.replaceAll('/', '\\')}`;
    } catch { return null; }
  }
  // Markdown links can contain a URL-style Windows drive path without file:.
  // Node file operations need the native form, e.g. C:/Reports/file.json.
  if (/^\/[A-Za-z]:[\\/]/.test(reference)) reference = reference.slice(1);
  if (!reference || reference.length > 4096 || /[\0\r\n]/.test(reference)) return null;
  return { input: reference, line, column };
}

// The shape of a path, without knowing whether it exists.
export function pathLike(input) {
  return absolutePath(input) || homePath(input) || input.startsWith('./') || input.startsWith('../') ||
    input.includes('/') || input.includes('\\') || /\.[\p{L}][\p{L}\p{N}._-]{0,15}$/u.test(input);
}

// Inline code is often a command or an expression. Skip what cannot be a
// single path so it does not spend a probe.
function codeCandidate(text) {
  if (text.length > 512 || /[\t<>|"*?;`${}]|\s-|:\/\/|^\s|\s$|^-/.test(text)) return null;
  const parsed = parseFileReference(text);
  return parsed && pathLike(parsed.input) && /[\p{L}\p{N}]/u.test(parsed.input) ? parsed : null;
}

// Bare prose needs stronger evidence than code: a leading "/", "~/", "./",
// "../" or drive, or a relative path that ends in a file extension, ends in a
// separator, or is at least three segments deep. "and/or" and "TCP/IP" are not
// candidates. Only ASCII path characters are scanned so a path directly beside
// CJK text keeps its own boundaries.
export function scanTextForPaths(text) {
  const found = [];
  for (const match of text.matchAll(/[\w.+@~:#\\\/-]+/g)) {
    const lead = match[0].match(/^[:#+-]+/)?.[0].length ?? 0;
    const token = match[0].slice(lead).replace(/[.:#+@~-]+$/, '');
    if (!/[\\/]/.test(token) || token.includes('://') || token.startsWith('//')) continue;
    const parsed = parseFileReference(token);
    if (!parsed || !/[A-Za-z]/.test(parsed.input)) continue;
    const input = parsed.input;
    const segments = input.split(/[\\/]/).filter(Boolean);
    const anchored = absolutePath(input) || homePath(input) || /^\.{1,2}[\\/]/.test(input);
    const relative = /[\\/]$/.test(input) || segments.length >= 3 || segments.length === 2 && /\.[A-Za-z][\w-]{0,15}$/.test(segments[1]);
    if (!(anchored ? segments.length >= 1 : relative)) continue;
    const start = match.index + lead;
    found.push({ start, end: start + token.length, ...parsed });
  }
  return found;
}

// Without a Node to ask, resolve the way the console always has: absolute
// paths as written, relative ones against the recorded working directory.
export function resolveWithoutNode(input, cwd) {
  if (absolutePath(input)) return input;
  if (homePath(input) || !cwd) return null;
  const separator = /^[A-Za-z]:\\/.test(cwd) || (cwd.includes('\\') && !cwd.includes('/')) ? '\\' : '/';
  const relative = input.replace(/[\\/]/g, separator);
  return cwd.endsWith(separator) ? cwd + relative : cwd + separator + relative;
}

// What a plain click does. A source position always reads the file; otherwise
// websites and containers open in their own tab, like any link would.
export function defaultFileAction(path, type, line) {
  if (line) return 'preview';
  if (type === 'directory') return 'workspace';
  if (/\.html?$/i.test(path)) return 'site';
  if (/\.zip$/i.test(path)) return 'workspace';
  if (type) return 'preview';
  return /[\\/]$/.test(path) || !path.split(/[\\/]/).filter(Boolean).at(-1)?.includes('.') ? 'workspace' : 'preview';
}

// Batches existence checks per Node and working directory, and remembers the
// answers briefly so re-rendering a message does not ask again.
export class FileProbe {
  constructor(post) { this.post = post; this.cache = new Map(); this.groups = new Map(); this.unavailable = new Map(); }
  key(context, input) { return JSON.stringify([context.nodeId, context.executionContext ?? '', context.userSessionId ?? '', context.cwd ?? '', input]); }
  cached(context, input) {
    const entry = this.cache.get(this.key(context, input));
    if (!entry) return undefined;
    if (Date.now() - entry.at > (entry.value.exists ? positiveLifetime : negativeLifetime)) return undefined;
    return entry.value;
  }
  forget(context, input) { this.cache.delete(this.key(context, input)); }
  // Resolves to the Node's answer, or null when the Node cannot be asked now.
  request(context, input) {
    const until = this.unavailable.get(context.nodeId);
    if (until && until > Date.now()) return Promise.resolve(null);
    const groupKey = this.key(context, '');
    let group = this.groups.get(groupKey);
    if (!group) {
      group = { context, waiting: new Map() };
      this.groups.set(groupKey, group);
      setTimeout(() => { this.groups.delete(groupKey); void this.flush(group); }, 30);
    }
    let waiter = group.waiting.get(input);
    if (!waiter) { waiter = {}; waiter.promise = new Promise(resolve => { waiter.resolve = resolve; }); group.waiting.set(input, waiter); }
    return waiter.promise;
  }
  async flush({ context, waiting }) {
    const inputs = [...waiting.keys()];
    for (let offset = 0; offset < inputs.length; offset += probeBatch) {
      const chunk = inputs.slice(offset, offset + probeBatch);
      let entries = null;
      try {
        const result = await this.post('/v1/files/probe', { nodeId: context.nodeId, paths: chunk,
          ...(context.cwd ? { cwd: context.cwd } : {}),
          ...(context.executionContext ? { executionContext: context.executionContext } : {}),
          ...(context.userSessionId != null ? { userSessionId: context.userSessionId } : {}) });
        entries = new Map((result.entries ?? []).map(entry => [entry.input, entry]));
      } catch (error) {
        // An older or offline Node cannot answer; stop asking it for a while
        // and let the references fall back to their written form.
        this.unavailable.set(context.nodeId, Date.now() + (error.code === 'capability_unavailable' ? 10 * 60_000 : 15_000));
      }
      for (const input of chunk) {
        const value = entries?.get(input) ?? null;
        if (value) {
          const key = this.key(context, input);
          this.cache.delete(key); this.cache.set(key, { value, at: Date.now() });
        }
        waiting.get(input).resolve(value);
      }
    }
    while (this.cache.size > cacheLimit) this.cache.delete(this.cache.keys().next().value);
  }
}

const element = (tag, className, text) => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
};

export class FileLinks {
  // post(path, body): the console's CSRF-carrying JSON request.
  // node(id): the registered Node, or undefined. nodeName(id): its display name.
  // preview(ref, context): open the reader beside the conversation.
  constructor({ post, node, nodeName, preview, notify }) {
    Object.assign(this, { post, node, nodeName, preview, notify });
    this.probe = new FileProbe(post);
    this.suppressClickUntil = 0;
  }

  // A Node the console has not loaded yet is asked anyway: the Server answers
  // for it, and a Node without the capability is then left alone for a while.
  canProbe(context) {
    if (!context.nodeId) return false;
    const node = this.node(context.nodeId);
    return !node || node.capabilities?.fileProbeV1 === true;
  }

  // `references` holds the Markdown targets set aside before sanitizing, keyed
  // by their placeholder href.
  decorate(root, references, context) {
    const candidates = [];
    for (const anchor of root.querySelectorAll('a[href]')) {
      const href = anchor.getAttribute('href');
      const parsed = references.get(href) ?? this.linkReference(href);
      if (!parsed) { anchor.target = '_blank'; anchor.rel = 'noopener noreferrer'; continue; }
      anchor.setAttribute('href', '#');
      candidates.push({ node: anchor, parsed, explicit: true });
    }
    for (const image of root.querySelectorAll('img[src]')) {
      const src = image.getAttribute('src');
      const parsed = references.get(src) ?? this.linkReference(src);
      if (!parsed) continue;
      const anchor = element('a', 'node-file-image-link', `▧ ${image.alt || parsed.input.split(/[\\/]/).filter(Boolean).at(-1) || parsed.input}`);
      anchor.setAttribute('href', '#');
      image.replaceWith(anchor);
      candidates.push({ node: anchor, parsed, explicit: true });
    }
    for (const code of root.querySelectorAll('code')) {
      if (code.closest('pre, a, button')) continue;
      const parsed = codeCandidate(code.textContent);
      if (parsed) candidates.push({ node: code, parsed, explicit: false, code: true });
    }
    const probing = this.canProbe(context);
    if (probing) this.collectText(root, candidates);
    // One message asks about a bounded number of distinct references.
    const asked = new Set();
    for (const candidate of candidates) {
      const { parsed } = candidate;
      if (!probing || (!asked.has(parsed.input) && asked.size >= probeBatch)) {
        this.applyWritten(candidate, context);
        continue;
      }
      asked.add(parsed.input);
      const known = this.probe.cached(context, parsed.input);
      if (known) { this.applyProbed(candidate, known, context); continue; }
      // Until the Node answers, an explicit link keeps working as written.
      if (candidate.explicit) this.applyWritten(candidate, context);
      void this.probe.request(context, parsed.input).then(result => {
        if (!candidate.node.isConnected) return;
        if (result) this.applyProbed(candidate, result, context);
        else this.applyWritten(candidate, context);
      });
    }
  }

  linkReference(href) {
    const parsed = parseFileReference(href, { url: true });
    return parsed && pathLike(parsed.input) ? parsed : null;
  }

  collectText(root, candidates) {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
      acceptNode: node => node.parentElement.closest('a, code, pre, button, svg, math, .trace-diagram') ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT,
    });
    const texts = [];
    while (walker.nextNode()) texts.push(walker.currentNode);
    for (const text of texts) {
      const matches = scanTextForPaths(text.data);
      // Split from the end so earlier offsets stay valid.
      for (const match of matches.reverse()) {
        const tail = text.splitText(match.start);
        tail.splitText(match.end - match.start);
        const span = element('span', 'node-file-candidate', tail.data);
        tail.replaceWith(span);
        candidates.push({ node: span, parsed: match, explicit: false });
      }
    }
  }

  // Use the reference as written: there is no Node to confirm it with. Only a
  // Markdown link, or code that contains a separator, is clearly a path.
  applyWritten(candidate, context) {
    const { parsed, node } = candidate;
    const path = resolveWithoutNode(parsed.input, context.cwd);
    if (path && (candidate.explicit || candidate.code && /[\\/]/.test(parsed.input))) { this.link(candidate, { path, type: '' }, context); return; }
    if (node.tagName !== 'A' || node.dataset.nodeFilePath) return;
    // A relative target must not navigate the console itself.
    node.removeAttribute('href');
    node.classList.add('node-file-unresolved');
    node.title = '无法确定这个相对路径所属的 Node 或工作目录';
  }

  applyProbed(candidate, result, context) {
    if (result.exists) { this.link(candidate, { path: result.path, type: result.type }, context); return; }
    if (!candidate.explicit) { this.unlink(candidate); return; }
    // A Markdown link stays a link; say that its target is not there.
    const path = result.path ?? resolveWithoutNode(candidate.parsed.input, context.cwd) ?? candidate.parsed.input;
    const anchor = this.link(candidate, { path, type: '' }, context);
    anchor.dataset.nodeFileMissing = 'true';
    anchor.dataset.nodeFileInput = candidate.parsed.input;
    anchor.classList.add('node-file-missing');
    anchor.setAttribute('href', '#');
    anchor.removeAttribute('target');
    anchor.title = `在 ${this.nodeName(context.nodeId)} 上没有找到 ${path} · 点击重新检查`;
  }

  unlink(candidate) {
    const anchor = candidate.node;
    if (anchor.tagName !== 'A' || !candidate.original) return;
    anchor.replaceWith(candidate.original);
    candidate.node = candidate.original;
  }

  link(candidate, { path, type }, context) {
    let anchor = candidate.node;
    if (anchor.tagName !== 'A') {
      anchor = element('a', anchor.tagName === 'CODE' ? 'node-file-code-link' : '');
      candidate.original = candidate.node;
      candidate.node.replaceWith(anchor);
      anchor.append(candidate.original.tagName === 'CODE' ? candidate.original : candidate.original.textContent);
      candidate.node = anchor;
    }
    const { line, column } = candidate.parsed;
    anchor.classList.add('node-file-link');
    anchor.classList.remove('node-file-missing', 'node-file-unresolved');
    delete anchor.dataset.nodeFileMissing;
    anchor.dataset.nodeFilePath = path;
    if (type) anchor.dataset.nodeFileType = type; else delete anchor.dataset.nodeFileType;
    if (line) anchor.dataset.nodeFileLine = String(line);
    if (column) anchor.dataset.nodeFileColumn = String(column);
    anchor.dataset.fileContext = JSON.stringify(context);
    const ref = this.reference(anchor);
    const action = defaultFileAction(path, type, line);
    // The link is a real address: modified clicks and the default action for
    // websites and directories open it in a new tab without a script popup.
    if (ref.nodeId) {
      anchor.href = workspaceURL(ref, { site: action === 'site', sourceThreadId: context.sourceThreadId ?? '', line });
      anchor.target = '_blank'; anchor.rel = 'noopener';
    } else anchor.setAttribute('href', '#');
    const kind = type === 'directory' ? '目录' : type ? '文件' : '路径';
    anchor.title = `${kind} · ${context.nodeId ? this.nodeName(context.nodeId) : '请选择所属 Node'} · ${path}${type ? ' · 右键或长按查看更多操作' : ''}`;
    return anchor;
  }

  reference(anchor) {
    const context = JSON.parse(anchor.dataset.fileContext || '{}');
    return {
      context,
      nodeId: context.nodeId, path: anchor.dataset.nodeFilePath, type: anchor.dataset.nodeFileType || '',
      ...(anchor.dataset.nodeFileLine ? { line: Number(anchor.dataset.nodeFileLine) } : {}),
      ...(context.executionContext ? { executionContext: context.executionContext } : {}),
      ...(context.userSessionId != null ? { userSessionId: context.userSessionId } : {}),
    };
  }

  // Returns whether the event was a file reference; the caller stops there.
  handleClick(event) {
    const anchor = event.target.closest?.('[data-node-file-path]');
    if (!anchor) return false;
    if (Date.now() < this.suppressClickUntil) { event.preventDefault(); return true; }
    const ref = this.reference(anchor);
    if (anchor.dataset.nodeFileMissing) { event.preventDefault(); void this.recheck(anchor, ref); return true; }
    if (event.altKey && this.node(ref.nodeId)?.capabilities?.desktopRevealV1 === true) {
      event.preventDefault(); void this.run('reveal', ref); return true;
    }
    const modified = event.ctrlKey || event.metaKey || event.shiftKey;
    if (!ref.nodeId || (!modified && defaultFileAction(ref.path, ref.type, ref.line) === 'preview')) {
      event.preventDefault(); this.preview(ref, ref.context);
    }
    return true;
  }

  async recheck(anchor, ref) {
    const input = anchor.dataset.nodeFileInput;
    if (!input || !this.canProbe(ref.context)) { this.preview(ref, ref.context); return; }
    this.probe.forget(ref.context, input);
    const result = await this.probe.request(ref.context, input);
    if (!anchor.isConnected) return;
    if (!result?.exists) { this.notify(`在 ${this.nodeName(ref.nodeId)} 上仍然没有找到 ${ref.path}`); return; }
    const candidate = { node: anchor, explicit: true, parsed: { input, line: ref.line ?? null, column: null } };
    this.link(candidate, { path: result.path, type: result.type }, ref.context);
    this.notify('文件已出现，可以打开了');
  }

  handleContextMenu(event) {
    const anchor = event.target.closest?.('[data-node-file-path]');
    if (!anchor || anchor.dataset.nodeFileMissing) return false;
    const ref = this.reference(anchor);
    if (!ref.nodeId) return false;
    event.preventDefault();
    if (this.menuOpen() && this.menuAnchor === anchor) return true;
    const box = anchor.getBoundingClientRect();
    // A keyboard context-menu key reports no pointer position.
    const pointer = event.clientX || event.clientY;
    this.openMenu(anchor, ref, pointer ? event.clientX : box.left, pointer ? event.clientY : box.bottom);
    return true;
  }

  // Touch has no secondary button; a held press opens the same menu.
  handlePointerDown(event) {
    if (event.pointerType !== 'touch') return;
    const anchor = event.target.closest?.('[data-node-file-path]');
    if (!anchor || anchor.dataset.nodeFileMissing) return;
    const ref = this.reference(anchor);
    if (!ref.nodeId) return;
    const { clientX, clientY } = event;
    const cancel = () => {
      clearTimeout(timer);
      for (const type of ['pointerup', 'pointercancel', 'pointermove', 'scroll']) window.removeEventListener(type, moved, true);
    };
    const moved = next => { if (next.type !== 'pointermove' || Math.hypot(next.clientX - clientX, next.clientY - clientY) > 10) cancel(); };
    const timer = setTimeout(() => {
      cancel();
      if (!anchor.isConnected) return;
      this.suppressClickUntil = Date.now() + 700;
      this.openMenu(anchor, ref, clientX, clientY);
    }, 480);
    for (const type of ['pointerup', 'pointercancel', 'pointermove', 'scroll']) window.addEventListener(type, moved, true);
  }

  actions(ref, surface) {
    const actions = fileActions(ref, { type: ref.type, node: this.node(ref.nodeId), nodeName: this.nodeName(ref.nodeId),
      sourceThreadId: ref.context?.sourceThreadId ?? '', surface });
    const primary = defaultFileAction(ref.path, ref.type, ref.line);
    for (const action of actions) action.primary = action.id === primary;
    return actions;
  }

  async run(id, ref) {
    if (id === 'preview') this.preview(ref, ref.context);
    else await runFileAction(id, ref, { post: this.post, notify: this.notify, nodeName: this.nodeName(ref.nodeId) });
  }

  menuOpen() { return Boolean(this.menu?.matches(':popover-open')); }
  closeMenu(restoreFocus = false) {
    if (!this.menuOpen()) return;
    this.menu.hidePopover();
    if (restoreFocus) this.menuAnchor?.focus({ preventScroll: true });
  }

  openMenu(anchor, ref, x, y) {
    if (!this.menu) {
      this.menu = element('div', 'file-link-menu');
      // Manual: the press that opens the menu (a held touch, or a secondary
      // button that reports on press) must not count as an outside click.
      this.menu.popover = 'manual';
      this.menu.setAttribute('role', 'menu');
      this.menu.setAttribute('aria-label', '文件操作');
      this.menu.addEventListener('keydown', event => {
        // Escape belongs to this menu alone, not to what is open behind it.
        if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); this.closeMenu(true); return; }
        if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
        event.preventDefault();
        const items = [...this.menu.querySelectorAll('[role=menuitem]')];
        const index = items.indexOf(document.activeElement);
        const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1
          : (index + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
        items[next]?.focus();
      });
      document.addEventListener('pointerdown', event => { if (this.menuOpen() && !this.menu.contains(event.target)) this.closeMenu(); }, true);
      window.addEventListener('scroll', event => { if (!this.menu.contains(event.target)) this.closeMenu(); }, true);
      window.addEventListener('resize', () => this.closeMenu());
      window.addEventListener('blur', event => { if (event.target === window) this.closeMenu(); });
      document.body.append(this.menu);
    }
    this.closeMenu();
    this.menuAnchor = anchor;
    const head = element('div', 'file-link-menu-head');
    head.append(element('strong', '', fileName(ref)), element('span', '', `${this.nodeName(ref.nodeId)} · ${ref.path}`));
    this.menu.replaceChildren(head);
    for (const action of this.actions(ref, 'menu')) {
      const item = element(action.href ? 'a' : 'button', `file-link-menu-item${action.primary ? ' primary-action' : ''}`, action.label);
      item.setAttribute('role', 'menuitem');
      item.dataset.fileAction = action.id;
      if (action.title) item.title = action.title;
      if (action.primary) item.append(element('span', 'file-link-menu-hint', '单击'));
      else if (action.id === 'reveal') item.append(element('span', 'file-link-menu-hint', 'Alt + 单击'));
      if (action.href) {
        item.href = action.href;
        if (action.newTab) { item.target = '_blank'; item.rel = 'noopener'; }
        if (action.download) item.download = action.download;
        item.addEventListener('click', () => this.closeMenu());
      } else {
        item.type = 'button';
        item.addEventListener('click', () => { this.closeMenu(); void this.run(action.id, ref); });
      }
      this.menu.append(item);
    }
    this.menu.showPopover();
    const box = this.menu.getBoundingClientRect();
    this.menu.style.left = `${Math.max(8, Math.min(x, innerWidth - box.width - 8))}px`;
    this.menu.style.top = `${Math.max(8, y + box.height + 8 > innerHeight ? y - box.height - 4 : y + 4)}px`;
    this.menu.querySelector('.primary-action, [role=menuitem]')?.focus({ preventScroll: true });
  }
}

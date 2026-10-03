import DOMPurify from '/vendor/dompurify.js';
import { marked } from '/vendor/marked.js';

const chunkBytes = 128 * 1024;
export const fileName = ref => (ref.archive ? ref.archiveEntry : ref.path).replace(/[\\/]+$/, '').split(/[\\/]/).at(-1) || ref.path;
export const parentPath = value => {
  const parent = value.replace(/[\\/][^\\/]*$/, '') || '/';
  return /^[A-Za-z]:$/.test(parent) ? parent + (value.includes('\\') ? '\\' : '/') : parent;
};
export function resourceURL(ref, action = 'content', extra = {}) {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries({ ...ref, ...extra })) {
    if (['nodeId', 'path', 'archive', 'archiveEntry', 'archiveVersion', 'entryId', 'executionContext', 'userSessionId', 'action', 'cursor', 'download'].includes(key) && value != null) query.set(key, String(value));
  }
  return `/v1/files/${action}?${query}`;
}
export function workspaceURL(ref, { site = false, sourceThreadId = '', line = null } = {}) {
  return `/files.html?${new URLSearchParams({ nodeId: ref.nodeId, path: ref.path, ...(ref.archive ? { archive: 'true', archiveEntry: ref.archiveEntry || '' } : {}), ...(ref.archiveVersion ? { archiveVersion:ref.archiveVersion } : {}), ...(ref.entryId != null ? { entryId: ref.entryId } : {}), ...(line ? { line } : {}), ...(ref.executionContext ? { executionContext: ref.executionContext } : {}), ...(ref.userSessionId != null ? { userSessionId: ref.userSessionId } : {}), ...(sourceThreadId ? { sourceThreadId } : {}), ...(site ? { view: 'site' } : {}) })}`;
}
export function relativeResource(ref, link) {
  if (!link || link.startsWith('#')) return null;
  const lineMatch = link.match(/(?:#L|:)(\d+)(?::(\d+))?$/);
  if (lineMatch) link = link.slice(0,-lineMatch[0].length);
  if (/^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(link)) return null;
  const pathname = link.split(/[?#]/)[0];
  const current = ref.archive ? ref.archiveEntry : ref.path;
  const windows = /^[A-Za-z]:[\\/]|^\\\\/.test(current);
  const unc = current.startsWith('\\\\');
  const normalized = current.replaceAll('\\', '/');
  const base = normalized.replace(/^\/+/, '').split('/').map(encodeURIComponent).join('/');
  let value;
  try { value = decodeURIComponent(new URL(pathname, `https://resource.invalid/${base}`).pathname); } catch { return null; }
  if (windows && /^[A-Za-z]:/.test(normalized) && pathname.startsWith('/')) value = '/' + normalized.slice(0,2) + value;
  if (unc && pathname.startsWith('/')) value = '/' + normalized.split('/').filter(Boolean).slice(0,2).join('/') + value;
  if (windows || ref.archive) value = value.slice(1);
  if (unc) value = '\\\\' + value;
  if (windows) value = value.replaceAll('/', current.includes('\\') ? '\\' : '/');
  return { ...ref, ...(ref.archive ? { archiveEntry: value, entryId: undefined } : { path: value }), line: lineMatch ? Number(lineMatch[1]) : undefined };
}
const el = (tag, cls, text) => { const node = document.createElement(tag); if (cls) node.className = cls; if (text != null) node.textContent = text; return node; };
const button = (text, action) => { const b = el('button', 'ghost', text); b.type = 'button'; b.addEventListener('click', action); return b; };
const pinExecutionIdentity = (ref, stat) => ({ ...ref,
  ...(['user','system'].includes(stat.executionContext) ? {executionContext:stat.executionContext} : {}),
  ...(stat.executionContext === 'user' && stat.userSessionId != null ? {userSessionId:stat.userSessionId} : {}) });

function highlightLine(row,line) {
  const tokens=/(\/\/.*$|#.*$)|("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')|\b(const|let|var|function|class|import|export|from|return|if|else|for|while|def|func|package|type|struct|public|private|true|false|null|nil|None|async|await|try|catch|throw)\b/g;
  let start=0;for(const match of line.matchAll(tokens)){row.append(document.createTextNode(line.slice(start,match.index)));row.append(el('span',match[1]?'syntax-comment':match[2]?'syntax-string':'syntax-keyword',match[0]));start=match.index+match[0].length;}row.append(document.createTextNode(line.slice(start)|| (line?'':' ')));
}

async function responseJSON(url, signal) { const r = await fetch(url, { signal, credentials: 'same-origin' }); const body = await r.json(); if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`); return body; }
export const resourceMeta = (ref, extra, signal) => responseJSON(resourceURL(ref, 'meta', extra), signal);

export class FileReader {
  constructor(host, { navigate = ref => this.open(ref), independent = ref => window.open(workspaceURL(ref), '_blank', 'noopener'), states = new Map() } = {}) {
    this.host = host; this.navigate = navigate; this.independent = independent; this.states = states; this.history = []; this.historyIndex = -1;
  }
  stateKey(ref) { return JSON.stringify([ref.nodeId, ref.path, ref.archiveEntry, ref.entryId, ref.executionContext, ref.userSessionId]); }
  rememberPosition() {
    // Disposable, bounded reader state preserves navigation without changing
    // the thread store or retaining arbitrarily large loaded documents.
    if (!this.ref || !this.stat || this.loading || !this.offset || !this.body?.getClientRects().length || this.text.length > 2 * 1024 * 1024) return;
    const key = this.stateKey(this.ref);
    this.states.delete(key);
    this.states.set(key, { text:this.text, offset:this.offset, baseLine:this.baseLine, decoder:this.decoder, etag:this.etag,
      stat:this.stat, source:this.source, split:this.split, top:this.body.scrollTop });
    while (this.states.size > 12) this.states.delete(this.states.keys().next().value);
  }
  dispose() {
    this.rememberPosition();
    this.controller?.abort(); for (const media of this.host.querySelectorAll('video,audio,img,iframe')) { media.removeAttribute('src'); if (media.load) media.load(); }
  }
  async open(ref, { remember = true, source = false } = {}) {
    this.dispose(); this.controller = new AbortController(); const signal = this.controller.signal;
    this.loading = false; this.stat = null; this.split = false; this.ref = { ...ref }; this.source = source || Boolean(ref.line); this.text = ''; this.baseLine = 1; this.offset = 0; this.decoder = new TextDecoder(); this.etag = null;
    if (remember) { this.history.splice(++this.historyIndex); this.history.push({ ...ref }); }
    this.host.replaceChildren(); this.host.classList.add('file-reader');
    const toolbar = el('div', 'file-toolbar');
    const back = button('←', () => this.travel(-1)); back.disabled = this.historyIndex <= 0; back.title = '返回上一文件';
    const forward = button('→', () => this.travel(1)); forward.disabled = this.historyIndex >= this.history.length - 1; forward.title = '下一文件';
    toolbar.append(back, forward, button('刷新', () => { const current=this.ref; this.dispose(); this.states.delete(this.stateKey(current)); this.stat=null; void this.open(current, { remember: false }); }), button('独立页面', () => this.independent(this.ref)), button('取消读取', () => this.controller.abort()));
    const download = el('a', 'ghost', '下载'); download.href = resourceURL(ref, 'content', { download: true }); download.download = fileName(ref); toolbar.append(download);
    this.status = el('p', 'file-status', `正在读取 · ${ref.nodeId}`); this.status.setAttribute('role', 'status');
    this.body = el('div', 'file-reader-body'); this.host.append(toolbar, this.status, this.body);
    try {
      this.stat = await resourceMeta(ref, {}, signal); if (signal.aborted) return;
      ref = pinExecutionIdentity(ref,this.stat); this.ref = ref; download.href = resourceURL(ref,'content',{download:true});
      this.status.textContent = `${fileName(ref)} · ${Number(this.stat.size || 0).toLocaleString()} 字节 · Node ${ref.nodeId} · 当前内容`;
      if (this.stat.type === 'directory' || (!ref.archive && /\.zip$/i.test(ref.path))) {
        this.body.append(button('打开文件工作区', () => this.independent(ref))); return;
      }
      const name = fileName(ref); const ext = name.split('.').at(-1).toLowerCase();
      const url = resourceURL(ref);
      const images = ['png','jpg','jpeg','gif','webp','avif','bmp','svg','ico'];
      const videos = ['mp4','webm','mov','mkv','m4v']; const audio = ['mp3','wav','ogg','m4a','aac','flac','opus'];
      if (images.includes(ext) && !this.source) {
        const image = el('img', 'file-image'); image.alt = name; image.src = url; image.loading = 'lazy'; this.body.append(image); let zoom = 1;
        const resize = scale => { zoom = scale; image.style.maxWidth = 'none'; image.style.width = `${zoom * 100}%`; };
        toolbar.append(button('−', () => resize(Math.max(.1, zoom / 1.25))), button('+', () => resize(Math.min(10, zoom * 1.25))), button('原始尺寸', () => { image.style.width = 'auto'; image.style.maxWidth = 'none'; })); image.onerror = () => this.showError(new Error('图片读取失败或客户端不支持此格式')); return;
      }
      if (videos.includes(ext) || audio.includes(ext)) {
        const media = el(videos.includes(ext) ? 'video' : 'audio', 'file-media'); media.controls = true; media.preload = 'metadata'; media.src = url; this.body.append(media);
        const speed = el('select'); speed.setAttribute('aria-label','播放速度'); for (const value of [.5,1,1.25,1.5,2]) { const o = el('option', '', `${value}×`); o.value = value; o.selected = value === 1; speed.append(o); } speed.onchange = () => { media.playbackRate = Number(speed.value); }; toolbar.append(speed); media.onerror = () => this.showError(new Error('媒体读取失败或客户端不支持此编码；可以下载查看')); return;
      }
      if (ext === 'pdf') { const frame = el('iframe','file-pdf'); frame.title = name; frame.setAttribute('sandbox',''); frame.src = url; this.body.append(frame); return; }
      if (['html','htm'].includes(ext) && !this.source) { this.body.append(button('打开网站', () => window.open(workspaceURL(ref, { site: true }), '_blank', 'noopener'))); }
      this.markdown = ['md','markdown','mdown'].includes(ext);
      const textTypes = ['txt','log','json','jsonl','ndjson','yaml','yml','toml','xml','html','htm','css','js','mjs','ts','tsx','jsx','go','rs','py','sh','zsh','bash','java','kt','nix','csv','sql','svg','ini','conf','diff','patch','c','h','cpp','hpp','cs','rb','php','vue','svelte','dockerfile'];
      if (!this.markdown && !textTypes.includes(ext) && name.includes('.')) { this.body.append(el('p', '', '此格式暂不支持内置预览。')); return; }
      if (this.markdown) {
        toolbar.append(button('阅读 / 原文', () => { this.source = !this.source; this.split = false; this.renderText(); }), button('分屏', () => { this.split = !this.split; this.renderText(); }));
      }
      this.search = el('input'); this.search.type = 'search'; this.search.placeholder = '搜索已加载内容'; this.search.setAttribute('aria-label','搜索已加载内容'); this.search.oninput = () => this.renderText(); toolbar.append(this.search);
      this.more = button('继续读取', () => this.loadMore().catch(e => this.showError(e))); this.host.append(this.more);
      const saved=this.states.get(this.stateKey(ref));
      if (saved && saved.stat.size === this.stat.size && saved.stat.modifiedAt === this.stat.modifiedAt && saved.stat.archiveVersion === this.stat.archiveVersion && (!ref.line || ref.line >= saved.baseLine)) {
        this.states.delete(this.stateKey(ref)); // Transfer the decoder's partial UTF-8 state to this reader.
        this.text=saved.text; this.offset=saved.offset; this.baseLine=saved.baseLine; this.decoder=saved.decoder; this.etag=saved.etag;
        this.source=Boolean(ref.line)||saved.source; this.split=saved.split; this.renderText(); this.body.scrollTop=saved.top;
        this.more.hidden=this.offset>=Number(this.stat.size);this.more.textContent=`继续读取 · ${this.offset.toLocaleString()} / ${Number(this.stat.size).toLocaleString()} 字节`;
      } else if (Number(this.stat.size) === 0) { this.more.hidden = true; this.renderText(); } else await this.loadMore();
      if (ref.line) {
        // Locate a distant source line with bounded retained text. Earlier
        // chunks are counted and discarded, preserving an unfinished line.
        while (this.baseLine + this.text.split('\n').length - 1 < ref.line && this.offset < Number(this.stat.size)) {
          signal.throwIfAborted(); const lines=this.text.split('\n');this.baseLine+=lines.length-1;this.text=lines.at(-1);
          this.status.textContent=`正在定位第 ${ref.line} 行 · 已扫描 ${this.offset.toLocaleString()} 字节`;await this.loadMore();
        }
        this.status.textContent=`${fileName(ref)} · 从第 ${this.baseLine} 行显示 · Node ${ref.nodeId} · 当前内容`;
      }
      if (ref.line) this.host.querySelector(`[data-line="${Number(ref.line)}"]`)?.scrollIntoView({ block: 'center' });
    } catch (e) { if (this.controller.signal !== signal) return; if (!signal.aborted) this.showError(e); else this.status.textContent = '读取已取消，可以刷新重试'; }
  }
  travel(delta) { const next = this.historyIndex + delta; if (next < 0 || next >= this.history.length) return; this.historyIndex = next; void this.open(this.history[next], { remember: false }); }
  showError(error) { this.status.textContent = error.message; this.status.classList.add('error'); }
  async loadMore() {
    if (this.loading) return; this.loading = true; this.more.disabled = true; const signal = this.controller.signal; const more = this.more; const controller = this.controller;
    try {
      const response = await fetch(resourceURL(this.ref), { signal, headers: { Range: `bytes=${this.offset}-${this.offset + chunkBytes - 1}`, ...(this.etag ? { 'If-Range': this.etag } : {}) } });
      if (!response.ok) throw new Error(`文件读取失败（HTTP ${response.status}）`);
      if (this.offset && response.status !== 206) throw new Error('文件已发生变化，请刷新以查看新内容');
      const rangeTotal = response.headers.get('Content-Range')?.match(/\/(\d+)$/)?.[1];
      if (rangeTotal != null && Number(rangeTotal) !== Number(this.stat.size)) { await response.body?.cancel(); throw new Error('文件大小已发生变化，请刷新以查看完整内容'); }
      const etag = response.headers.get('ETag'); if (this.etag && etag !== this.etag) throw new Error('文件已发生变化，请刷新'); this.etag = etag;
      // The HTTP data-plane serves bounded Range chunks; refuse unexpected full
      // responses before accumulating text from an incompatible deployment.
      if (Number(response.headers.get('Content-Length')) > chunkBytes) { await response.body?.cancel(); throw new Error('服务器未按范围读取，请升级后重试'); }
      const reader = response.body.getReader(); let bytes = 0; const parts = [];
      while (true) { const { value, done } = await reader.read(); if (done) break; bytes += value.length; if (bytes > chunkBytes) { await reader.cancel(); throw new Error('响应超过请求范围'); } parts.push(this.decoder.decode(value, { stream: true })); }
      signal.throwIfAborted();
      const total = Number(this.stat.size || 0); this.offset += bytes; const end = this.offset >= total || bytes < chunkBytes;
      if (end) parts.push(this.decoder.decode()); this.text += parts.join(''); this.renderText(); this.more.hidden = end; this.more.textContent = `继续读取 · ${this.offset.toLocaleString()} / ${total.toLocaleString()} 字节`;
    } finally { if (this.controller === controller) this.loading = false; more.disabled = false; }
  }
  renderText() {
    const top = this.body.scrollTop; this.body.replaceChildren(); this.body.classList.toggle('file-split', Boolean(this.split));
    if (this.markdown && (!this.source || this.split)) {
      const render = el('article', 'markdown-body file-markdown');
      render.innerHTML = DOMPurify.sanitize(marked.parse(this.text), { FORBID_TAGS: ['iframe','style','form','input','button'], FORBID_ATTR: ['style'] });
      const headingIDs = new Map();
      const outline = el('nav','file-outline'); outline.setAttribute('aria-label','文档目录');
      for (const [i, heading] of [...render.querySelectorAll('h1,h2,h3,h4')].entries()) { const slug=heading.textContent.toLowerCase().trim().replace(/[^\p{L}\p{N}_ -]/gu,'').replace(/\s+/g,'-')||`heading-${i}`;const count=headingIDs.get(slug)||0;headingIDs.set(slug,count+1);const id=count?`${slug}-${count}`:slug;heading.id=id; const link = el('a','',heading.textContent); link.href = `#${id}`; link.onclick = e => { e.preventDefault(); heading.scrollIntoView({ block:'start' }); }; outline.append(link); }
      if (outline.childNodes.length) render.prepend(outline);
      for (const image of render.querySelectorAll('img')) { const ref = relativeResource(this.ref,image.getAttribute('src')); if (ref) image.src = resourceURL(ref); image.loading = 'lazy'; }
      for (const link of render.querySelectorAll('a[href]')) { const href = link.getAttribute('href'); const ref = relativeResource(this.ref,href); if (ref) { link.href = workspaceURL(ref); link.onclick = e => { e.preventDefault(); this.navigate(ref); }; } else if (href.startsWith('#')) { link.onclick=e=>{e.preventDefault();try{render.querySelector(`#${CSS.escape(decodeURIComponent(href.slice(1)))}`)?.scrollIntoView({block:'start'});}catch{}}; } else if (!href.startsWith('#')) { link.target = '_blank'; link.rel = 'noopener noreferrer'; } }
      for (const code of render.querySelectorAll('pre')) code.prepend(button('复制代码', () => navigator.clipboard.writeText(code.querySelector('code')?.textContent || '')));
      this.body.append(render);
    }
    if (!this.markdown || this.source || this.split) {
      const pre = el('pre','file-source'); pre.style.counterReset=`file-line ${this.baseLine-1}`; const query = this.search?.value || '';
      for (const [i, line] of this.text.split('\n').entries()) { const row = el('span','file-source-line'); row.dataset.line = i + this.baseLine; if (this.ref.line === i+this.baseLine) row.classList.add('selected');
        if (query && line.includes(query)) { const parts = line.split(query); parts.forEach((part,index) => { if (index) row.append(el('mark','',query)); row.append(document.createTextNode(part)); }); } else highlightLine(row,line); pre.append(row); }
      this.body.append(pre);
    }
    if (this.search?.value && this.markdown && !this.source && !this.split) this.status.textContent = `已加载内容中匹配 ${this.text.split(this.search.value).length - 1} 次；切换原文可定位`;
    this.body.scrollTop = top;
  }
}

export class FileWorkspace {
  constructor(host, options = {}) { this.host = host; this.options = options; this.tabs = new Map(); this.controller = new AbortController(); }
  dispose() { this.controller.abort(); for (const { reader } of this.tabs.values()) reader.dispose(); }
  async open(ref) {
    this.root = { ...ref }; const stat = await resourceMeta(ref, {}, this.controller.signal); ref = pinExecutionIdentity(ref,stat); this.root = {...ref};
    if (!ref.archive && /\.zip$/i.test(ref.path) && stat.type === 'file') { this.root = { ...ref, archive:true,archiveEntry:'',entryId:undefined }; }
    else if (stat.type !== 'directory') { this.root = ref.archive ? { ...ref,archiveEntry:parentPath(ref.archiveEntry),entryId:undefined } : { ...ref,path:parentPath(ref.path) }; if (this.root.archiveEntry === '/') this.root.archiveEntry = ''; }
    this.host.replaceChildren(); this.host.classList.add('file-workspace'); this.tree = el('aside','file-tree'); this.tree.setAttribute('aria-label','文件树');
    this.area = el('section','file-tabs-area'); this.tabbar = el('nav','file-tabbar'); this.panes = el('div','file-tab-panes'); this.area.append(this.tabbar,this.panes); this.host.append(this.tree,this.area);
    const actions=el('div','file-tree-actions');const site=button('预览此目录的网站',()=>{const entry=prompt('网站入口（相对于当前目录）','index.html');if(!entry)return;const value=this.root.archive?{...this.root,archiveEntry:`${this.root.archiveEntry?`${this.root.archiveEntry}/`:''}${entry}`}:{...this.root,path:`${this.root.path.replace(/[\\/]+$/,'')}/${entry}`};const url=new URL(workspaceURL(value,{site:true}),location.origin);url.searchParams.set('root',this.root.archive?this.root.archiveEntry:this.root.path);url.searchParams.set('entry',entry);window.open(url.href,'_blank','noopener');});actions.append(site);this.tree.append(actions);
    const refresh = button('刷新目录', () => { this.tree.replaceChildren(actions,refresh); void this.directory(this.root,this.tree); }); this.tree.append(refresh); await this.directory(this.root,this.tree);
    if (stat.type !== 'directory' && !(this.root.archive && !ref.archive)) await this.file(ref);
  }
  async directory(ref, container, cursor = 0, expectedModifiedAt = null) {
    const page = el('div','file-tree-page'); container.append(page);
    try {
      const result = await resourceMeta(ref,{action:'list',cursor},this.controller.signal);
      if (expectedModifiedAt && result.modifiedAt && expectedModifiedAt !== result.modifiedAt) { page.append(el('p','file-status','目录已发生变化，请刷新后继续'));page.append(button('重新读取',()=>{page.remove();void this.directory(ref,container,0);}));return; }
      const entries = result.entries || []; page.append(el('p','file-status',`第 ${entries.length ? cursor + 1 : 0}–${cursor + entries.length} 项${result.hasMore ? ' · 还有更多' : ` · 共 ${result.nextCursor} 项`}`)); entries.sort((a,b) => Number(b.type==='directory')-Number(a.type==='directory') || (a.name || a.path).localeCompare(b.name || b.path));
      for (const entry of entries) {
        const child = { ...ref, path:entry.path,archiveEntry:entry.archiveEntry,archiveVersion:entry.archiveVersion,entryId:entry.entryId,line:undefined }; const name = entry.name || fileName(child); const row = el('div','file-tree-row');
        if (entry.type==='directory') { const details = el('details'); const summary = el('summary','',name); const children = el('div','file-tree-children'); details.append(summary,children); details.ontoggle = () => { if (details.open && !children.childNodes.length) void this.directory(child,children); }; row.append(details); }
        else { const b = button(`${name}${entry.entryId != null ? ` · #${entry.entryId}` : ''}`,() => this.file(child)); b.title = child.archiveEntry || child.path; row.append(b); } page.append(row);
      }
      if (result.hasMore) { const next = button('下一页 →', () => { page.remove(); void this.directory(ref,container,result.nextCursor,result.modifiedAt); }); const previous = button('← 第一页', () => { page.remove(); void this.directory(ref,container,0); }); page.append(previous,next); }
      if (!entries.length) page.append(el('p','muted','空目录'));
    } catch(e) { if (!this.controller.signal.aborted) page.textContent = e.message; }
  }
  async file(ref) {
    const key = JSON.stringify([ref.path,ref.archiveEntry,ref.entryId]); let tab = this.tabs.get(key);
    if (!tab) {
      const label = el('span','file-tab'); const select = button(fileName(ref),() => this.select(key)); const close = button('×',() => { tab.reader.dispose(); tab.pane.remove();label.remove();this.tabs.delete(key);const next=this.tabs.keys().next().value;if(next)this.select(next); }); close.setAttribute('aria-label',`关闭 ${fileName(ref)}`); label.append(select,close);
      const pane = el('div','file-tab-pane'); const reader = new FileReader(pane,{navigate:next=>this.file(next),...this.options}); tab = {reader,pane,label};this.tabs.set(key,tab);this.tabbar.append(label);this.panes.append(pane);this.select(key);await reader.open(ref);return;
    } this.select(key); if(ref.line || ref.archiveVersion && ref.archiveVersion !== tab.reader.ref.archiveVersion)await tab.reader.open(ref,{remember:false});
  }
  select(key) { for (const [id,tab] of this.tabs) { if (id !== key) for (const media of tab.pane.querySelectorAll('video,audio')) media.pause(); tab.pane.hidden = id !== key; tab.label.classList.toggle('selected',id===key); } }
}

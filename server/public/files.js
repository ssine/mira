// The file workspace page: a tree with tabbed readers for one Node resource,
// and a launcher that serves a directory as a static website.
import { FileWorkspace, fileActions, fileName, parentPath, renderFileActions, resourceMeta, runFileAction } from '/file-preview.js';

const query = new URLSearchParams(location.search);
const ref = {
  nodeId: query.get('nodeId'), path: query.get('path'),
  ...(query.get('archive') === 'true' ? { archive: true, archiveEntry: query.get('archiveEntry') || '' } : {}),
  ...(query.has('archiveVersion') ? { archiveVersion: query.get('archiveVersion') } : {}),
  ...(query.has('entryId') ? { entryId: Number(query.get('entryId')) } : {}),
  ...(query.has('line') ? { line: Number(query.get('line')) } : {}),
  ...(query.has('executionContext') ? { executionContext: query.get('executionContext') } : {}),
  ...(query.has('userSessionId') ? { userSessionId: Number(query.get('userSessionId')) } : {}),
};
const sourceThreadId = query.get('sourceThreadId') || '';
const $ = id => document.getElementById(id);

let csrf = '';
let stat = null;       // the opened resource, once its Node has answered
let node = null;       // registry record: display name and capabilities
let mode = 'files';
let workspace = null;
let sessionID = query.get('previewId');
let lease = null;
let launching = false;

async function api(path, method = 'GET', body, { timeout = 30_000 } = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeout);
  try {
    const response = await fetch(path, {
      method, signal: controller.signal, credentials: 'same-origin',
      headers: { ...(body ? { 'Content-Type': 'application/json' } : {}), ...(method !== 'GET' ? { 'x-mira-csrf': csrf } : {}) },
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    let value = {};
    try { value = await response.json(); } catch { /* empty or non-JSON body */ }
    if (!response.ok) throw Object.assign(new Error(value.error || `HTTP ${response.status}`), { status: response.status, code: value.code });
    return value;
  } catch (error) {
    if (error.name === 'AbortError') throw Object.assign(new Error('请求超时'), { code: 'timeout' });
    throw error;
  } finally { clearTimeout(timer); }
}

const nodeName = () => !node ? '' :
  `${node.displayName?.trim() || node.hostname || '未命名设备'}${node.nodeMode === 'wsl' ? ' · WSL' : node.platform === 'windows' ? ' · Windows' : ''}`;

function describe() {
  const name = fileName(ref) || ref.path || '文件工作区';
  const location = ref.archive ? `${ref.path} → ${ref.archiveEntry || '/'}` : ref.path || '';
  $('fileName').textContent = name;
  $('fileLocation').textContent = [nodeName(), location].filter(Boolean).join(' · ');
  $('fileLocation').title = location;
  document.title = `${name} · Mira 文件工作区`;
}

let noticeTimer = null;
function notice(text = '', { error = false, transient = false } = {}) {
  const status = $('filePageStatus');
  clearTimeout(noticeTimer);
  status.textContent = text; status.hidden = !text; status.classList.toggle('error', error);
  if (text && transient) noticeTimer = setTimeout(() => { status.hidden = true; }, 4000);
}

function renderActions() {
  // Serving a website is this page's own "网站" view rather than a link away.
  const actions = fileActions(ref, { type: stat?.type ?? '', node, nodeName: nodeName(), sourceThreadId, surface: 'workspace' })
    .filter(action => action.id !== 'site');
  renderFileActions($('fileActions'), actions, id => runFileAction(id, ref, {
    post: (path, body) => api(path, 'POST', body), nodeName: nodeName(),
    notify: text => notice(text, { transient: true }),
  }), { short: true });
}

function closeTree() {
  document.body.classList.remove('file-tree-visible');
  $('fileTreeToggle').setAttribute('aria-expanded', 'false');
  $('fileTreeBackdrop').hidden = true;
}

async function loadWorkspace() {
  if (workspace) return;
  workspace = new FileWorkspace($('fileWorkspace'), { onSelect: closeTree });
  try { await workspace.open(ref); }
  catch (error) { if (error.name !== 'AbortError') notice(error.message, { error: true }); }
}

// A reload or Back returns to this page as it is: it never starts a website
// by itself again, and it still controls the preview session it started.
function rememberMode() {
  const url = new URL(location.href);
  if (mode === 'site') { url.searchParams.set('view', 'site'); url.searchParams.set('noauto', '1'); }
  else { url.searchParams.delete('view'); url.searchParams.delete('noauto'); }
  if (sessionID) url.searchParams.set('previewId', sessionID); else url.searchParams.delete('previewId');
  history.replaceState(null, '', url);
}

function setMode(next, { remember = true } = {}) {
  mode = next;
  $('modeFiles').setAttribute('aria-selected', String(next === 'files'));
  $('modeSite').setAttribute('aria-selected', String(next === 'site'));
  $('siteControls').hidden = next !== 'site';
  $('fileWorkspace').hidden = next !== 'files';
  $('fileTreeToggle').hidden = next !== 'files';
  closeTree();
  if (next === 'files') void loadWorkspace();
  if (remember) rememberMode();
}

// The container a website is served from: a ZIP is browsed as an archive.
function siteResource() {
  return !ref.archive && /\.zip$/i.test(ref.path) && stat?.type === 'file' ? { ...ref, archive: true, archiveEntry: '', entryId: undefined } : ref;
}

function siteDefaults() {
  if (query.has('root') || query.has('entry')) {
    $('siteRoot').value = query.get('root') ?? ''; $('siteEntry').value = query.get('entry') ?? 'index.html';
    return;
  }
  const resource = siteResource();
  const current = resource.archive ? resource.archiveEntry : resource.path;
  if (stat?.type === 'directory' || (resource.archive && !resource.archiveEntry)) {
    $('siteRoot').value = current; $('siteEntry').value = 'index.html';
    return;
  }
  // parentPath has no parent to return for a top-level ZIP entry.
  $('siteRoot').value = resource.archive && !/[\\/]/.test(current) ? '' : parentPath(current);
  $('siteEntry').value = fileName(resource);
}

function siteState(text = '', error = false) {
  $('siteState').textContent = text; $('siteState').classList.toggle('error', error);
}

const siteProblems = {
  preview_domain_unconfigured: '这个 Mira 部署还没有配置网站预览域名（MIRA_NODE_PREVIEW_DOMAIN 以及对应的通配符 DNS 和证书），所以无法打开网站。仍然可以在“文件”视图里查看内容。',
  preview_ingress_unconfigured: '当前访问 Mira 所用的地址没有对应的网站预览地址。请换用已配置预览域名的入口打开 Mira 后重试。',
  preview_unavailable: '这个 Node 的版本还不支持网站预览，请先升级 Node。',
  node_offline: 'Node 当前不在线，连接恢复后重试。',
  timeout: '启动超时：Node 没有及时响应。请确认它在线后重试。',
};

async function suggestEntries(root) {
  const host = $('siteSuggestions');
  host.replaceChildren(); host.hidden = true;
  try {
    const resource = siteResource();
    const listing = await resourceMeta(resource.archive ? { ...resource, archiveEntry: root, entryId: undefined } : { ...resource, path: root }, { action: 'list' });
    const pages = (listing.entries ?? []).filter(entry => entry.type !== 'directory' && /\.html?$/i.test(entry.name || entry.path)).slice(0, 12);
    if (!pages.length) return;
    host.append(Object.assign(document.createElement('span'), { textContent: '这个目录里的页面：' }));
    for (const page of pages) {
      const name = page.name || fileName({ path: page.path });
      const choice = Object.assign(document.createElement('button'), { type: 'button', className: 'ghost', textContent: name });
      choice.addEventListener('click', () => { $('siteEntry').value = name; void launchSite(); });
      host.append(choice);
    }
    host.hidden = false;
  } catch { /* the launch error already explains the situation */ }
}

function renew() {
  clearInterval(lease);
  lease = setInterval(() => {
    api(`/v1/file-previews/${sessionID}`, 'POST').catch(error => { clearInterval(lease); siteState(`预览会话续期失败：${error.message}`, true); });
  }, 10 * 60 * 1000);
}

// Opens the website in this tab. A page-initiated navigation needs no popup
// permission, which mobile browsers and WebViews often withhold.
async function launchSite({ auto = false } = {}) {
  if (launching) return;
  const root = $('siteRoot').value.trim();
  const entry = $('siteEntry').value.trim().replace(/^[\\/]+/, '');
  launching = true; $('siteOpen').disabled = true; $('siteLink').hidden = true; $('siteSuggestions').hidden = true;
  siteState('正在启动网站预览…');
  try {
    if (!entry) throw Object.assign(new Error('请填写入口文件'), { code: 'invalid_entry_empty' });
    if (sessionID) { await api(`/v1/file-previews/${sessionID}`, 'DELETE').catch(() => {}); sessionID = null; clearInterval(lease); }
    const result = await api('/v1/file-previews', 'POST', { nodeId: ref.nodeId, root, entry, resource: siteResource() });
    sessionID = result.id; renew();
    $('siteStop').hidden = false;
    $('siteLink').href = result.url;
    siteState('网站已准备好，正在打开…');
    // If the browser did not follow the navigation, offer the same address.
    setTimeout(() => { $('siteLink').hidden = false; siteState('网站已准备好。'); }, 2500);
    if (auto) location.replace(result.url);
    else { rememberMode(); location.assign(result.url); }
  } catch (error) {
    if (error.code === 'invalid_entry') {
      siteState(`在根目录 ${root || '/'} 下没有找到入口文件 ${entry}。请修改入口，或从下面选择一个页面。`, true);
      void suggestEntries(root);
    } else if (error.code === 'invalid_root') siteState(`站点根目录 ${root || '/'} 不存在，或不是目录。`, true);
    else siteState(siteProblems[error.code] ?? `无法打开网站：${error.message}`, true);
  } finally { launching = false; $('siteOpen').disabled = false; }
}

async function stopSite() {
  try { if (sessionID) await api(`/v1/file-previews/${sessionID}`, 'DELETE'); siteState('预览会话已关闭。'); }
  catch (error) { siteState(error.message, true); return; }
  sessionID = null; clearInterval(lease);
  $('siteStop').hidden = true; $('siteLink').hidden = true;
  rememberMode();
}

$('fileTreeToggle').addEventListener('click', () => {
  const open = document.body.classList.toggle('file-tree-visible');
  $('fileTreeToggle').setAttribute('aria-expanded', String(open));
  $('fileTreeBackdrop').hidden = !open;
});
$('fileTreeBackdrop').addEventListener('click', closeTree);
$('modeFiles').addEventListener('click', () => setMode('files'));
$('modeSite').addEventListener('click', () => setMode('site'));
$('siteOpen').addEventListener('click', () => { void launchSite(); });
$('siteStop').addEventListener('click', () => { void stopSite(); });
for (const id of ['siteRoot', 'siteEntry']) $(id).addEventListener('keydown', event => { if (event.key === 'Enter') void launchSite(); });
// This tab was opened from the conversation: closing it returns there with the
// conversation exactly as it was. Navigate only when the tab cannot be closed.
$('fileReturn').addEventListener('click', event => {
  if (event.ctrlKey || event.metaKey || event.shiftKey || event.button) return;
  event.preventDefault();
  window.close();
  setTimeout(() => { location.assign($('fileReturn').href); }, 250);
});
window.addEventListener('pagehide', () => { workspace?.dispose(); clearInterval(lease); });
// Returning from the website with Back restores this page from memory.
window.addEventListener('pageshow', event => {
  if (!event.persisted) return;
  $('siteLink').hidden = true;
  if (sessionID) { renew(); siteState('预览会话仍然有效；可以再次打开网站，或关闭它。'); }
});

(async () => {
  describe();
  if (sourceThreadId) $('fileReturn').href = `/?thread=${encodeURIComponent(sourceThreadId)}`;
  if (!ref.nodeId || !ref.path) { notice('缺少文件所属 Node 或路径，请从 Mira 对话重新打开。', { error: true }); return; }
  try { csrf = (await api('/v1/admin/session')).csrfToken; }
  catch (error) { notice(error.status === 401 ? '请先返回 Mira 登录，再重新打开文件。' : error.message, { error: true }); return; }
  void api(`/v1/nodes/${encodeURIComponent(ref.nodeId)}`).then(value => { node = value; describe(); renderActions(); }).catch(() => {});
  const wantsSite = query.get('view') === 'site';
  if (wantsSite) { $('fileModes').hidden = false; setMode('site', { remember: false }); siteState('正在读取文件信息…'); }
  else setMode('files', { remember: false });
  try { stat = await resourceMeta(ref); }
  catch (error) {
    // The workspace reports its own read failure; the launcher needs this one.
    if (wantsSite) siteState(siteProblems[error.code] ?? `无法读取 ${ref.path}：${error.message}`, true);
    return;
  }
  renderActions();
  $('fileModes').hidden = !(wantsSite || stat.type === 'directory' || ref.archive || /\.(?:html?|zip)$/i.test(ref.path));
  siteDefaults();
  if (!wantsSite) return;
  siteState('');
  if (sessionID) {
    // Reached from a running website to adjust its root.
    try {
      const details = await api(`/v1/file-previews/${sessionID}`);
      $('siteRoot').value = details.root; $('siteEntry').value = decodeURIComponent(details.entry.slice(1));
      $('siteStop').hidden = false; renew();
      siteState('调整根目录或入口后重新打开网站，或关闭当前预览会话。');
    } catch { sessionID = null; siteState('原来的预览会话已结束，可以重新打开网站。'); }
  } else if (query.get('noauto') !== '1') await launchSite({ auto: true });
})();

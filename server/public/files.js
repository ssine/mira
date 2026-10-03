import { FileWorkspace, parentPath, fileName } from '/file-preview.js';
const query = new URLSearchParams(location.search);
const ref = { nodeId:query.get('nodeId'),path:query.get('path'),...(query.get('archive')==='true'?{archive:true,archiveEntry:query.get('archiveEntry')||''}:{}),...(query.has('archiveVersion')?{archiveVersion:query.get('archiveVersion')}:{}),...(query.has('entryId')?{entryId:Number(query.get('entryId'))}:{}),...(query.has('line')?{line:Number(query.get('line'))}:{}),...(query.has('executionContext')?{executionContext:query.get('executionContext')}:{}),...(query.has('userSessionId')?{userSessionId:Number(query.get('userSessionId'))}:{}) };
const $ = id => document.getElementById(id); let csrf; let sessionID = query.get('previewId'); let lease;
async function api(path,method='GET',body) { const r=await fetch(path,{method,headers:{...(body?{'Content-Type':'application/json'}:{}),...(method!=='GET'?{'x-mira-csrf':csrf}:{})},...(body?{body:JSON.stringify(body)}:{})});const b=await r.json();if(!r.ok)throw new Error(b.error||`HTTP ${r.status}`);return b; }
async function site() {
  const root=$('siteRoot').value.trim();const entry=$('siteEntry').value.trim();if(sessionID)await api(`/v1/file-previews/${sessionID}`,'DELETE');
  const result=await api('/v1/file-previews','POST',{nodeId:ref.nodeId,root,entry,resource:ref});sessionID=result.id;clearInterval(lease);lease=setInterval(()=>api(`/v1/file-previews/${sessionID}`,'POST').catch(e=>{$('filePageStatus').textContent=e.message;clearInterval(lease)}),10*60*1000);
  $('siteStop').hidden=false;$('filePageStatus').textContent=`网站已准备好 · 有效期 ${new Date(result.expiresAt).toLocaleTimeString()}`;
  // Automatic entry runs in the tab opened synchronously by the chat click.
  // For root adjustments a user-created tab is reserved before the request.
  return result.url;
}
(async()=>{try{
  if(!ref.nodeId||!ref.path)throw new Error('缺少文件所属 Node 或路径，请从 Mira 对话重新打开');
  const auth=await api('/v1/admin/session');csrf=auth.csrfToken;
  const thread=query.get('sourceThreadId');if(thread)$('fileReturn').href=`/?thread=${encodeURIComponent(thread)}`;
  $('fileLocation').textContent=`${ref.nodeId} · ${ref.path}${ref.archive?` → ${ref.archiveEntry}`:''}`;
  $('filePageStatus').textContent='文件工作区只读 · 展示 Node 上的当前内容';
  const workspace=new FileWorkspace($('fileWorkspace'));await workspace.open(ref);
  window.addEventListener('pagehide',()=>{workspace.dispose();clearInterval(lease)});
  $('fileTreeToggle').onclick=()=>document.body.classList.toggle('file-tree-visible');
  if(query.get('view')==='site'){
    $('siteControls').hidden=false;const current=ref.archive?ref.archiveEntry:ref.path;$('siteRoot').value=parentPath(current);if(ref.archive&&$('siteRoot').value==='/')$('siteRoot').value='';$('siteEntry').value=fileName(ref);if(query.has('root'))$('siteRoot').value=query.get('root');if(query.has('entry'))$('siteEntry').value=query.get('entry');
    $('siteOpen').onclick=async()=>{const target=window.open('about:blank','_blank');try{const url=await site();if(target)target.location.replace(url);else{$('filePageStatus').textContent='窗口被拦截，请允许打开预览窗口'}}catch(e){target?.close();$('filePageStatus').textContent=e.message}};
    $('siteStop').onclick=async()=>{try{await api(`/v1/file-previews/${sessionID}`,'DELETE');sessionID=null;clearInterval(lease);$('siteStop').hidden=true;$('filePageStatus').textContent='预览会话已关闭'}catch(e){$('filePageStatus').textContent=e.message}};
    // Retain this authenticated loading page for root adjustment and leases;
    // first navigation replaces it only when no adjustment controls are needed.
    if (query.get('noauto') !== '1') { const url=await site(); location.replace(url); }
    else if(sessionID) {const details=await api(`/v1/file-previews/${sessionID}`);$('siteRoot').value=details.root;$('siteEntry').value=decodeURIComponent(details.entry.slice(1));$('siteStop').hidden=false;$('filePageStatus').textContent='调整根目录后，点击打开网站；或关闭当前预览会话';}
  }
}catch(e){$('filePageStatus').textContent=e.message;if(/401/.test(e.message))$('filePageStatus').textContent='请先返回 Mira 登录，再重新打开文件';}})();

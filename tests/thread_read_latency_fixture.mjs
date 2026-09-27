// Disposable HTTP fixture: slow sidebar requests must not gate canonical prose.
import http from 'node:http';
import fs from 'node:fs/promises';
const ids = ['00000000-0000-4000-8000-0000000000a1', '00000000-0000-4000-8000-0000000000b2'];
const rows = ids.map((threadId, i) => ({threadId, title:`Conversation ${i+1}`, cwd:'/work', generation:1, itemCount:1, listRoot:true, activity:{state:'idle'}}));
let holdRoots = true, failRoots = false, holdPaths = false;
const pending = [], requests = [];
const reply = (response, value, status=200) => { response.writeHead(status, {'Content-Type':'application/json'}); response.end(JSON.stringify(value)); };
const server = http.createServer(async (request, response) => {
 const url = new URL(request.url, 'http://localhost'), path = url.pathname;
 if (path === '/__test/control') {
  let body=''; for await (const chunk of request) body+=chunk;
  if (body) ({holdRoots=false, failRoots=false, holdPaths=false} = JSON.parse(body));
  for (let i=pending.length-1; i>=0; i--) {
   const entry=pending[i];
   if (entry.view==='roots' ? !holdRoots : !holdPaths) { pending.splice(i,1); entry.finish(); }
  }
  return reply(response,{requests,pending:pending.map(p=>p.view)});
 }
 if (path === '/healthz') return reply(response,{version:'test',adminConfigured:true});
 if (path === '/v1/admin/session') return reply(response,{csrfToken:'fixture'});
 if (path === '/v1/nodes') return reply(response,{data:[]});
 if (path === '/v1/codex/threads') {
  const view=url.searchParams.get('view'); requests.push({view,limit:url.searchParams.get('limit')});
  const finish=()=>reply(response,{paged:true,data:rows,total:rows.length,projects:[],nextCursor:null});
  if (view==='roots' && failRoots) return reply(response,{error:'Temporary sidebar failure'},503);
  if (view==='roots' && holdRoots || view==='path' && holdPaths) {pending.push({view,finish});return;}
  return finish();
 }
 const row=rows.find(r=>path.includes(r.threadId));
 if (row && path.endsWith('/transcript')) return reply(response,{generation:1,itemCount:1,trace:[{key:'answer',kind:'assistant',body:`History for ${row.title}`,turnId:'turn'}],nextCursor:null});
 if (row) return reply(response,row);
 if (path.startsWith('/v1/')) return reply(response,{data:[]});
 try {
  const resource=path==='/'?'index.html':path.slice(1);
  if (resource.includes('..')) throw Error('Invalid path');
  const file=new URL(resource.startsWith('vendor/')?`../node/internal/webassets/web/${resource}`:`../server/public/${resource}`,import.meta.url);
  response.writeHead(200,{'Content-Type':resource.endsWith('.js')?'text/javascript':resource.endsWith('.css')?'text/css':'text/html','Cache-Control':'no-store'});
  response.end(await fs.readFile(file));
 } catch {response.writeHead(404);response.end();}
});
server.listen(0,'127.0.0.1',()=>console.log(`http://127.0.0.1:${server.address().port}`));

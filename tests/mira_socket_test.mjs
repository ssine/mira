import assert from 'node:assert/strict';
import test from 'node:test';
import { MiraSocket, configureMiraTransport } from '../server/public/mira-socket.js';
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
function globals(t) {
 const previous = Object.fromEntries(['location','sessionStorage','fetch','WebSocket'].map(k=>[k,globalThis[k]]));
 globalThis.location={href:'https://transport.test/?miraTransport=https'};
 globalThis.sessionStorage={getItem:()=>null};
 t.after(()=>Object.assign(globalThis,previous));
 configureMiraTransport({csrf:()=> 'test-csrf'});
}
const id='a'.repeat(32);
const handshake=()=>Response.json({id,protocol:'test-v1'},{status:201});
function record(seq,text){const payload=new TextEncoder().encode(text),data=new Uint8Array(13+payload.length),view=new DataView(data.buffer);view.setUint8(0,1);view.setBigUint64(1,BigInt(seq));view.setUint32(9,payload.length);data.set(payload,13);return data;}
function untilAbort(signal){return new Promise((_,reject)=>{signal.addEventListener('abort',()=>reject(signal.reason),{once:true});if(signal.aborted)reject(signal.reason)});}
test('HTTPS receive retries a lost response body without publishing duplicate messages', async t=>{
 globals(t);let receives=0;const cursors=[];
 globalThis.fetch=async (url,options)=>{
  assert.equal(options.redirect,'error');
  if(url.includes('transport=https')){assert.equal(options.headers['X-Mira-Csrf'],'test-csrf');return handshake()}
  if(url.endsWith('/close'))return new Response(null,{status:204});
  cursors.push(url);
  if(++receives===1)return new Response(new ReadableStream({start(c){c.enqueue(record(1,'once').slice(0,8));c.error(new Error('lost body'))}}));
  if(receives===2)return new Response(record(1,'once'));
  return untilAbort(options.signal);
 };
 const socket=new MiraSocket('wss://transport.test/connect',['test-v1']);t.after(()=>socket.close());
 const messages=[];socket.addEventListener('message',event=>messages.push(event.data));
 while(messages.length===0)await delay(5);
 assert.deepEqual(messages,['once']);assert.equal(cursors[0],cursors[1]);socket.close();
});
test('HTTPS send retries immutable bytes; expiry ends the connection without replay',async t=>{
 globals(t);let sends=0;const frames=[];
 globalThis.fetch=async(url,options)=>{
  if(url.includes('transport=https'))return handshake();
  if(url.includes('/receive'))return untilAbort(options.signal);
  if(url.endsWith('/close'))return new Response(null,{status:204});
  frames.push(Buffer.from(options.body));
  if(++sends===1)throw new Error('lost acknowledgement');
  if(sends===2)return new Response(null,{status:204});
  return new Response(null,{status:410});
 };
 const socket=new MiraSocket('wss://transport.test/connect',['test-v1']);t.after(()=>socket.close());
 await new Promise(resolve=>socket.addEventListener('open',resolve,{once:true}));socket.send('one');
 await socket.outgoing;assert.equal(sends,2);assert.deepEqual(frames[0],frames[1]);
 socket.send('two');await socket.outgoing;assert.equal(socket.readyState,3);assert.equal(sends,3);
 assert.throws(()=>socket.send('three'),/not open/);
});
test('closing an opening WebSocket prevents HTTP fallback', async t=>{
 globals(t);globalThis.location.href='https://close.test/';let http=0;
 globalThis.fetch=async()=>{http++;return handshake()};
 class Native extends EventTarget {close(){this.dispatchEvent(Object.assign(new Event('close'),{code:1000,reason:''}))}}
 globalThis.WebSocket=Native;
 const socket=new MiraSocket('wss://close.test/connect',['test-v1']);socket.close();await delay(5);
 assert.equal(socket.readyState,3);assert.equal(http,0);
});
test('auto falls back on a failed handshake; explicit websocket stays selected', async t=>{
 globals(t);
 for(const preference of ['auto','websocket']){
  globalThis.location.href=`https://${preference}-handshake.test/?miraTransport=${preference}`;let posts=0;
  globalThis.fetch=async(url,options)=>{
   if(url.includes('transport=https')){posts++;return handshake()}
   if(url.endsWith('/close'))return new Response(null,{status:204});
   return untilAbort(options.signal);
  };
  class Native extends EventTarget {constructor(){super();queueMicrotask(()=>this.dispatchEvent(new Event('error')))}close(){this.dispatchEvent(Object.assign(new Event('close'),{code:1006,reason:''}))}}
  globalThis.WebSocket=Native;
  const socket=new MiraSocket(`wss://${preference}-handshake.test/connect`,['test-v1']);
  await new Promise(resolve=>socket.addEventListener(preference==='auto'?'open':'close',resolve,{once:true}));
  assert.equal(posts,preference==='auto'?1:0);socket.close();
 }
});
test('two established abnormal closes select HTTPS on the next connection', async t=>{
 globals(t);globalThis.location.href='https://abnormal.test/';let nativeCount=0,posts=0;
 class Native extends EventTarget {constructor(){super();nativeCount++;this.protocol='test-v1';queueMicrotask(()=>this.dispatchEvent(new Event('open')))}close(){}}
 globalThis.WebSocket=Native;
 globalThis.fetch=async(url,options)=>{
  if(url.includes('transport=https')){posts++;return handshake()}
  if(url.endsWith('/close'))return new Response(null,{status:204});
  return untilAbort(options.signal);
 };
 for(let i=0;i<2;i++){
  const socket=new MiraSocket('wss://abnormal.test/connect',['test-v1']);
  await new Promise(resolve=>socket.addEventListener('open',resolve,{once:true}));
  socket.native.dispatchEvent(Object.assign(new Event('close'),{code:1006,reason:'connection lost'}));
  assert.equal(socket.readyState,3);
 }
 const socket=new MiraSocket('wss://abnormal.test/connect',['test-v1']);
 await new Promise(resolve=>socket.addEventListener('open',resolve,{once:true}));
 assert.equal(nativeCount,2);assert.equal(posts,1);socket.close();
});
test('malformed HTTPS batches publish no partial messages', async t=>{
 globals(t);
 globalThis.fetch=async(url)=>{
  if(url.includes('transport=https'))return handshake();
  if(url.endsWith('/close'))return new Response(null,{status:204});
  const good=record(1,'must not publish'),batch=new Uint8Array(good.length+1);batch.set(good);return new Response(batch);
 };
 const socket=new MiraSocket('wss://malformed.test/connect',['test-v1']);const messages=[];
 socket.addEventListener('message',event=>messages.push(event.data));
 await new Promise(resolve=>socket.addEventListener('close',resolve,{once:true}));
 assert.deepEqual(messages,[]);assert.equal(socket.received,0);
});

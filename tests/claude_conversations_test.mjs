import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs/promises";
import vm from "node:vm";
import { claudeTrace, ClaudeRuntime } from "../server/public/claude.js";
import { conversationPageReader } from "../server/public/conversation-pages.js";
import { accountGroups, accountNode } from "../server/public/codex-accounts.js";
import { AccountSpend } from "../server/public/account-spend.js";

test("same account name stays separated by engine, including expense endpoints", () => {
 const node = { nodeId: "wsl", codexAccounts: [{ name: "Mafia", nodeAccountId: "c" }], claudeAccounts: [{ engine:"claude", name:"Mafia", nodeAccountId:"a", configured:true }] };
 const groups = accountGroups([node]); assert.equal(groups.length,2); assert.notEqual(groups[0].key,groups[1].key);
 assert.equal(accountNode(node,"a").engine,"claude");
 for (const g of groups) assert.match(AccountSpend.prototype.urlFor(g.key,"7d"), new RegExp(`/v1/${g.engine}/accounts/cost-history\\?name=Mafia`));
});

test("native projections replace stream prose, merge tool results, and retain native questions", () => {
 const rows = [
 {type:"mira_user",text:"hello"},
 {type:"stream_event",event:{type:"message_start",message:{id:"m"}}},
 {type:"stream_event",event:{type:"content_block_delta",index:0,delta:{type:"text_delta",text:"part"}}},
 {type:"assistant",uuid:"final",timestamp:"2026-09-30T01:02:03.456Z",message:{id:"m",content:[{type:"text",text:"full"},{type:"tool_use",id:"t",name:"Read",input:{path:"a"}}]}},
 {type:"user",message:{content:[{type:"tool_result",tool_use_id:"t",content:"output"}]}},
 {type:"assistant",parent_tool_use_id:"t",message:{content:[{type:"text",text:"child only"}]}},
 {type:"mira_question",questionId:"q",questions:[{question:"Choose"}]},
 {type:"result",duration_ms:100,total_cost_usd:1}
 ].map((payload,seq)=>({seq,payload,turnId:"turn"}));
 const view=claudeTrace(rows);
 assert.deepEqual(view.trace.filter(x=>x.kind==="assistant"&&x.body).map(x=>x.body),["full"]);
 assert.equal(view.trace.find(x=>x.kind==="assistant"&&x.body).completedAt,"2026-09-30T01:02:03.456Z");
 assert.equal(view.trace.find(x=>x.kind==="assistant"&&x.body).timingScope,"recorded");
 assert.equal(view.trace.filter(x=>x.kind==="tool").length,1);assert.match(view.trace.find(x=>x.kind==="tool").body,/output/);
 assert.equal(view.questions.size,1);
 assert.equal(claudeTrace([...rows,{seq:99,payload:{type:"mira_answer",questionId:"q",answers:{a:"yes"}}}]).questions.size,0);
});

test("Claude block completion keeps streaming identity until the whole message stops", async () => {
 for (const reloaded of [false,true]) {
  let seq=0, data=[];
  const runtime=new ClaudeRuntime(async()=>({data,cursor:seq,earliest:1,session:{}}));
  const poll=async(...payloads)=>{
   data=payloads.map(payload=>({seq:++seq,turnId:"turn",payload}));
   return (await runtime.history({threadId:"s",sessionId:"s"},{poll:true})).trace;
  };
  const stream=event=>({type:"stream_event",event});
  const prose=trace=>trace.filter(item=>item.kind==="assistant").map(item=>item.body);
  const saved=(uuid,type,value)=>({type:"assistant",uuid,message:{id:"m",content:[{type,[type=== "thinking"?"thinking":"text"]:value}]}});
  if (!reloaded) await poll(stream({type:"message_start",message:{id:"m"}}));
  await poll(saved("thinking","thinking","Consider the question"));
  await poll(stream({type:"content_block_stop",index:0}),stream({type:"content_block_start",index:1,content_block:{type:"text",text:""}}));
  assert.deepEqual(prose(await poll(stream({type:"content_block_delta",index:1,delta:{type:"text_delta",text:"Hello"}}))),["Hello"],"body remains live after the thinking block is saved");
  assert.deepEqual(prose(await poll(stream({type:"content_block_delta",index:1,delta:{type:"text_delta",text:" world"}}))),["Hello world"]);
  assert.deepEqual(prose(await poll(saved("text","text","Hello world"))),["Hello world"],"saved text replaces the transient body before message_stop");
  const final=await poll(stream({type:"content_block_stop",index:1}),stream({type:"message_stop"}),{type:"result",duration_ms:50});
  assert.deepEqual(prose(final),["Hello world"]);
  assert.equal(final.find(item=>item.kind==="assistant").turnElapsedMs,50);
  assert.equal([...runtime.rows.values()].some(row=>row.payload.type==="stream_event"),false,"stopped message deltas are released");
  assert.deepEqual(prose(await poll({type:"assistant",uuid:"another-answer",message:{id:"another-message",content:[{type:"text",text:"Hello world"}]}})),["Hello world","Hello world"],"different messages with identical prose remain distinct");
 }
});

test("a child message ending between parent deltas does not clear the parent stream", async () => {
 let seq=0,data=[];
 const runtime=new ClaudeRuntime(async()=>({data,cursor:seq,session:{}}));
 const poll=async(...payloads)=>{
  data=payloads.map(payload=>({seq:++seq,turnId:"turn",payload}));
  return (await runtime.history({threadId:"s",sessionId:"s"},{poll:true})).trace;
 };
 const stream=(event,parent_tool_use_id=null)=>({type:"stream_event",event,parent_tool_use_id});
 await poll(stream({type:"message_start",message:{id:"parent"}}),stream({type:"content_block_delta",index:0,delta:{type:"text_delta",text:"First "}}));
 await poll(stream({type:"message_start",message:{id:"child"}},"tool"),stream({type:"content_block_delta",index:0,delta:{type:"text_delta",text:"Child text"}},"tool"));
 await poll({type:"assistant",parent_tool_use_id:"tool",message:{id:"child",content:[{type:"text",text:"Child text"}]}},stream({type:"message_stop"},"tool"));
 const live=await poll(stream({type:"content_block_delta",index:0,delta:{type:"text_delta",text:"answer"}}));
 assert.deepEqual(live.filter(item=>item.kind==="assistant").map(item=>item.body),["First answer"]);
 const final=await poll({type:"assistant",message:{id:"parent",content:[{type:"text",text:"First answer"}]}},stream({type:"message_stop"}));
 assert.deepEqual(final.filter(item=>item.kind==="assistant").map(item=>item.body),["First answer"]);
 assert.equal([...runtime.rows.values()].some(row=>row.payload.type==="stream_event"),false);
});

test("mixed pagination keeps both cursors, all projects, and does not restart an exhausted engine", async () => {
 const calls=[];
 const reader=conversationPageReader(async url=>{
  const u=new URL(url,"https://example.test"),engine=u.pathname.includes("claude")?"claude":"codex",cursor=u.searchParams.get("cursor");calls.push([engine,cursor]);
  return {paged:true,projects:[{key:"same",count:engine==="claude"?70:2}],data:[{threadId:engine+(cursor||"head"),updatedAt:"2026-01-01"}],nextCursor:engine==="claude"&&!cursor?"next":null};
 },()=>"claude");
 const first=await reader(new URLSearchParams({view:"roots"}));assert.equal(first.projects[0].count,72);assert.equal(first.data.length,2);
 const second=await reader(new URLSearchParams({view:"roots",cursor:first.nextCursor}));assert.deepEqual(second.data.map(x=>x.threadId),["claudenext"]);assert.equal(second.nextCursor,null);assert.equal(second.projects[0].count,72);
 assert.deepEqual(calls,[["codex",null],["claude",null],["claude","next"]]);
});

test("lost native turn response reuses exactly the same request and account",async()=>{
 const requests=[];const runtime=new ClaudeRuntime(async (_url,options)=>{requests.push(JSON.parse(options.body));if(requests.length===1)throw new Error("lost");return{turnId:"ok"};});
 await assert.rejects(runtime.send({sessionId:"s"},{text:"one",nodeAccountId:"A"}));
 await runtime.send({sessionId:"s"},{text:"two",nodeAccountId:"B"});assert.deepEqual(requests[0],requests[1]);
});

test("empty native pages preserve the legacy Codex full-tree contract", async () => {
 const rows=[{threadId:"root"},{threadId:"child",parentThreadId:"root",activity:{state:"running"}}];
 const reader=conversationPageReader(async url=>url.startsWith("/v1/codex/")?{data:rows}:{paged:true,data:[],projects:[]},()=>"codex");
 const page=await reader(new URLSearchParams({view:"roots"}));
 assert.equal(page.paged,false);assert.deepEqual(page.data,rows);
});

test("sending or reselecting during a native history read starts a fresh poll", async () => {
 const app=await fs.readFile(new URL("../server/public/app.js",import.meta.url),"utf8");
 const start=app.indexOf("async function loadClaudeTranscript(");
 const reads=[],timers=[];
 const context=vm.createContext({agent:{threadId:"s",selectionEpoch:1,threads:[{threadId:"s"}],activeTurns:new Map()},claudeHistoryJob:null,claudePollTimer:null,
  claudeRuntime:{history:()=>new Promise(resolve=>reads.push(resolve))},clearTimeout(){},setTimeout:callback=>timers.push(callback),document:{body:{dataset:{view:"agentView"}}}});
 vm.runInContext(app.slice(start,app.indexOf("\nfunction claudeQuestionForm",start)),context);
 const old=context.loadClaudeTranscript("s");
 context.agent.selectionEpoch++;
 const current=context.loadClaudeTranscript("s");
 assert.equal(reads.length,2,"new selection must not join an obsolete read");
 reads[0](null);await old;assert.equal(timers.length,0);
 reads[1](null);await current;assert.equal(timers.length,1,"current selection keeps polling");
});

test("account model IDs borrow effort levels from the SDK alias and expose the account default effort", async () => {
 const efforts=["low","medium","high","xhigh","max"];
 const catalog=[{value:"default",resolvedModel:"claude-opus-5-5[1m]",displayName:"Default",supportedEffortLevels:efforts},
  {value:"opus",resolvedModel:"claude-opus-5-5",displayName:"Opus 5.5",supportedEffortLevels:efforts},
  {value:"legacy",resolvedModel:"claude-legacy",displayName:"Legacy",supportsEffort:false}];
 const runtime=new ClaudeRuntime(async url=>url.endsWith("/describe")?{models:catalog}:{status:"ready"});
 const node=provider=>({nodeId:"wsl",nodeAccountId:"a",reportedAppServer:{provider}});
 const configured=await runtime.models(node({model:"claude-opus-5-5",effort:"xhigh"}));
 assert.equal(configured.defaultModel,"claude-opus-5-5");assert.equal(configured.configuredReasoningEffort,"xhigh");
 assert.deepEqual(configured.models[0],{model:"claude-opus-5-5",displayName:"Opus 5.5",description:"账号默认模型",supportedReasoningEfforts:efforts.map(reasoningEffort=>({reasoningEffort}))});
 const longContext=await runtime.models(node({model:"claude-opus-5-5[1m]"}));
 assert.equal(longContext.models[0].displayName,"Default");assert.equal(longContext.models[0].supportedReasoningEfforts.length,5);
 assert.equal(longContext.configuredReasoningEffort,null);
 assert.equal((await runtime.models(node({model:"opus"}))).models.length,3,"catalog values are not duplicated");
 const unknown=await runtime.models(node({model:"claude-unknown"}));
 assert.deepEqual(unknown.models[0].supportedReasoningEfforts,[]);
 assert.equal((await runtime.models(node({model:"legacy"}))).models.length,3);
});

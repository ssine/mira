import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs/promises";
import vm from "node:vm";
import { claudeTrace, ClaudeRuntime, claudeHistoryAcknowledgementRequired } from "../server/public/claude.js";
import { conversationPageReader } from "../server/public/conversation-pages.js";
import { accountGroups, accountNode } from "../server/public/codex-accounts.js";
import { AccountSpend } from "../server/public/account-spend.js";

test("API errors in success-shaped results show the provider error text", () => {
 const rows = [{seq:1,turnId:"turn",payload:{type:"result",subtype:"success",is_error:true,
  result:"API Error: Request rejected (429) · No available OAuth accounts in pool"}}];
 const error = claudeTrace(rows,{activeTurn:"turn"}).trace.find(item=>item.kind==="error");
 assert.match(error.body,/429.*No available OAuth accounts/);
 assert.notEqual(error.body,"success");
});

test("history acknowledgement is conservative for older Servers and distinct from historical completeness", () => {
  assert.equal(claudeHistoryAcknowledgementRequired({ persistence: "incomplete" }), true);
  assert.equal(claudeHistoryAcknowledgementRequired({ persistence: "incomplete", historyAcknowledgementRequired: false }), false);
  assert.equal(claudeHistoryAcknowledgementRequired({ persistence: "incomplete", historyAcknowledgementRequired: true }), true);
  assert.equal(claudeHistoryAcknowledgementRequired({ persistence: "saved" }), false);
});

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
 const view=claudeTrace(rows,{activeTurn:"turn"});
 assert.deepEqual(view.trace.filter(x=>x.kind==="assistant"&&x.body).map(x=>x.body),["full"]);
 assert.equal(view.trace.find(x=>x.kind==="assistant"&&x.body).completedAt,"2026-09-30T01:02:03.456Z");
 assert.equal(view.trace.find(x=>x.kind==="assistant"&&x.body).timingScope,"recorded");
 assert.equal(view.trace.filter(x=>x.kind==="tool").length,1);assert.match(view.trace.find(x=>x.kind==="tool").body,/output/);
 assert.equal(view.questions.size,1);
 assert.equal(claudeTrace([...rows,{seq:99,payload:{type:"mira_answer",questionId:"q",answers:{a:"yes"}}}]).questions.size,0);
});

test("questions remain visible outside tool groups and expire with their owning turn", () => {
 const question={seq:1,turnId:"turn",payload:{type:"mira_question",questionId:"q",questions:[{question:"Choose a target"}]}};
 const pending=claudeTrace([question],{activeTurn:"turn"});
 assert.equal(pending.trace[0].kind,"question");
 assert.equal(pending.trace[0].body,"Choose a target");
 assert.equal(pending.trace[0].questionState,"pending");
 assert.equal(pending.questions.size,1);
 for(const activeTurn of [null,"next-turn"]) {
  const ended=claudeTrace([question],{activeTurn});
  assert.equal(ended.questions.size,0);
  assert.equal(ended.trace[0].questionState,"cancelled");
 }
 for(const type of ["mira_completed","mira_interrupt_requested","mira_execution_stopped"]) {
  const ended=claudeTrace([question,{seq:2,turnId:"turn",payload:{type}}],{activeTurn:"turn"});
  assert.equal(ended.questions.size,0,"terminal events override a stale session snapshot");
 }
 const answered=claudeTrace([question,{seq:2,turnId:"turn",payload:{type:"mira_answer",questionId:"q",answers:{"Choose a target":"WSL"}}}]);
 assert.equal(answered.questions.size,0);
 assert.equal(answered.trace[0].questionState,"answered");
 assert.equal(answered.trace[0].body,"Choose a target\nWSL");
});

test("confirmed worker exit explains incomplete history in the transcript", () => {
 const view=claudeTrace([{seq:1,turnId:"turn",payload:{type:"mira_execution_stopped",degraded:true,message:"执行进程已退出，未收到完整的结束记录。"}}]);
 assert.equal(view.trace[0].kind,"error");
 assert.equal(view.trace[0].title,"执行已停止");
 assert.match(view.trace[0].body,/未收到完整的结束记录/);
});

test("reloading an unknown Claude turn retains its input reservation", async () => {
 const app=await fs.readFile(new URL("../server/public/app.js",import.meta.url),"utf8");
 const start=app.indexOf("function acceptThreadActivity(");
 const agent={persistedActivity:new Map(),activeTurns:new Map(),turnThreads:new Map(),threads:[]};
 const context=vm.createContext({agent,acceptThreadTokenUsage(){},rememberThreadCost(){},
  $:()=>({querySelector:()=>null}),threadActivity:id=>agent.persistedActivity.get(id)});
 vm.runInContext(app.slice(start,app.indexOf("\nfunction renderThreadStates",start)),context);
 context.acceptThreadActivity({threadId:"claude",engine:"claude",activity:{state:"unknown",turnId:"turn",generation:1,itemCount:5}});
 assert.equal(agent.activeTurns.get("claude"),"turn");
 assert.equal(agent.persistedActivity.get("claude").state,"unknown");
 context.acceptThreadActivity({threadId:"codex",engine:"codex",activity:{state:"unknown",turnId:"other",generation:1,itemCount:5}});
 assert.equal(agent.activeTurns.has("codex"),false);
});

test("tool calls carry readable activities, keep their images, and omit sub-agent task events", () => {
 const png = {type:"image",source:{type:"base64",media_type:"image/png",data:"AAAA"}};
 const use = (id,name,input,timestamp) => ({type:"assistant",uuid:`a-${id}`,timestamp,message:{id:`m-${id}`,content:[{type:"tool_use",id,name,input}]}});
 const done = (id,content,timestamp,extra={}) => ({type:"user",uuid:`u-${id}`,timestamp,...extra,message:{content:[{type:"tool_result",tool_use_id:id,content}]}});
 const rows = [
 {type:"system",subtype:"init",cwd:"/work/repo/"},
 {type:"mira_user",text:"go"},
 use("bash","Bash",{command:"go test ./...",description:"Run tests"},"2026-09-30T01:00:00Z"),
 done("bash","ok","2026-09-30T01:00:38Z"),
 use("edit","Edit",{file_path:"/work/repo/app.js",old_string:"a",new_string:"b"}),
 done("edit","updated",undefined,{tool_use_result:{filePath:"/work/repo/app.js",structuredPatch:[{lines:[" x","-a","+b","+c"]}]}}),
 use("write","Write",{file_path:"/elsewhere/new.md",content:"one\ntwo\n"}),
 done("write","created",undefined,{toolUseResult:{type:"create",content:"one\ntwo\n",structuredPatch:[]}}),
 use("shot","Read",{file_path:"/work/repo/shot.png"}),
 done("shot",[png,{type:"text",text:"image"}]),
 use("agent","Agent",{description:"调查超时",prompt:"…",subagent_type:"general-purpose"}),
 {type:"system",subtype:"task_started",task_id:"x",description:"调查超时"},
 {type:"system",subtype:"task_notification",task_id:"x",status:"completed",summary:"done"},
 done("agent","report"),
 use("mcp","mcp__home_nodes__process",{action:"list"}),
 {type:"assistant",uuid:"think",message:{id:"m-think",content:[{type:"thinking",thinking:"**Checking** the result"}]}},
 use("grep","Grep",{pattern:"TODO",path:"/work/repo/src"}),
 ].map((payload,seq)=>({seq,payload,turnId:"turn"}));
 const view = claudeTrace(rows,{activeTurn:"turn"});
 const tools = Object.fromEntries(view.trace.filter(x=>x.kind==="tool").map(x=>[x.key.slice(5),x.activity]));
 assert.deepEqual(tools.bash,{status:"completed",durationMs:38000,actions:[{kind:"run",label:"go test ./..."}]});
 assert.deepEqual(tools.edit.actions,[{kind:"edit",label:"app.js",added:2,removed:1}]);
 assert.deepEqual(tools.write.actions,[{kind:"create",label:"/elsewhere/new.md",added:2,removed:0}]);
 assert.deepEqual(tools.shot.actions,[{kind:"read",label:"shot.png"}]);
 assert.deepEqual(tools.agent.actions,[{kind:"agent",label:"调查超时"}]);
 assert.deepEqual(tools.mcp,{status:"running",durationMs:null,actions:[{kind:"tool",label:"home_nodes · process"}]});
 assert.deepEqual(tools.grep.actions,[{kind:"search",label:"“TODO”（src）"}]);
 assert.deepEqual(view.trace.find(x=>x.key==="tool:shot").nativeImages,["data:image/png;base64,AAAA"]);
 assert.equal(view.trace.some(x=>x.title==="图片"||x.title==="子任务"),false);
 assert.equal(view.trace.find(x=>x.kind==="reasoning").body,"**Checking** the result");
 // The turn ended without these results.
 const ended = claudeTrace(rows).trace.filter(x=>x.activity?.status==="interrupted").map(x=>x.key);
 assert.deepEqual(ended,["tool:mcp","tool:grep"]);
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

test("project directories are ordered by latest activity across engines", async () => {
 const reader=conversationPageReader(async url=>url.startsWith("/v1/claude/")
  ?{paged:true,data:[],projects:[{key:"claude-only",count:1,updatedAt:"2026-10-01T09:00:00Z"},{key:"shared",count:1,updatedAt:"2026-10-01T08:00:00Z"}]}
  :{paged:true,data:[],projects:[{key:"shared",count:2,updatedAt:"2026-09-30T00:00:00Z"},{key:"codex-only",count:1,updatedAt:"2026-10-01T08:30:00Z"}]},()=>"codex");
 const page=await reader(new URLSearchParams({view:"roots"}));
 assert.deepEqual(page.projects.map(p=>[p.key,p.count,p.updatedAt]),[["claude-only",1,"2026-10-01T09:00:00Z"],["codex-only",1,"2026-10-01T08:30:00Z"],["shared",3,"2026-10-01T08:00:00Z"]]);
});

test("lost native turn response reuses exactly the same request and account",async()=>{
 const requests=[];const runtime=new ClaudeRuntime(async (_url,options)=>{requests.push(JSON.parse(options.body));if(requests.length===1)throw new Error("lost");return{turnId:"ok"};});
 await assert.rejects(runtime.send({sessionId:"s"},{text:"one",nodeAccountId:"A"}));
 await runtime.send({sessionId:"s"},{text:"two",nodeAccountId:"B"});assert.deepEqual(requests[0],requests[1]);
});

test("added messages render where Claude read them and report unread ones", () => {
 const steer=(id,text)=>({type:"mira_steer",steerId:id,text,message:{role:"user",content:[{type:"text",text}]}});
 const lifecycle=(id,state)=>({type:"command_lifecycle",command_uuid:id,state});
 const reply=(id,text)=>({type:"assistant",uuid:id,message:{id,content:[{type:"text",text}]}});
 const rows=[
  {type:"mira_user",text:"start"},steer("read","read me"),lifecycle("read","queued"),steer("rejected","resent"),{type:"mira_steer_rejected",steerId:"rejected"},
  steer("waiting","later"),lifecycle("waiting","queued"),reply("a","first"),lifecycle("read","started"),reply("b","second"),
 ].map((payload,seq)=>({seq,payload,turnId:"turn"}));
 const users=view=>view.trace.filter(x=>x.kind==="user"||x.kind==="assistant").map(x=>[x.body,x.steerState]);
 assert.deepEqual(users(claudeTrace(rows,{activeTurn:"turn"})),[["start",undefined],["first",undefined],["read me","inserted"],["second",undefined],["later","queued"]]);
 // A stop cancels a waiting message; one never started by a turn that ended is also unread.
 assert.equal(claudeTrace(rows).trace.find(x=>x.key==="steer:waiting").steerState,"cancelled");
 const stopped=[...rows,{seq:20,turnId:"turn",payload:lifecycle("waiting","cancelled")},{seq:21,turnId:"turn",payload:reply("c","stopped")}];
 assert.deepEqual(users(claudeTrace(stopped,{activeTurn:"turn"})).slice(-2),[["later","cancelled"],["stopped",undefined]]);
 // A result that names the message places it when no start was recorded.
 const listed=[...rows,{seq:20,turnId:"turn",payload:{type:"result",user_message_uuids:["waiting"]}}];
 assert.equal(claudeTrace(listed).trace.find(x=>x.key==="steer:waiting").steerState,"inserted");
});

test("lost steer response retries the same request, and a finished turn clears it",async()=>{
 const requests=[];let fail=true;
 const runtime=new ClaudeRuntime(async (url,options)=>{requests.push([url,JSON.parse(options.body)]);if(fail){fail=false;throw new Error("lost");}return{accepted:true};});
 await assert.rejects(runtime.steer({sessionId:"s"},{expectedTurnId:"t",text:"one"}));
 await runtime.steer({sessionId:"s"},{expectedTurnId:"t",text:"two"});
 assert.deepEqual(requests[0],requests[1]);assert.equal(requests[0][0],"/v1/claude/sessions/s/steer");
 assert.equal(runtime.steerRequests.size,0);
 const finished=new ClaudeRuntime(async()=>{throw Object.assign(new Error("finished"),{status:409,code:"turn_not_steerable"});});
 await assert.rejects(finished.steer({sessionId:"s"},{expectedTurnId:"t",text:"one"}));assert.equal(finished.steerRequests.size,0);
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

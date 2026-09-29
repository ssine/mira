import assert from "node:assert/strict";
import test from "node:test";
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
 {type:"assistant",uuid:"final",message:{id:"m",content:[{type:"text",text:"full"},{type:"tool_use",id:"t",name:"Read",input:{path:"a"}}]}},
 {type:"user",message:{content:[{type:"tool_result",tool_use_id:"t",content:"output"}]}},
 {type:"assistant",parent_tool_use_id:"t",message:{content:[{type:"text",text:"child only"}]}},
 {type:"mira_question",questionId:"q",questions:[{question:"Choose"}]},
 {type:"result",duration_ms:100,total_cost_usd:1}
 ].map((payload,seq)=>({seq,payload,turnId:"turn"}));
 const view=claudeTrace(rows);
 assert.deepEqual(view.trace.filter(x=>x.kind==="assistant"&&x.body).map(x=>x.body),["full"]);
 assert.equal(view.trace.filter(x=>x.kind==="tool").length,1);assert.match(view.trace.find(x=>x.kind==="tool").body,/output/);
 assert.equal(view.questions.size,1);
 assert.equal(claudeTrace([...rows,{seq:99,payload:{type:"mira_answer",questionId:"q",answers:{a:"yes"}}}]).questions.size,0);
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

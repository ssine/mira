// Disposable question/answer API, serving the real Web assets and CSP.
import http from "node:http";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";

export const questionThread = "00000000-0000-4000-8000-0000000000a1";
export async function startQuestionFixture({ cacheHistory = false, images = false, compaction = false } = {}) {
  const nodeId = "00000000-0000-4000-8000-000000000001", accountId = "00000000-0000-4000-8000-000000000002";
  const turnId = "00000000-0000-4000-8000-000000000003", questionId = "00000000-0000-4000-8000-000000000004";
  const session = { sessionId: questionThread, nodeId, nodeAccountId: accountId, title: "Claude 问答回归", cwd: "/work", model: "opus", persistence: "pending", activeTurn: turnId };
  const node = { nodeId, hostname: "Question fixture", status: "online", platform: "linux", approvalStatus: "approved",
    capabilities: { claudeRuntimeV1: true, claudeAccountsV1: true }, desiredAppServer: { defaultCwd: "/work" },
    claudeAccounts: [{ nodeAccountId: accountId, name: "Claude fixture", enabled: true, configured: true, engine: "claude", reportedAppServer: { status: "ready" } }] };
  const events = [], answers = [], turns = [];
  let failAnswer = false, delayAnswer = false, heldAnswer;
  const emit = payload => events.push({ seq: events.length + 1, turnId, payload });
  emit({ type: "mira_user", text: "请先让我选择，再继续。" });
  const questions = [
    { header: "部署位置", question: "这次部署到哪里？", multiSelect: false, options: [{ label: "Home Server", description: "只更新服务端" }, { label: "WSL", description: "更新当前执行节点" }] },
    { header: "验证项目", question: "需要执行哪些检查？", multiSelect: true, options: [{ label: "单元测试", description: "检查逻辑" }, { label: "浏览器测试", description: "检查页面交互" }] },
    { header: "补充", question: "还有哪些要求？", options: [] },
  ];
  emit({ type: "mira_question", questionId, questions });
  emit({ type: "assistant", uuid: "tool-message", message: { id: "m", content: [{ type: "tool_use", id: "tool-question", name: "AskUserQuestion", input: { questions } }] } });
  if (cacheHistory) {
    events.length = 0;
    for (let i = 0; i < 80; i++) emit({ type: "assistant", uuid: `saved-${i}`, message: { id: `saved-${i}`, content: [{ type: "text", text: `已保存的回复 ${i}\n\n${"竖屏对话内容。".repeat(40)}` }] } });
  }
  if (images) {
    // A 1×1 PNG sent by the user and returned by a screenshot tool.
    const png = { type: "image", source: { type: "base64", media_type: "image/png", data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" } };
    events.length = 0;
    session.activeTurn = null;
    emit({ type: "mira_user", text: "看这张图", attachments: [{ name: "a.png", path: "/work/a.png" }], message: { role: "user", content: [{ type: "text", text: "看这张图" }, png] } });
    emit({ type: "assistant", uuid: "shot-use", message: { id: "shot", content: [{ type: "tool_use", id: "shot", name: "mcp__home_nodes__screen", input: { action: "screenshot" } }] } });
    emit({ type: "user", uuid: "shot-result", message: { role: "user", content: [{ type: "tool_result", tool_use_id: "shot", content: [png] }] } });
    emit({ type: "assistant", uuid: "reply", message: { id: "reply", content: [{ type: "text", text: "看到了。" }] } });
    emit({ type: "mira_completed" });
  }
  if (compaction) {
    events.length = 0;
    session.activeTurn = null;
    emit({ type: "mira_user", text: "继续发版" });
    emit({ type: "system", subtype: "status", status: "compacting" });
    emit({ type: "system", subtype: "compact_boundary", uuid: "boundary", compact_metadata: { trigger: "auto", pre_tokens: 167625, post_tokens: 5554, preserved_messages: { anchor_uuid: "summary" } } });
    emit({ type: "user", uuid: "summary", isSynthetic: true, message: { role: "user", content: [{ type: "text", text: "Summary:\n1. 推送 main 并发布新版本。" }] } });
    emit({ type: "assistant", uuid: "reply", message: { id: "reply", content: [{ type: "text", text: "继续处理。" }] } });
    emit({ type: "mira_completed" });
  }
  let holdHistory = false;
  const heldHistory = new Set();
  const summary = () => ({ ...session, threadId: questionThread, engine: "claude", runtimeNodeId: nodeId, generation: 1, listRoot: true, childCount: 0,
    itemCount: events.length, updatedAt: new Date().toISOString(), activity: { state: session.activeTurn ? "running" : "idle", turnId: session.activeTurn, generation: 1, itemCount: events.length } });
  const source = await fs.readFile(new URL("../node/internal/webassets/webassets.go", import.meta.url), "utf8");
  const csp = source.match(/const contentSecurityPolicy = "([^"]+)"/)[1];
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, "http://localhost"), path = url.pathname;
    const json = (value, status = 200) => { res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" }); res.end(JSON.stringify(value)); };
    let raw = ""; for await (const chunk of req) raw += chunk;
    const body = raw ? JSON.parse(raw) : {};
    if (path === "/__test/state") return json({ answers, turns, activeTurn: session.activeTurn, held: !!heldAnswer });
    if (path === "/__test/control") {
      if (body.action === "hold-history") holdHistory = true;
      if (body.action === "release-history") { holdHistory = false; for (const release of heldHistory) release(); heldHistory.clear(); }
      if (body.action === "append-history") emit({ type: "assistant", uuid: "new-saved", message: { id: "new-saved", content: [{ type: "text", text: "新保存的回复，缓存也应更新。" }] } });
      if (body.action === "fail") failAnswer = true;
      if (body.action === "hold") delayAnswer = true;
      if (body.action === "release") { delayAnswer = false; heldAnswer?.(); heldAnswer = null; }
      if (body.action === "poll") emit({ type: "system", subtype: "status", status: "waiting" });
      if (body.action === "question") emit({ type: "mira_question", questionId: `${questionId.slice(0, -1)}5`, questions: [questions[0]] });
      // No terminal event: reconciliation can clear the turn independently.
      if (body.action === "end") { session.activeTurn = null; session.persistence = "incomplete"; session.historyAcknowledgementRequired = true; }
      if (body.action === "complete") { session.activeTurn = null; emit({ type: "mira_completed" }); }
      return json({});
    }
    if (path === "/healthz") return json({ version: "test", adminConfigured: true });
    if (path === "/v1/admin/session") return json({ csrfToken: "fixture" });
    if (path === "/v1/nodes") return json({ data: [node] });
    if (path === `/v1/nodes/${nodeId}`) return json(node);
    if (path === "/v1/codex/threads") return json({ data: [], projects: [], paged: true });
    if (path.startsWith("/v1/codex/threads/")) return json({ error: "missing" }, 404);
    if (path === "/v1/claude/conversations") return json({ data: [summary()], projects: [{ key: JSON.stringify([nodeId, "/work"]), nodeId, cwd: "/work", count: 1 }], paged: true });
    if (path === `/v1/claude/conversations/${questionThread}`) return json(summary());
    if (path.includes("/runtimes/")) return json(path.endsWith("describe") ? { models: [{ value: "opus", displayName: "Claude" }] } : { status: "ready" });
    if (path.endsWith("/events")) {
      if (holdHistory) await new Promise(resolve => { heldHistory.add(resolve); res.once("close", () => { heldHistory.delete(resolve); resolve(); }); });
      const before = Number(url.searchParams.get("before")), after = Number(url.searchParams.get("after"));
      const data = events.filter(e => e.seq > after && (!before || e.seq < before));
      return json({ data, cursor: events.at(-1)?.seq || 0, earliest: data[0]?.seq || 0, hasMore: false, session });
    }
    if (path.endsWith("/answer")) {
      answers.push(body);
      if (failAnswer) { failAnswer = false; return json({ error: "测试连接失败" }, 503); }
      if (delayAnswer) await new Promise(resolve => { heldAnswer = resolve; });
      if (!session.activeTurn) return json({ error: "No active question" }, 409);
      emit({ type: "mira_answer", questionId: body.questionId, answers: body.answers });
      return json({ accepted: true });
    }
    if (path.endsWith("/turns")) {
      if (session.historyAcknowledgementRequired && !body.continueAcknowledgedHistory) return json({ error: "History acknowledgement required" }, 409);
      if (body.continueAcknowledgedHistory) session.historyAcknowledgementRequired = false;
      turns.push(body); session.activeTurn = body.requestId;
      return json({ turnId: body.requestId });
    }
    if (path.endsWith("/costs")) return json({ generation: 1, turnCostEstimates: {} });
    if (path.startsWith("/v1/")) return json({ data: [] });
    try {
      if (cacheHistory && path === "/__test/portrait") {
        res.writeHead(200, { "Content-Type": "text/html" });
        return res.end(`<iframe style="width:390px;height:844px;border:0" src="/?thread=${questionThread}&cacheTest=1"></iframe>`);
      }
      const resource = path === "/" ? "index.html" : path.slice(1);
      if (resource.includes("..")) throw Error("Invalid path");
      const file = path === "/__test/cache-scenarios.mjs" ? new URL("./claude_cache_scenarios.mjs", import.meta.url)
        : path === "/__test/scenarios.mjs" ? new URL("./claude_questions_scenarios.mjs", import.meta.url)
        : new URL(resource.startsWith("vendor/") ? `../node/internal/webassets/web/${resource}` : `../server/public/${resource}`, import.meta.url);
      res.writeHead(200, { "Content-Type": resource.endsWith(".css") ? "text/css" : /\.(js|mjs)$/.test(resource) ? "text/javascript" : "text/html",
        // The optional same-origin frame gives camoufoxctl a narrow viewport;
        // the normal question suite still validates the production CSP.
        "Content-Security-Policy": cacheHistory ? csp.replace("frame-ancestors 'none'", "frame-ancestors 'self'") : csp, "Cache-Control": "no-store" });
      let content = await fs.readFile(file);
      if (resource === "index.html" && url.searchParams.has("cacheTest")) content = content.toString().replace("</body>", '<script type="module" src="/__test/cache-scenarios.mjs"></script></body>');
      res.end(content);
    } catch { res.end(); }
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  return { origin: `http://127.0.0.1:${server.address().port}`, close: () => new Promise(resolve => server.close(resolve)) };
}
if (process.argv[1] === fileURLToPath(import.meta.url)) console.log((await startQuestionFixture({ cacheHistory: process.argv.includes("--cache") })).origin);

// Disposable question/answer API, serving the real Web assets and CSP.
import http from "node:http";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";

export const questionThread = "00000000-0000-4000-8000-0000000000a1";
export async function startQuestionFixture() {
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
      if (body.action === "fail") failAnswer = true;
      if (body.action === "hold") delayAnswer = true;
      if (body.action === "release") { delayAnswer = false; heldAnswer?.(); heldAnswer = null; }
      if (body.action === "poll") emit({ type: "system", subtype: "status", status: "waiting" });
      if (body.action === "question") emit({ type: "mira_question", questionId: `${questionId.slice(0, -1)}5`, questions: [questions[0]] });
      // No terminal event: reconciliation can clear the turn independently.
      if (body.action === "end") { session.activeTurn = null; session.persistence = "incomplete"; }
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
      turns.push(body); session.activeTurn = body.requestId;
      return json({ turnId: body.requestId });
    }
    if (path.endsWith("/costs")) return json({ generation: 1, turnCostEstimates: {} });
    if (path.startsWith("/v1/")) return json({ data: [] });
    try {
      const resource = path === "/" ? "index.html" : path.slice(1);
      if (resource.includes("..")) throw Error("Invalid path");
      const file = path === "/__test/scenarios.mjs" ? new URL("./claude_questions_scenarios.mjs", import.meta.url)
        : new URL(resource.startsWith("vendor/") ? `../node/internal/webassets/web/${resource}` : `../server/public/${resource}`, import.meta.url);
      res.writeHead(200, { "Content-Type": resource.endsWith(".css") ? "text/css" : /\.(js|mjs)$/.test(resource) ? "text/javascript" : "text/html",
        "Content-Security-Policy": csp, "Cache-Control": "no-store" });
      res.end(await fs.readFile(file));
    } catch { res.end(); }
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  return { origin: `http://127.0.0.1:${server.address().port}`, close: () => new Promise(resolve => server.close(resolve)) };
}
if (process.argv[1] === fileURLToPath(import.meta.url)) console.log((await startQuestionFixture()).origin);

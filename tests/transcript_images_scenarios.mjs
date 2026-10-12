const $ = selector => document.querySelector(selector);
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const check = (condition, message) => { if (!condition) throw Error(message); };
async function until(fn, message) {
  for (let i = 0; i < 300; i++) { if (await fn()) return; await delay(50); }
  throw Error(`Timed out: ${message}`);
}
const state = () => fetch("/__test/state").then(response => response.json());
const keys = () => [...document.querySelectorAll(".trace-card.image")].map(card => card.dataset.traceKey);
function verifyCompactions() {
  const card = $('[data-trace-key="compaction"]');
  check(card && !card.querySelector("details").open, "plaintext compaction is collapsed by default");
  check(card.querySelector(".compaction-expand").textContent === "展开", "notice offers an explicit expand action");
  check(!card.querySelector(".trace-body").checkVisibility(), "summary is not visible while collapsed");
  check(!$('[data-trace-key="compaction-summary"]'), "generated summary is not duplicated as ordinary prose");
  const encrypted = $('[data-trace-key="encrypted-compaction"]');
  check(encrypted.querySelector("details").hidden, "encrypted compaction does not offer an empty disclosure");
  check($('[data-trace-key="normal-reply"] .trace-body').textContent.trim() === "继续正常回复。", "normal prose before encrypted compaction remains visible");
  const { compactionSummaryKeys, mergeTranscriptItems } = window.transcriptRegression;
  const assistant = { key: "older", kind: "assistant", body: "summary", turnId: "turn", sourceItemSeq: 10 };
  const notice = { key: "newer", kind: "compaction", compactionSummary: "Handoff\nsummary", turnId: "turn", sourceItemSeq: 11 };
  check(compactionSummaryKeys([notice]).size === 0, "summary-only tail can render without older prose");
  check(compactionSummaryKeys(mergeTranscriptItems([notice], [assistant])).has("older"), "prepending an older page folds the duplicate summary");
  check(compactionSummaryKeys([{ ...assistant, turnId: "other" }, notice]).size === 0, "matching prose in another turn remains visible");
  check(compactionSummaryKeys([{ ...assistant, body: "normal answer" }, notice]).size === 0, "unrelated plaintext prose remains visible");
}
export async function verifyImages() {
  await until(() => document.querySelectorAll(".trace-card.image img:not([hidden])").length === 2, "images load without expanding tools");
  check(keys().join() === "history-2-image-0,history-4-image-0", "image order follows history, not download completion");
  check(!$(".tool-group").open, "images do not require expanded tool groups");
  const cards = [...document.querySelectorAll(".trace-card")];
  check(cards.findIndex(card => card.dataset.traceKey === "history-2-image-0") < cards.findIndex(card => card.textContent.includes("Between pictures")), "first image precedes later prose");
  const counters = await state();
  check(counters.nodeReads === 0, "never read paths from a Node");
  verifyCompactions();
  return { keys: keys(), nodeReads: counters.nodeReads };
}
export async function verifyToolOrdering() {
  const { agent, renderTranscript, upsertTrace, liveTraceKey, traceViewport } = window.transcriptRegression;
  const saved = agent.transcriptItems, trace = $("#conversationTrace");
  const order = () => [...trace.children].flatMap(row => row.classList.contains("tool-group")
    ? [...row._miraEntries.keys()] : row.dataset.traceKey ? [row.dataset.traceKey] : []);
  const nestedIds = [], canonical = [], expected = [];
  try {
    traceViewport().clear(); trace.replaceChildren();
    for (let segment = 0; segment < 30; segment++) {
      const turnId = `order-turn-${Math.floor(segment / 5)}`;
      for (const suffix of ["outer", "nested-a", "nested-b", "nested-c"]) {
        const id = `${segment}-${suffix}`, key = liveTraceKey({ turnId }, id);
        const card = upsertTrace(key, "tool", id, `result for ${id}`, "完成", { turnId, autoScroll: false });
        expected.push(key);
        if (suffix === "outer") canonical.push({ key: `stored-${id}`, itemId: id, turnId, kind: "tool", title: id,
          body: `result for ${id}`, status: "完成", sourceItemSeq: segment * 5 + 1 });
        else nestedIds.push(key);
        // Some cards remain mounted in open groups; others are detached. Both
        // must occupy the same position when canonical history replaces prose.
        if (segment % 3 === 0) card._miraToolGroup.open = true;
      }
      const key = `order-reply-${segment}`, body = `Reply for segment ${segment}`;
      upsertTrace(key, "assistant", "Codex", body, "完成", { turnId, autoScroll: false });
      expected.push(key);
      canonical.push({ key, turnId, kind: "assistant", title: "Codex", body, sourceItemSeq: segment * 5 + 5 });
    }
    await delay(80);
    check(order().join() === expected.join(), "initial tools follow their own conversation segments");
    agent.transcriptItems = canonical;
    for (let iteration = 0; iteration < 30; iteration++) {
      renderTranscript(null, { preserveLive: true, anchorBottom: false });
      check(order().join() === expected.join(), `reconciliation ${iteration + 1} retains tool positions and group order`);
      check(trace.querySelectorAll(".tool-group").length === 30, "unpersisted nested tools never form an extra tail group");
      check([...trace.querySelectorAll(".tool-group")].every(group => group._miraEntries.size === 4), "all 120 tools stay in their original groups");
    }
    // Canonical counterparts can appear later; consume each live tool once.
    agent.transcriptItems = mergeOrderedTools(canonical, nestedIds);
    renderTranscript(null, { preserveLive: true, anchorBottom: false });
    check(order().join() === expected.join(), "late canonical tools reconcile without duplication or movement");
    renderTranscript(null, { anchorBottom: false });
    check(order().join() === expected.join(), "history-only rebuilding has the same order as live rendering");
    return { turns: 6, tools: 120, reconciliations: 30 };
  } finally {
    agent.transcriptItems = saved;
    renderTranscript(null, { anchorBottom: true });
  }
}

function mergeOrderedTools(canonical, nestedIds) {
  const nested = nestedIds.map(key => {
    const [, turnId, itemId] = JSON.parse(key.slice("item-".length));
    const segment = Number(itemId.split("-")[0]);
    const offset = { "nested-a": 1, "nested-b": 2, "nested-c": 3 }[itemId.slice(itemId.indexOf("-") + 1)];
    return { key: `stored-${itemId}`, itemId, turnId, kind: "tool", title: itemId, body: `result for ${itemId}`,
      status: "完成", sourceItemSeq: segment * 5 + 1 + offset };
  });
  return [...canonical, ...nested].sort((left, right) => left.sourceItemSeq - right.sourceItemSeq);
}

export async function runTranscriptScenarios() {
  await verifyImages();
  const compaction = $('[data-trace-key="compaction"]'), compactionDetails = compaction.querySelector("details");
  compaction.querySelector("summary").click();
  await until(() => compactionDetails.open && compaction.querySelector(".trace-body h2")?.textContent === "压缩摘要", "expanding the notice reveals formatted summary");
  const imageNodes = [...document.querySelectorAll(".trace-card.image img")];
  const initialReads = (await state()).imageRequests;
  $(".tool-group").open = true;
  $("#conversationScroll").scrollTop = 0;
  await until(() => $(".trace-card.tool"), "nearby tool row mounts after opening its group");
  const tool = $(".trace-card.tool"), details = tool.querySelector(".trace-detail");
  details.open = true;
  await until(() => tool.querySelector(".trace-body").textContent.includes("revision 1"), "first detail load");
  const scroll = $("#conversationScroll");
  scroll.scrollTop = 1200;
  await delay(80);
  const before = scroll.scrollTop;
  check(before > 500, "long expanded tool has a reading position");
  await until(() => $(".trace-cost:not([hidden])")?.textContent === "费用 $0.21", "deferred turn cost loads");
  const missingCosts = [];
  const observer = new MutationObserver(() => {
    if ($(".trace-cost:not([hidden])")?.textContent !== "费用 $0.21") missingCosts.push($(".trace-cost:not([hidden])")?.textContent);
  });
  observer.observe($("#conversationTrace"), { childList: true, subtree: true, characterData: true });
  await fetch("/__test/state", { method: "POST", body: JSON.stringify({ version: 2 }) });
  await until(() => tool.querySelector(".trace-detail-status")?.textContent.includes("正在加载"), "new event hydrates open detail");
  check(tool.isConnected, "polling retains the expanded tool DOM");
  check(tool.querySelector(".trace-body").textContent.includes("revision 1"), "old body remains visible while loading");
  check(Math.abs(scroll.scrollTop - before) < 2, "polling does not reset scroll to the top");
  scroll.scrollTop = before + 180;
  const chosen = scroll.scrollTop;
  await until(() => tool.querySelector(".trace-body").textContent.includes("revision 2"), "updated detail arrives");
  await delay(100);
  check(Math.abs(scroll.scrollTop - chosen) < 2, "detail completion respects scrolling during the request");
  check(details.open && tool.closest(".tool-group").open, "expanded state survives updates");
  check(compaction.isConnected && compactionDetails.open, "expanded compaction survives history polling");
  compaction.querySelector("summary").click();
  check(!compactionDetails.open, "summary can be collapsed again at the notice");
  check(imageNodes.every((img, index) => img === document.querySelectorAll(".trace-card.image img")[index]), "decoded images survive reconciliation");
  check((await state()).imageRequests === initialReads, "updates do not reload unchanged images");
  check(keys().join() === "history-2-image-0,history-4-image-0", "details do not reorder images");
  observer.disconnect();
  check(missingCosts.length === 0, "polling keeps the loaded cost visible while refreshing estimates");
  await fetch("/__test/state", { method: "POST", body: JSON.stringify({ version: 3 }) });
  scroll.scrollTop = scroll.scrollHeight;
  const refresh = window.transcriptRegression.loadAgentTranscript(new URL(location.href).searchParams.get("thread"), null,
    { preserveLoaded: true, anchorBottom: true });
  scroll.scrollTop = 1400;
  const readingTop = scroll.scrollTop;
  await refresh;
  await delay(80);
  check(Math.abs(scroll.scrollTop - readingTop) < 2, "scrolling up during a tail request cancels bottom following");
  await fetch("/__test/state", { method: "POST", body: JSON.stringify({ version: 4 }) });
  scroll.scrollTop = scroll.scrollHeight;
  await window.transcriptRegression.loadAgentTranscript(new URL(location.href).searchParams.get("thread"), null,
    { preserveLoaded: true, anchorBottom: true });
  check(scroll.scrollHeight - scroll.clientHeight - scroll.scrollTop < 2, "tail reconciliation stays at bottom before the next paint");
  await until(() => $('[data-trace-key="normal-reply"] .trace-body').textContent.trim() === "继续正常回复。", "viewport prose mounts after tail reconciliation");
  verifyCompactions();
  const scrollAfter = scroll.scrollTop;
  const ordering = await verifyToolOrdering();
  return { scrollBefore: before, scrollAfter, imageReads: initialReads, keys: keys(), ordering };
}

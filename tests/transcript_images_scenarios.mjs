const $ = selector => document.querySelector(selector);
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const check = (condition, message) => { if (!condition) throw Error(message); };
async function until(fn, message) {
  for (let i = 0; i < 300; i++) { if (await fn()) return; await delay(50); }
  throw Error(`Timed out: ${message}`);
}
const state = () => fetch("/__test/state").then(response => response.json());
const keys = () => [...document.querySelectorAll(".trace-card.image")].map(card => card.dataset.traceKey);
export async function verifyImages() {
  await until(() => document.querySelectorAll(".trace-card.image img:not([hidden])").length === 2, "images load without expanding tools");
  check(keys().join() === "history-2-image-0,history-4-image-0", "image order follows history, not download completion");
  check(!$(".tool-group").open, "images do not require expanded tool groups");
  const cards = [...document.querySelectorAll(".trace-card")];
  check(cards.findIndex(card => card.dataset.traceKey === "history-2-image-0") < cards.findIndex(card => card.textContent.includes("Between pictures")), "first image precedes later prose");
  const counters = await state();
  check(counters.nodeReads === 0, "never read paths from a Node");
  return { keys: keys(), nodeReads: counters.nodeReads };
}
export async function runTranscriptScenarios() {
  await verifyImages();
  const imageNodes = [...document.querySelectorAll(".trace-card.image img")];
  const initialReads = (await state()).imageRequests;
  const tool = $(".trace-card.tool"), details = tool.querySelector(".trace-detail");
  tool.closest(".tool-group").open = true;
  details.open = true;
  await until(() => tool.querySelector(".trace-body").textContent.includes("revision 1"), "first detail load");
  const scroll = $("#conversationScroll");
  scroll.scrollTop = 1200;
  await delay(80);
  const before = scroll.scrollTop;
  check(before > 500, "long expanded tool has a reading position");
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
  check(imageNodes.every((img, index) => img === document.querySelectorAll(".trace-card.image img")[index]), "decoded images survive reconciliation");
  check((await state()).imageRequests === initialReads, "updates do not reload unchanged images");
  check(keys().join() === "history-2-image-0,history-4-image-0", "details do not reorder images");
  return { scrollBefore: before, scrollAfter: scroll.scrollTop, imageReads: initialReads, keys: keys() };
}

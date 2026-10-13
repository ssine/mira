const scroll = () => document.querySelector("#conversationScroll");
const trace = () => document.querySelector("#conversationTrace");
const gap = () => scroll().scrollHeight - scroll().clientHeight - scroll().scrollTop;
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const check = (condition, message) => { if (!condition) throw Error(message); };

export async function prepareScrollChecks() {
  while (!window.transcriptRegression?.agent.transcriptItems.length) await delay(40);
  const { conversationScroll } = await import("/conversation-scroll.js");
  conversationScroll(scroll()).reset();
  const { agent, renderTranscript } = window.transcriptRegression;
  agent.transcriptItems = Array.from({ length: 120 }, (_, index) => ({
    key: `scroll-reply-${index}`, kind: "assistant", turnId: `scroll-turn-${index}`,
    body: `Reply ${index}\n\n${"Readable prose. ".repeat(40)}`,
  }));
  renderTranscript(null, { anchorBottom: true });
  const deadline = Date.now() + 5000;
  while ((!trace().lastElementChild?._miraViewportVisible || gap() >= 2) && Date.now() < deadline) await delay(40);
  check(gap() < 2, "long conversation opens at the latest reply");
  check(trace().lastElementChild?._miraViewportVisible, "latest reply is mounted before streaming");
  check(trace().querySelectorAll(".trace-body").length === 120, "long transcript is rendered");
}

export function queueStreamUpdate() {
  const { queueTraceStreamRender } = window.transcriptRegression;
  const card = trace().lastElementChild, body = card.querySelector(".trace-body");
  const delta = `\n\n${"New streamed text.\n".repeat(3)}`;
  queueTraceStreamRender(card, `${body._miraSource}${delta}`, "assistant", true, delta);
}

export async function checkReadingPosition(top) {
  await delay(160);
  check(Math.abs(scroll().scrollTop - top) < 2, "small upward scroll survives a queued stream frame");
  // A newly observed placeholder forces the same viewport callback that runs
  // when a long conversation moves across a mounted/unmounted row boundary.
  const marker = document.createElement("div");
  marker.style.minHeight = "1px";
  trace().append(marker);
  const renderer = window.transcriptRegression.traceViewport();
  const reconciled = () => { marker.dataset.reconciled = "true"; };
  renderer.watch(marker, reconciled, reconciled);
  const deadline = Date.now() + 2000;
  while (!marker.dataset.reconciled && Date.now() < deadline) await delay(20);
  check(marker.dataset.reconciled, "viewport observer reconciles the tail row");
  await delay(80);
  // Rows above the reader can change measured height as they are unmounted.
  // Their anchor adjustment must never pull an upward scroll toward the tail.
  check(scroll().scrollTop <= top + 2 && gap() >= 38,
    "viewport reconciliation does not pull the reader back toward the bottom");
  renderer.forget(marker);
  marker.remove();
  const readingTop = scroll().scrollTop;
  window.transcriptRegression.upsertTrace("scroll-arriving", "assistant", "Codex", "An arriving reply.", "完成");
  await delay(80);
  check(Math.abs(scroll().scrollTop - readingTop) < 2, "new reply preserves the reading position");
}

export async function checkScrollResumes() {
  scroll().scrollTop = scroll().scrollHeight;
  await delay(120);
  window.transcriptRegression.upsertTrace("scroll-resumed", "assistant", "Codex", "Following resumes at the bottom.", "完成");
  await delay(160);
  check(gap() < 2, "returning to the bottom resumes automatic following");
  return "passed: small upward steps, queued stream, viewport reconciliation, arriving reply and resumed following";
}

// Camoufox's CLI evaluation runs the in-page half of the same regression.
export async function runScrollChecks() {
  await prepareScrollChecks();
  queueStreamUpdate();
  scroll().dispatchEvent(new WheelEvent("wheel", { deltaY: -40, bubbles: true }));
  // A wheel can arrive before a queued animation frame or the native scroll.
  await delay(40);
  check(gap() > 0, "upward wheel immediately cancels the queued bottom scroll");
  scroll().scrollTop -= 40;
  const top = scroll().scrollTop;
  await checkReadingPosition(top);
  scroll().scrollTop -= 40;
  const next = scroll().scrollTop;
  await delay(250);
  check(Math.abs(scroll().scrollTop - next) < 2, "slow repeated steps are retained");
  return checkScrollResumes();
}

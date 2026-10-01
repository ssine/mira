// Run in a portrait viewport: cached running history must paint before HTTP.
const $ = selector => document.querySelector(selector);
const assert = (condition, message) => { if (!condition) throw Error(message); };
const threadId = "00000000-0000-4000-8000-0000000000a1";
const control = action => fetch("/__test/control", { method: "POST", body: JSON.stringify({ action }) });
async function until(predicate, message, timeout = 7000) {
  const deadline = Date.now() + timeout;
  while (!await predicate()) {
    if (Date.now() > deadline) throw Error(message);
    await new Promise(resolve => setTimeout(resolve, 40));
  }
}
async function cached() {
  const { ClientCache } = await import("/client-cache.js");
  const cache = new ClientCache(); cache.setVersion("test");
  return cache.read(`transcript:${threadId}`);
}
const gap = () => { const s = $("#conversationScroll"); return s.scrollHeight - s.clientHeight - s.scrollTop; };
async function run() {
  await until(() => $("#conversationTrace").textContent.includes("已保存的回复 79"), "Latest reply did not load");
  await until(() => gap() < 100, "Initial portrait load started at old messages");
  assert(innerWidth === 390, "Fixture is not portrait");
  await until(async () => (await cached())?.rows.length === 80, "Running Claude history was never cached");
  await control("hold-history");
  $("#agentNewThread").click();
  $(`#agentThreadList button[data-thread-id="${threadId}"]`).click();
  await until(() => $("#conversationTrace").textContent.includes("已保存的回复 79"), "Reopen waited for HTTP despite its cached history");
  assert(gap() < 100, "Cached portrait history did not open at the latest reply");
  await control("release-history");
  await control("append-history");
  // Reopening must revalidate an active snapshot even if background polling
  // was paused (for example, when this scenario is inside the Camoufox frame).
  $(`#agentThreadList button[data-thread-id="${threadId}"]`).click();
  await until(() => $("#conversationTrace").textContent.includes("新保存的回复"), "Fresh reply did not arrive");
  await until(async () => (await cached())?.rows.some(row => row.payload.uuid === "new-saved"), "Running cache did not advance");
  return "passed: portrait first paint, active cache, reopen before HTTP, refreshed cache";
}
run().then(result => document.documentElement.dataset.cacheTest = result)
  .catch(async error => { document.documentElement.dataset.cacheTest = error.message; await control("release-history"); console.error(error); });

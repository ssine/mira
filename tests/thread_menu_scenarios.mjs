// Browser-native scenarios shared by CI and local Camoufox validation.
const a = "00000000-0000-4000-8000-0000000000a1";
const b = "00000000-0000-4000-8000-0000000000b2";
const $ = selector => document.querySelector(selector);
const assert = (condition, message) => { if (!condition) throw Error(message); };
const pause = () => new Promise(resolve => setTimeout(resolve, 560));
async function until(predicate, message) {
  const deadline = Date.now() + 10000;
  while (!predicate()) {
    if (Date.now() > deadline) throw Error(message);
    await new Promise(resolve => setTimeout(resolve, 20));
  }
}

export async function runThreadMenuScenarios() {
  const button = id => $(`[data-thread-id="${id}"]`);
  const row = id => $(`[data-thread-row="${id}"]`);
  await until(() => button(b) && !$("#conversationInput").disabled, "Conversations did not load");
  if ($("#agentThreadDrawer").inert) $("#agentThreadDrawerToggle").click();
  // Finish the opening animation before measuring gesture coordinates.
  await Promise.all($("#agentThreadDrawer").getAnimations().map(animation => animation.finished));
  button(b).scrollIntoView({ block: "nearest" });
  const menu = $("#threadOptionsMenu");
  const open = () => menu.matches(":popover-open");
  const selected = () => new URL(location.href).searchParams.get("thread");
  const pointer = (type, target = button(b), extra = {}) => {
    const bounds = target.getBoundingClientRect();
    return target.dispatchEvent(new PointerEvent(type, { bubbles: true, cancelable: true,
      pointerId: 71, pointerType: "touch", isPrimary: true, button: 0,
      buttons: type === "pointerup" ? 0 : 1,
      clientX: bounds.x + bounds.width / 2, clientY: bounds.y + bounds.height / 2, ...extra }));
  };
  const click = () => button(b).dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, detail: 1 }));
  const contextMenu = () => button(b).querySelector("strong").dispatchEvent(new MouseEvent("contextmenu", {
    bubbles: true, cancelable: true, clientX: 100, clientY: 200,
  }));

  assert(!$(".thread-menu-toggle, [data-thread-menu]"), "Per-row options still occupy space");
  assert(row(b).children.length === 1, "A conversation has an overlay or separate options column");
  await until(() => $(`[data-thread-cost="${b}"]`)?.textContent === "· $0.75", "Cost did not load");
  const cost = $(`[data-thread-cost="${b}"]`), bounds = cost.getBoundingClientRect();
  assert(document.elementFromPoint(bounds.right - 2, bounds.y + bounds.height / 2)?.closest("[data-thread-cost]") === cost,
    "The rightmost price is obscured");
  assert(!contextMenu(), "The browser context menu was not prevented");
  assert(open() && $("#threadOpenWindow").getAttribute("href") === `/?thread=${b}`, "Right-click opened the wrong conversation");
  assert(selected() === a, "Right-click also selected the conversation");
  menu.hidePopover();

  for (const key of ["F10", "ContextMenu"]) {
    button(b).focus();
    const accepted = button(b).dispatchEvent(new KeyboardEvent("keydown", { key, shiftKey: key === "F10", bubbles: true, cancelable: true }));
    assert(!accepted && open(), `${key} did not open options`);
    assert(selected() === a, "Keyboard options also selected the conversation");
    assert(document.activeElement === $("#threadShowDetails"), "Menu focus is not on a visible item");
    menu.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
    assert(document.activeElement === $("#threadRename"), "Keyboard navigation did not advance");
    menu.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
    menu.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true }));
    assert(document.activeElement === $("#threadFork"), "Keyboard navigation did not skip a hidden item");
    menu.hidePopover();
  }

  let opens = 0;
  menu.addEventListener("beforetoggle", event => { if (event.newState === "open") opens++; });
  pointer("pointerdown"); await pause();
  assert(open(), "A stationary long press did not open options");
  assert(selected() === a, "Long press selected the conversation");
  contextMenu();
  assert(opens === 1, "Native contextmenu opened the long-press menu twice");
  await pause(); // Suppression lasts until release, even after an extended hold.
  // Refresh can replace a list row while its menu is open. The release still
  // belongs to the same conversation and must not turn into navigation.
  row(b).replaceWith(row(b).cloneNode(true));
  pointer("pointerup");
  assert(!click(), "Long-press release click was not suppressed");
  assert(open() && selected() === a, "Long-press release navigated or dismissed the menu");
  menu.hidePopover();

  // A fresh tap must work immediately, rather than waiting out a cooldown.
  pointer("pointerdown"); pointer("pointerup"); click();
  await until(() => selected() === b && !$("#conversationInput").disabled, "A new tap after long press did not navigate");
  if ($("#agentThreadDrawer").inert) $("#agentThreadDrawerToggle").click();
  button(a).click();
  await until(() => selected() === a && !$("#conversationInput").disabled, "Could not restore the initial conversation");
  if ($("#agentThreadDrawer").inert) $("#agentThreadDrawerToggle").click();
  await Promise.all($("#agentThreadDrawer").getAnimations().map(animation => animation.finished));
  button(b).scrollIntoView({ block: "nearest" });

  const cancellations = [
    ["short tap", () => pointer("pointerup")],
    ["horizontal movement", () => pointer("pointermove", button(b), { clientX: button(b).getBoundingClientRect().x + button(b).offsetWidth / 2 - 16 })],
    ["vertical movement", () => pointer("pointermove", button(b), { clientY: button(b).getBoundingClientRect().y + button(b).offsetHeight / 2 + 16 })],
    ["scroll", () => $("#agentThreadList").dispatchEvent(new Event("scroll"))],
    ["cancelled contact", () => pointer("pointercancel")],
    ["lost capture", () => pointer("lostpointercapture")],
    ["second contact", () => pointer("pointerdown", document.body, { pointerId: 72, isPrimary: false })],
    ["resize", () => window.dispatchEvent(new Event("resize"))],
    ["blur", () => window.dispatchEvent(new Event("blur"))],
  ];
  for (const [label, cancel] of cancellations) {
    // Synthetic contacts have no native pointer capture. Use a pen to exercise
    // cancellation here; the CI runner separately delivers a real touch swipe.
    pointer("pointerdown", button(b), { pointerType: "pen" }); cancel(); await pause();
    assert(!open(), `${label} opened long-press options`);
    assert(selected() === a, `${label} changed the conversation`);
    pointer("pointerup");
  }
  // An early native touch menu cancels the timer and suppresses its release too.
  pointer("pointerdown"); contextMenu();
  const beforeNativeWait = opens;
  await pause();
  assert(open() && opens === beforeNativeWait, "The timer reopened an early native menu");
  pointer("pointerup"); assert(!click() && selected() === a, "Native touch menu release navigated");
  menu.hidePopover();

  // A pen and small contact jitter use the same long-press contract.
  pointer("pointerdown", button(b), { pointerType: "pen" });
  pointer("pointermove", button(b), { pointerType: "pen", clientX: button(b).getBoundingClientRect().x + button(b).offsetWidth / 2 + 3 });
  await pause();
  assert(open(), "Small pen movement cancelled the long press");
  pointer("pointerup", button(b), { pointerType: "pen" }); click(); menu.hidePopover();
  assert(selected() === a, "Pen long press also navigated");
  return "PASS: unobscured cost, right-click, keyboard, long press, native deduplication, release suppression, fresh taps and gesture cancellation";
}

if (new URL(import.meta.url).searchParams.get("run") === "1") {
  runThreadMenuScenarios().then(result => { document.documentElement.dataset.threadMenuTest = result; })
    .catch(error => { document.documentElement.dataset.threadMenuTest = `FAIL: ${error.message}`; });
}

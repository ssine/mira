// Share scroll intent across streaming, viewport rendering and layout changes.
const controllers = new WeakMap();

export function conversationScroll(root) {
  if (controllers.has(root)) return controllers.get(root);
  let paused = false, top = root.scrollTop;
  const gap = () => root.scrollHeight - root.clientHeight - root.scrollTop;
  const sync = () => {
    const current = root.scrollTop;
    if (current < top && gap() > 1) paused = true;
    else if (current > top && gap() <= 1) paused = false;
    top = current;
  };
  root.addEventListener("wheel", event => {
    // Cancel before the browser scrolls, including a render already queued for
    // this frame. A single small upward step is enough to stop following.
    if (event.deltaY < 0 && !event.ctrlKey && !event.shiftKey) paused = true;
  }, { passive: true });
  root.addEventListener("scroll", sync, { passive: true });
  const controller = {
    nearBottom(threshold = 96) { sync(); return !paused && gap() <= threshold; },
    toBottom() {
      if (paused) return;
      root.scrollTop = root.scrollHeight;
      top = root.scrollTop;
    },
    reset() { paused = false; root.scrollTop = 0; top = root.scrollTop; },
  };
  controllers.set(root, controller);
  return controller;
}

const routeKey = "mira.app.route";
const standalone = window.matchMedia("(display-mode: standalone)");
const isInstalled = () => standalone.matches || navigator.standalone === true;

function appRoute(url) {
  const thread = url.searchParams.get("thread");
  if (thread && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(thread)) {
    return `/?thread=${thread}`;
  }
  const view = url.searchParams.get("view");
  return ["agent", "runtime", "nodes"].includes(view) ? `/?view=${view}` : "/?view=nodes";
}

// Only a route is remembered. Transcripts and authentication stay on the Server.
export function rememberAppRoute() {
  try { localStorage.setItem(routeKey, appRoute(new URL(location.href))); } catch { /* optional storage */ }
}

export function clearAppRoute() {
  try { localStorage.removeItem(routeKey); } catch { /* optional storage */ }
}

function restoreAppLaunch() {
  const url = new URL(location.href);
  if (url.searchParams.get("launch") !== "pwa") return;
  let target = "/?view=agent";
  try {
    const saved = localStorage.getItem(routeKey);
    if (saved?.startsWith("/?")) target = appRoute(new URL(saved, location.origin));
  } catch { /* use a new conversation */ }
  // An explicit conversation link always wins over the saved launch route.
  if (url.searchParams.has("thread") || url.searchParams.has("view")) target = appRoute(url);
  history.replaceState(null, "", target);
}

function installControls() {
  let installPrompt;
  const controls = [...document.querySelectorAll("[data-install-app]")];
  const sync = () => controls.forEach((button) => button.classList.toggle("hidden", isInstalled() || !window.isSecureContext));
  const dialog = document.createElement("dialog");
  dialog.className = "pwa-install-dialog";
  const title = document.createElement("h2");
  title.textContent = "将 Mira 添加到桌面";
  title.id = "pwaInstallTitle";
  dialog.setAttribute("aria-labelledby", title.id);
  const description = document.createElement("p");
  const apple = /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
  description.textContent = apple
    ? "打开浏览器的分享菜单，选择「添加到主屏幕」，并以网页 App 打开。"
    : "在 Chrome 或 Edge 的菜单中选择「安装应用」或「添加到主屏幕」。若当前浏览器没有此选项，请用 Chrome 打开这个网址。";
  const close = document.createElement("button");
  close.type = "button";
  close.className = "primary";
  close.textContent = "知道了";
  close.addEventListener("click", () => dialog.close());
  dialog.append(title, description, close);
  document.body.append(dialog);
  for (const button of controls) button.addEventListener("click", async () => {
    rememberAppRoute();
    if (!installPrompt) { if (!dialog.open) dialog.showModal(); return; }
    const prompt = installPrompt;
    installPrompt = null;
    button.disabled = true;
    try { await prompt.prompt(); await prompt.userChoice; }
    catch { if (!dialog.open) dialog.showModal(); }
    finally { button.disabled = false; }
  });
  window.addEventListener("beforeinstallprompt", (event) => {
    event.preventDefault();
    installPrompt = event;
    sync();
  });
  window.addEventListener("appinstalled", () => {
    installPrompt = null;
    controls.forEach((button) => button.classList.add("hidden"));
    dialog.close();
  });
  standalone.addEventListener("change", sync);
  sync();
}

function mobileViewport() {
  const viewport = window.visualViewport;
  if (!viewport) return;
  const touch = window.matchMedia("(pointer: coarse)");
  let frame;
  const update = () => {
    frame = null;
    if (!touch.matches || Math.abs(viewport.scale - 1) > .05) return;
    const trace = document.querySelector("#conversationScroll");
    const follow = trace && trace.scrollHeight - trace.scrollTop - trace.clientHeight < 96;
    document.documentElement.style.setProperty("--app-viewport-height", `${viewport.height}px`);
    document.documentElement.style.setProperty("--app-viewport-top", `${viewport.offsetTop}px`);
    // Preserve a reader's position; follow the bottom only if already there.
    if (follow) trace.scrollTop = trace.scrollHeight;
  };
  const schedule = () => { frame ??= requestAnimationFrame(update); };
  viewport.addEventListener("resize", schedule);
  viewport.addEventListener("scroll", schedule);
  window.addEventListener("pageshow", schedule);
  touch.addEventListener("change", schedule);
  update();
}

export function initializePwa() {
  restoreAppLaunch();
  installControls();
  mobileViewport();
  if ("serviceWorker" in navigator && window.isSecureContext) {
    void navigator.serviceWorker.register("/service-worker.js", { scope: "/", updateViaCache: "none" }).catch(() => {
      // Installation/offline support is optional; ordinary online use still works.
    });
  }
}

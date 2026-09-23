const routeKey = "mira.app.route";
const standalone = window.matchMedia("(display-mode: standalone)");
let installedWindow = standalone.matches || navigator.standalone === true;
const isInstalled = () => installedWindow;

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
  standalone.addEventListener("change", () => {
    // Keep installed state across temporary display-mode changes.
    installedWindow ||= standalone.matches;
    sync();
  });
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

export function createCompletionNotifications(api, toast) {
  const controls = [...document.querySelectorAll("[data-completion-notifications]")];
  const preferenceKey = "mira.push.enabled";
  let authenticated = false, busy = false, enabled = false, epoch = 0;
  const supported = () => window.isSecureContext && "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  const wanted = () => { try { return localStorage.getItem(preferenceKey) === "true"; } catch { return enabled; } };
  const remember = (value) => { try { localStorage.setItem(preferenceKey, String(value)); } catch { /* optional storage */ } };
  const sync = () => controls.forEach((button) => {
    button.classList.remove("hidden");
    button.disabled = busy;
    button.textContent = enabled ? "关闭完成通知" : "开启完成通知";
    button.setAttribute(button.getAttribute("role") === "menuitemcheckbox" ? "aria-checked" : "aria-pressed", String(enabled));
  });
  const registration = async () => {
    let timer;
    try {
      return await Promise.race([navigator.serviceWorker.ready, new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error("通知服务尚未就绪，请稍后重试")), 10000);
      })]);
    } finally { clearTimeout(timer); }
  };
  const save = (subscription) => api("/v1/push/subscription", { method: "POST", body: JSON.stringify(subscription.toJSON()) });
  const refresh = async () => {
    if (!authenticated || busy || !supported()) return;
    const current = epoch;
    busy = true; sync();
    try {
      const sub = await (await registration()).pushManager.getSubscription();
      enabled = false;
      if (current !== epoch || !authenticated) return;
      if (wanted() && sub && Notification.permission === "granted") {
        await save(sub);
        if (current === epoch && authenticated) enabled = true;
      }
    } catch { /* Offline/session errors do not disable the saved preference. */ }
    finally { busy = false; sync(); }
  };
  for (const button of controls) button.addEventListener("click", async () => {
    if (!authenticated || busy) return;
    const current = epoch;
    const apple = /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
    if (apple && !isInstalled()) { toast("请先将 Mira 添加到主屏幕，再从主屏幕打开并开启通知（需要 iOS 16.4 或更新版本）"); return; }
    if (!supported()) { toast("当前浏览器不支持推送通知，请使用支持通知的浏览器并通过 HTTPS 打开 Mira"); return; }
    busy = true; sync();
    let created;
    try {
      if (enabled) {
        const sub = await (await registration()).pushManager.getSubscription();
        if (sub) await api("/v1/push/subscription", { method: "DELETE", body: JSON.stringify({ endpoint: sub.endpoint }) });
        remember(false); enabled = false;
        if (sub) await sub.unsubscribe();
        toast("已关闭这台设备的完成通知");
      } else {
        // iOS requires the permission request directly inside the user gesture.
        const permission = await Notification.requestPermission();
        if (permission !== "granted") {
          toast(permission === "denied" ? "通知已被禁止，请在系统或浏览器设置中允许 Mira 通知后重试" : "允许通知后，才能接收对话完成提醒");
          return;
        }
        const worker = await registration();
        const config = await api("/v1/push/config");
        const key = Uint8Array.from(atob(config.publicKey.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
        let sub = await worker.pushManager.getSubscription();
        const oldKey = sub?.options?.applicationServerKey;
        if (oldKey && String(new Uint8Array(oldKey)) !== String(key)) { await sub.unsubscribe(); sub = null; }
        if (!sub) { sub = await worker.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key }); created = sub; }
        if (!authenticated || current !== epoch) { await created?.unsubscribe(); return; }
        await save(sub);
        if (!authenticated || current !== epoch) { await created?.unsubscribe(); return; }
        enabled = true; remember(true);
        toast("已开启：每轮对话完成后，这台设备会收到通知");
      }
    } catch (error) {
      if (created) await created.unsubscribe().catch(() => {});
      toast(`通知设置失败：${error.message}`);
    } finally { busy = false; sync(); }
  });
  window.addEventListener("focus", () => { void refresh(); });
  sync();
  return {
    setAuthenticated(value) {
      if (authenticated === value) return;
      authenticated = value; epoch++;
      if (value) void refresh();
      else { enabled = false; sync(); }
    },
    async logout() {
      remember(false); enabled = false; authenticated = false; epoch++; sync();
      if (!supported()) return;
      const worker = await navigator.serviceWorker.getRegistration();
      const sub = await worker?.pushManager.getSubscription();
      await sub?.unsubscribe();
      for (const notification of await worker?.getNotifications() ?? []) notification.close();
    },
  };
}

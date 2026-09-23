// Android owns its notification transport. Only the configured HTTPS main frame
// receives this origin-scoped bridge; it never carries cookies or Node secrets.
export const nativeAndroid = () => typeof window.MiraAndroid?.postMessage === "function";
let sequence = 0;
const pending = new Map();
let attached;
export function androidRequest(method, values = {}) {
  if (!nativeAndroid()) return Promise.reject(new Error("Android 应用接口不可用"));
  if (attached !== window.MiraAndroid) {
    attached = window.MiraAndroid;
    attached.onmessage = (event) => {
      let value;
      try { value = JSON.parse(event.data); } catch { return; }
      const request = pending.get(value.id);
      if (!request) return;
      pending.delete(value.id); clearTimeout(request.timer);
      if (value.error) request.reject(new Error(value.error)); else request.resolve(value.result);
    };
  }
  return new Promise((resolve, reject) => {
    const id = String(++sequence);
    const timer = setTimeout(() => { pending.delete(id); reject(new Error("Android 未响应，请返回应用重试")); }, method === "downloadStart" ? 300000 : 30000);
    pending.set(id, { resolve, reject, timer });
    try { attached.postMessage(JSON.stringify({ id, method, ...values })); }
    catch (error) { clearTimeout(timer); pending.delete(id); reject(error); }
  });
}

export function installAndroidNavigation(openNotification) {
  if (!nativeAndroid()) return;
  window.addEventListener("mira:native-notification", (event) => {
    const threadId = event.detail;
    if (typeof threadId !== "string" || !/^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(threadId)) return;
    let accepted = false;
    try { accepted = openNotification?.(`/?thread=${threadId}`) === true; } catch { /* retain current editor */ }
    void androidRequest("navigation", { threadId, accepted }).catch(() => {});
  });
  document.addEventListener("click", (event) => {
    const link = event.target.closest?.("a[download]");
    if (!event.isTrusted || !link?.href.startsWith(`blob:${location.origin}/`)) return;
    event.preventDefault();
    void downloadBlob(link.href, link.download).catch(() => {});
  });
  void androidRequest("ready").catch(() => {});
}

export function createAndroidNotifications(api, toast) {
  const controls = [...document.querySelectorAll("[data-completion-notifications]")];
  let authenticated = false, enabled = false, busy = false, epoch = 0;
  const sync = () => controls.forEach((button) => {
    button.classList.remove("hidden"); button.disabled = busy;
    button.textContent = enabled ? "关闭完成通知" : "开启完成通知";
    button.setAttribute(button.getAttribute("role") === "menuitemcheckbox" ? "aria-checked" : "aria-pressed", String(enabled));
  });
  const save = (method, state) => api("/v1/push/node-subscription", { method, body: JSON.stringify({ nodeKey: state.nodeKey }) });
  const refresh = async () => {
    if (!authenticated || busy) return;
    const current = epoch; busy = true; sync();
    try {
      const state = await androidRequest("context");
      if (current !== epoch || !authenticated) return;
      enabled = false;
      if (state.enabled && state.permission === "granted") {
        await save("POST", state);
        if (current === epoch && authenticated) enabled = true;
      }
    } catch { /* Retain local opt-in through offline/authentication failures. */ }
    finally { busy = false; sync(); }
  };
  for (const button of controls) button.addEventListener("click", async () => {
    if (!authenticated || busy) return;
    const current = epoch; busy = true; sync();
    try {
      if (enabled) {
        const state = await androidRequest("setEnabled", { enabled: false });
        enabled = false;
        await save("DELETE", state);
        toast("已关闭这台设备的完成通知");
      } else {
        const state = await androidRequest("permission");
        if (current !== epoch || !authenticated) return;
        if (state.permission !== "granted") { toast("请在 Android 应用设置中允许 Mira 通知后重试"); return; }
        await save("POST", state);
        if (current !== epoch || !authenticated) return;
        await androidRequest("setEnabled", { enabled: true });
        if (current !== epoch || !authenticated) { await androidRequest("setEnabled", { enabled: false }); return; }
        enabled = true;
        toast("已开启完成通知，请允许 Mira 在后台运行");
      }
    } catch (error) { toast(`通知设置失败：${error.message}`); }
    finally { busy = false; sync(); }
  });
  window.addEventListener("focus", () => { void refresh(); });
  document.addEventListener("visibilitychange", () => { if (!document.hidden) void refresh(); });
  sync();
  return {
    setAuthenticated(value) {
      if (authenticated === value) return;
      authenticated = value; epoch++;
      if (value) void refresh(); else { enabled = false; sync(); }
    },
    async logout() {
      authenticated = false; enabled = false; epoch++; sync();
      await androidRequest("setEnabled", { enabled: false });
    },
  };
}

let blobDownloadRunning = false;
async function downloadBlob(url, name) {
  if (blobDownloadRunning) return;
  blobDownloadRunning = true;
  let reader;
  try {
    await androidRequest("downloadStart", { name });
    const response = await fetch(url);
    if (!response.ok || !response.body) throw new Error("无法读取下载文件");
    reader = response.body.getReader();
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      for (let offset = 0; offset < value.length; offset += 49152) {
        const chunk = value.subarray(offset, offset + 49152);
        await androidRequest("downloadChunk", { data: btoa(String.fromCharCode(...chunk)) });
      }
    }
    await androidRequest("downloadFinish");
  } catch (error) {
    await androidRequest("downloadCancel").catch(() => {});
    // Use the existing page's live toast surface without exposing bridge internals.
    const notice = document.querySelector("#toast");
    if (notice) { notice.textContent = error.message; notice.classList.remove("hidden"); }
  } finally { await reader?.cancel().catch(() => {}); blobDownloadRunning = false; }
}

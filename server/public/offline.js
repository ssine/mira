try { document.documentElement.dataset.theme = localStorage.getItem("mira.theme") === "dark" ? "dark" : "light"; } catch { /* optional preference */ }
document.querySelector('meta[name="theme-color"]').content = document.documentElement.dataset.theme === "dark" ? "#1f2226" : "#ffffff";
let checking = false;
async function reconnect() {
  if (checking || document.hidden) return;
  checking = true;
  document.querySelector("#status").textContent = "正在检查连接…";
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 5000);
  try {
    const response = await fetch("/healthz", { cache: "no-store", signal: controller.signal });
    if (response.ok) { location.reload(); return; }
  } catch { /* remain on the offline page with the original URL */ }
  finally { clearTimeout(timeout); checking = false; }
  document.querySelector("#status").textContent = "仍未连接，稍后会自动重试。";
}
document.querySelector("#retry").addEventListener("click", reconnect);
window.addEventListener("online", reconnect);
document.addEventListener("visibilitychange", () => { if (!document.hidden) void reconnect(); });
setInterval(reconnect, 10000);
void reconnect();

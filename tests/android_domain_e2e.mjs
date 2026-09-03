import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { isIP } from "node:net";

// request(path, body?) is an authenticated administrator or trusted Node client.
// Keep credentials out of this test and run against an explicitly approved device.
export async function verifyAndroidDomainNode({ request, serverUrl, nodeKey, expectedVersion, mode = "root", screen = true }) {
  const url = new URL(serverUrl);
  assert.equal(url.protocol, "https:", "acceptance requires HTTPS");
  assert.equal(isIP(url.hostname), 0, "use a domain to exercise Android system DNS");
  const node = (await request("/v1/nodes")).data.find(value => value.nodeKey === nodeKey);
  assert.ok(node, "Android Node must already be approved");
  assert.equal(node.platform, "android");
  assert.equal(node.status, "online");
  assert.equal(node.channelStatus.connected, true);
  assert.equal(node.nodeVersion, expectedVersion);
  assert.equal(node.capabilities.appServer, false, "Android should not advertise Codex App Server");
  const invoke = async (capability, params) => (await request(`/v1/nodes/${node.nodeId}/invoke`, {
    capability, params, timeoutMs: 45000,
  })).result;
  const status = await invoke("status", {});
  assert.equal(status.rootEnabled, mode === "root");
  assert.ok(status.memory.totalBytes > 0);
  const processes = await invoke("process", { action: "count" });
  assert.ok(processes.processCount > 0);
  const roots = await invoke("file", { action: "roots" });
  assert.ok(roots.roots.some(root => root.configured === "/"));
  const base = mode === "root" ? "/data/local/tmp" : "/data/user/0/com.ssine.codexnode/no_backup";
  const directory = `${base}/mira-apk-e2e-${randomUUID()}`;
  const file = `${directory}/中文.txt`;
  let created = false;
  let screenshotBytes = 0;
  let screenTap = false;
  let terminalProcess;
  try {
    await invoke("file", { action: "mkdir", path: directory });
    created = true;
    await invoke("file", { action: "write", path: file, content: "Mira Android 文件验证\n", overwrite: false });
    assert.equal((await invoke("file", { action: "read", path: file })).content, "Mira Android 文件验证\n");
    await invoke("file", { action: "move", path: file, destination: `${directory}/moved.txt`, overwrite: false });
    assert.equal((await invoke("file", { action: "stat", path: `${directory}/moved.txt` })).type, "file");
    const process = await invoke("process", { action: "start", command: "/system/bin/id", cwd: directory });
    let completed;
    for (let i = 0; i < 40; i++) {
      completed = await invoke("process", { action: "poll", processId: process.processId, cursor: 0 });
      if (!completed.running) break;
      await new Promise(resolve => setTimeout(resolve, 150));
    }
    assert.equal(completed.running, false);
    assert.equal(completed.exitCode, 0);
    const identity = completed.output.chunks.map(chunk => chunk.text).join("");
    assert.equal(identity.includes("uid=0(root)"), mode === "root");
    terminalProcess = await invoke("process", { action: "start", command: "/system/bin/sleep", args: ["30"], cwd: directory });
    await invoke("process", { action: "signal", processId: terminalProcess.processId, signal: "SIGTERM" });
    terminalProcess = null;
    if (screen) {
      const display = await invoke("screen", { action: "display" });
      assert.ok(display.width > 0 && display.height > 0);
      const screenshot = await invoke("screen", { action: "screenshot" });
      const png = Buffer.from(screenshot.content, "base64");
      assert.equal(png.subarray(0, 8).toString("hex"), "89504e470d0a1a0a");
      screenshotBytes = png.length;
      const hierarchy = await invoke("screen", { action: "hierarchy" });
      assert.ok(hierarchy.content.includes("com.ssine.codexnode"), "leave Mira foreground for safe UI acceptance");
      const tag = [...hierarchy.content.matchAll(/<node\b((?:"[^"]*"|[^">])*)>/g)]
        .find(match => /class="android.widget.Spinner"/.test(match[1]));
      const bounds = tag?.[1].match(/bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"/);
      assert.ok(bounds, "Mira privilege-mode selector must be visible");
      const [, x1, y1, x2, y2] = bounds.map(Number);
      await invoke("screen", { action: "tap", x: Math.floor((x1+x2)/2), y: Math.floor((y1+y2)/2) });
      try {
        const menu = await invoke("screen", { action: "hierarchy" });
        assert.ok(menu.content.includes("Root only") && menu.content.includes("App only"), "tap must open the actual selector");
        screenTap = true;
      } finally { await invoke("screen", { action: "key", keyCode: "KEYCODE_BACK" }); }
    }
    return { ok: true, version: node.nodeVersion, mode, domainEnrollment: true, reverseWSS: true,
      files: true, managedProcess: true, processCount: processes.processCount,
      memoryBytes: status.memory.totalBytes, screenshotBytes, screenTap, adbRequired: false };
  } finally {
    if (terminalProcess) await invoke("process", { action: "signal", processId: terminalProcess.processId, signal: "SIGTERM" }).catch(()=>{});
    if (created) await invoke("file", { action: "remove", path: directory, recursive: true });
  }
}

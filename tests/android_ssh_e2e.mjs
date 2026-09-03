import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { adminRequest, approvePendingNode } from "./auth_helpers.mjs";

// Invoked by a local device hook passed to ssh_e2e.mjs. The caller installs the
// APK, chooses its real root/app mode, and supplies authenticated requests to the
// existing Node. No production credential, device address or signing key lives here.
export async function verifyAndroidSSHNode({
  request, deviceNodeId, mode, binaryPath, privateDirectory,
  testServerUrl, deviceServerUrl, admin, cli, sourceNode, otherNode,
}) {
  assert(["root", "app"].includes(mode));
  assert(privateDirectory.startsWith("/") && privateDirectory !== "/");
  const invoke = async (capability, params) => (await request(`/v1/nodes/${deviceNodeId}/invoke`, { capability, params, timeoutMs: 30000 })).result;
  const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
  const root = `${privateDirectory}/ssh-acceptance-${mode}-${crypto.randomUUID()}`;
  const key = `android-ssh-${mode}-${crypto.randomUUID()}`;
  let managed;
  let created = false;
  try {
    const status = await invoke("status", {});
    assert.equal(status.native.uid === 0, mode === "root", "incorrect APK privilege mode");
    await invoke("file", { action: "mkdir", path: root }); created = true;
    await invoke("file", { action: "write", path: root+"/node.json", content: JSON.stringify({
      serverUrl: deviceServerUrl, nodeKey: key, identityFile: root+"/identity.json", allowedRoots: [root],
      privilegeMode: mode, appServerAutoStart: false, heartbeatSeconds: 1,
    }) });
    managed = await invoke("process", { action: "start", command: binaryPath, args: ["--config", root+"/node.json"], cwd: root });
    await approvePendingNode(testServerUrl, admin, key);
    let node;
    for (let i=0; i<100; i++) {
      node = (await adminRequest(testServerUrl, admin, "/v1/nodes")).data.find(n => n.nodeKey === key && n.channelStatus.connected);
      if (node) break;
      await sleep(300);
    }
    assert(node, "Android test Node did not connect");
    assert.equal(node.machineStatus.native.uid, status.native.uid);
    let result = await cli(sourceNode.identity, ["ssh", key, "--", "id; printf ANDROID_SSH_OK"]);
    assert.equal(result.code, 0, result.stderr); assert.match(result.stdout.toString(), /ANDROID_SSH_OK/);
    assert.match(result.stdout.toString(), mode === "root" ? /uid=0/ : /uid=10/);
    result = await cli(sourceNode.identity, ["ssh", "-t", key, "--", "stty size; printf ANDROID_PTY_OK"]);
    assert.equal(result.code, 0, result.stderr); assert.match(result.stdout.toString(), /24 80/); assert.match(result.stdout.toString(), /ANDROID_PTY_OK/);
    const payload = crypto.randomBytes(5*1024*1024+23);
    const local = path.join(sourceNode.root, "android-source.bin"); const restored = path.join(sourceNode.root, "android-restored.bin");
    await fs.writeFile(local, payload);
    result = await cli(sourceNode.identity, ["scp", local, `${key}:${root}/remote.bin`]); assert.equal(result.code, 0, result.stderr);
    result = await cli(sourceNode.identity, ["scp", `${key}:${root}/remote.bin`, restored]); assert.equal(result.code, 0, result.stderr);
    assert.deepEqual(await fs.readFile(restored), payload);
    const quote = value => "'"+value.replaceAll("'", "'\\''")+"'";
    result = await cli(sourceNode.identity, ["ssh", key, "--",
      `MIRA_IDENTITY_FILE=${quote(root+"/identity.json")} ${quote(binaryPath)} cli ssh ${quote(otherNode.key)} -- 'printf ANDROID_TO_LINUX_OK'`]);
    assert.equal(result.code, 0, result.stderr); assert.match(result.stdout.toString(), /ANDROID_TO_LINUX_OK/);
    if (mode === "app") {
      result = await cli(sourceNode.identity, ["ssh", key, "--", "ls /data/system"]);
      assert.notEqual(result.code, 0, "app SSH gained privileged access");
    }
    return { mode, uid: status.native.uid, ssh: true, nativePTY: true, androidToLinux: true, sftpBytes: payload.length, binaryMatch: true };
  } finally {
    if (managed) {
      await invoke("process", { action: "signal", processId: managed.processId, signal: "SIGTERM" });
      for (let i=0; i<50; i++) {
        const view = await invoke("process", { action: "poll", processId: managed.processId, cursor: 0 });
        if (!view.running) break;
        if (i === 49) throw new Error("Android test Node did not stop; test files retained for diagnosis");
        await sleep(100);
      }
    }
    if (created) await invoke("file", { action: "remove", path: root, recursive: true });
  }
}

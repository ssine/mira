import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";
import { SSHRelay, validSSHKey } from "../server/ssh-relay.mjs";

function publicKey() {
  const kind = Buffer.from("ssh-ed25519");
  const data = Buffer.alloc(51); data.writeUInt32BE(kind.length); kind.copy(data, 4); data.writeUInt32BE(32, 15); crypto.randomBytes(32).copy(data, 19);
  return "ssh-ed25519 "+data.toString("base64");
}
test("only canonical Ed25519 public keys can enter the registry", () => {
  const key = publicKey(); assert(validSSHKey(key));
  for (const invalid of [null, {}, "", "PRIVATE KEY", key+" comment", key+"\n", key.replace("ed25519", "rsa"), "ssh-ed25519 "+Buffer.alloc(51).toString("base64")]) assert.equal(validSSHKey(invalid), false);
});
test("key publication is credential-bound and immutable", async () => {
  const hostKey = publicKey(), clientKey = publicKey(); const calls = [];
  const relay = new SSHRelay({ pool: { query: async (sql, values) => { calls.push({ sql, values }); return { rows: [{ host_key: hostKey, client_key: clientKey }] }; } }, nodeChannel: {}, authService: {} });
  try {
    const principal = { nodeId: "node", credentialId: "authenticated-credential" };
    assert.equal((await relay.publish(principal, { hostKey, clientKey, credentialId: "attacker-selected" })).status, 200);
    assert.equal(calls[0].values[0], principal.credentialId);
    assert.equal((await relay.publish(principal, { hostKey: publicKey(), clientKey })).status, 409);
    assert.equal((await relay.publish(principal, { hostKey, clientKey: hostKey })).status, 400);
  } finally { relay.close(); }
});
test("disconnect/revocation closes both caller and target sessions, idempotently", () => {
  let destroyed = 0, terminated = 0, notified = 0;
  const relay = new SSHRelay({ pool: { query: async () => ({ rows: [] }) }, authService: {}, nodeChannel: { trySendToNode: () => { notified++; } } });
  for (const [id, source, target] of [["a","revoked","other"], ["b","other","revoked"], ["c","other","another"]]) {
    relay.sessions.set(id, { sessionId: id, sourceNodeId: source, targetNodeId: target, principal: {}, streams: { source: { destroy() { destroyed++; } } }, sockets: { target: { terminate() { terminated++; } } } });
  }
  assert.equal(relay.sessionCount("revoked"), 2);
  assert.equal(relay.sessionCount("other"), 3);
  relay.disconnectNode("revoked"); relay.disconnectNode("revoked");
  assert.equal(relay.sessionCount("revoked"), 0);
  assert.equal(relay.sessions.size, 1); assert.equal(destroyed, 2); assert.equal(terminated, 2); assert.equal(notified, 2);
  relay.close();
});

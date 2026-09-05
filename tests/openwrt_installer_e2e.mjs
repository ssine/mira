import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";

// Run the real installer and linked Node in an isolated BusyBox/musl filesystem.
// Only package feeds and the procd control API are fixtures; no host services,
// package managers, Node identities or network connections are used.
export async function testOpenWrtInstaller({ root, releases, current, previous }) {
  const fixture = await fs.mkdtemp(path.join(os.tmpdir(), "mira-openwrt-installer-"));
  const container = `mira-openwrt-installer-${randomUUID()}`;
  const docker = (args, options = {}) => execFileSync("docker", args, { encoding: "utf8", timeout: 30_000, stdio: "pipe", ...options });
  const exec = (args, options) => docker(["exec", container, ...args], options);
  const shell = (script) => exec(["sh", "-c", script]);
  const read = (file) => exec(["cat", file]);
  const exists = (file) => shell(`test -e '${file}' && echo yes || true`).trim() === "yes";
  const install = (...args) => exec(["sh", "/installer.sh", "--release-directory", "/releases", ...args]);
  const fail = (pattern, ...args) => assert.throws(() => install(...args), (error) => {
    assert.match(`${error.stdout}\n${error.stderr}`, pattern);
    return true;
  });
  const prefix = "/opt/mira user's $(touch INJECTED)";
  const nodePath = `${prefix}/bin/mira-node`;
  const pointer = () => exec(["readlink", nodePath]).trim();
  const checksum = (file) => exec(["sha256sum", file]).split(" ")[0];
  const reset = () => shell("if [ -x /etc/init.d/mira-node ]; then /etc/init.d/mira-node stop; fi; rm -rf /root/.local /root/.config /opt/* /etc/init.d/mira-node /tmp/mira-* /tmp/package-* /tmp/fail-*; cp /fixture/manager /usr/bin/opkg; chmod +x /usr/bin/opkg");
  try {
    await fs.writeFile(path.join(fixture, "manager"), `#!/bin/sh
set -eu
printf '%s %s\\n' "$(basename "$0")" "$*" >> /tmp/package-events
[ ! -e /tmp/fail-packages ] || exit 1
case "$1" in update) exit 0 ;; install|add) shift ;; *) exit 2 ;; esac
[ ! -e /tmp/incomplete-packages ] || exit 0
for package in "$@"; do
  case "$package" in
    curl) command=curl ;; diffutils) command=diff ;; script-utils) command=script ;;
    ca-bundle) mkdir -p /etc/ssl/certs; echo fixture > /etc/ssl/certs/ca-certificates.crt; continue ;;
    *) echo "Unexpected package: $package" >&2; exit 2 ;;
  esac
  if [ "$command" = script ]; then
    printf '#!/bin/sh\\necho "script from util-linux fixture"\\n' > "/usr/bin/$command"
  else
    printf '#!/bin/sh\\nexit 0\\n' > "/usr/bin/$command"
  fi
  chmod +x "/usr/bin/$command"
done
`);
    await fs.writeFile(path.join(fixture, "rc.common"), `#!/bin/sh
set -eu
initscript=$1
action=$2
printf '%s\\n' "$action" >> /tmp/mira-service-events
procd_open_instance() { :; }
procd_set_param() {
  kind=$1; shift
  case "$kind" in
    command) printf '%s\\n' "$@" > /tmp/mira-command ;;
    env) printf '%s\\n' "$@" > /tmp/mira-env ;;
    respawn|term_timeout|stdout|stderr) printf '%s %s\\n' "$kind" "$*" >> /tmp/mira-settings ;;
    *) exit 2 ;;
  esac
}
stop_node() {
  if [ -f /tmp/mira-pid ]; then kill "$(cat /tmp/mira-pid)" 2>/dev/null || true; rm -f /tmp/mira-pid; fi
}
procd_close_instance() {
  binary=$(cat /tmp/mira-command)
  if [ -f /tmp/fail-start ]; then
    case "$(readlink "$binary")" in *"/versions/$(cat /tmp/fail-start)/"*) return 0 ;; esac
  fi
  set --
  while IFS= read -r entry; do set -- "$@" "$entry"; done < /tmp/mira-env
  env "$@" "$binary" </dev/null >/tmp/mira-node-log 2>&1 &
  echo $! > /tmp/mira-pid
}
. "$initscript"
case "$action" in
  enable) touch /tmp/mira-enabled ;;
  disable) rm -f /tmp/mira-enabled ;;
  enabled) test -f /tmp/mira-enabled ;;
  restart|start) stop_node; start_service ;;
  stop) stop_node ;;
  running|status) test -f /tmp/mira-pid && kill -0 "$(cat /tmp/mira-pid)" ;;
  *) exit 2 ;;
esac
`);
    docker(["run", "--detach", "--name", container, "--network", "none", "--read-only", "--tmpfs", "/tmp:exec", "--tmpfs", "/root:exec", "--tmpfs", "/opt:exec", "--tmpfs", "/etc:exec", "--tmpfs", "/usr/bin:exec", "--tmpfs", "/sbin:exec", "--tmpfs", "/usr/sbin:exec",
      "--mount", `type=bind,src=${fixture},dst=/fixture,readonly`,
      "--mount", `type=bind,src=${releases},dst=/releases,readonly`,
      "--mount", `type=bind,src=${path.join(root, "scripts/install.sh")},dst=/installer.sh,readonly`,
      "alpine:3.22.2", "/bin/sh", "-c", "mkdir -p /etc/init.d /etc/ssl/certs; cp /fixture/rc.common /etc/rc.common; echo OpenWrt > /etc/openwrt_release; echo 'root:x:0:0:root:/root:/bin/sh' > /etc/passwd; echo 'root:x:0:' > /etc/group; /bin/busybox --install -s /usr/bin; cp /fixture/manager /usr/bin/opkg; chmod +x /usr/bin/opkg; printf '#!/bin/sh\\nexit 0\\n' > /sbin/procd; chmod +x /sbin/procd; exec /bin/sleep 600"]);
    for (let attempt = 0; attempt < 50 && !exists("/sbin/procd"); attempt++) await new Promise((resolve) => setTimeout(resolve, 100));

    // Dependency installation precedes publication and installs util-linux script
    // even though the Node itself is a static image with no shared library needs.
    shell("rm -f /usr/bin/curl /usr/bin/diff /usr/bin/script /etc/ssl/certs/ca-certificates.crt");
    install("--server", "http://127.0.0.1:9", "--version", previous, "--prefix", prefix, "--service", "procd");
    assert.equal(read("/tmp/package-events"), "opkg update\nopkg install curl diffutils script-utils ca-bundle\n");
    assert.equal(pointer(), `${prefix}/share/mira/versions/${previous}/mira-node`);
    assert.equal(read(`${prefix}/share/mira/service-manager`), "procd\n");
    assert.equal(read("/tmp/mira-command").trim(), nodePath);
    assert(exists("/tmp/mira-enabled"));
    assert(!exists("/INJECTED"), "prefix was interpreted as shell code");
    assert.match(read("/tmp/mira-settings"), /term_timeout 30/);
    const service = read("/etc/init.d/mira-node");
    const identity = checksum("/root/.config/mira/identity.json");
    const configuration = checksum("/root/.config/mira/node.json");
    assert.equal(JSON.parse(read("/root/.config/mira/node.json")).appServerAutoStart, false);
    assert(!exists("/root/.config/mira/runtimes"), "router install downloaded Codex");
    shell("rm /tmp/package-events");

    // CLI updates pass --prefix without --service. Persisted procd ownership must
    // survive this path, retaining the exact service environment and identity.
    install("--update", "--version", current, "--prefix", prefix);
    assert.equal(pointer(), `${prefix}/share/mira/versions/${current}/mira-node`);
    assert.equal(read("/etc/init.d/mira-node"), service);
    assert.equal(checksum("/root/.config/mira/identity.json"), identity);
    assert.equal(checksum("/root/.config/mira/node.json"), configuration);
    exec(["test", "-x", `${prefix}/share/mira/versions/${previous}/mira-node`]);
    assert(!exists("/tmp/package-events"), "already installed dependencies were reinstalled");
    const runningPid = read("/tmp/mira-pid");
    install("--update", "--version", current, "--prefix", prefix);
    assert.equal(read("/tmp/mira-pid"), runningPid, "identical reinstall interrupted the running Node");

    // Fail the requested version, then verify the previous service runs again.
    shell(`printf '%s' '${previous}' > /tmp/fail-start`);
    fail(/procd startup failed/, "--update", "--version", previous, "--prefix", prefix);
    assert.equal(pointer(), `${prefix}/share/mira/versions/${current}/mira-node`);
    assert.equal(read("/etc/init.d/mira-node"), service);
    assert(exists("/tmp/mira-enabled"));
    assert.equal(checksum("/root/.config/mira/identity.json"), identity);
    exec(["/etc/init.d/mira-node", "running"]);
    shell("rm /tmp/fail-start");
    exec(["/etc/init.d/mira-node", "start"]);
    fail(/another Mira installation/, "--server", "http://127.0.0.1:9", "--version", current, "--prefix", "/opt/other", "--service", "procd");
    exec(["/etc/init.d/mira-node", "stop"]);
    exec(["/etc/init.d/mira-node", "disable"]);
    shell(`printf '%s' '${previous}' > /tmp/fail-start`);
    fail(/procd startup failed/, "--update", "--version", previous, "--prefix", prefix);
    assert.equal(pointer(), `${prefix}/share/mira/versions/${current}/mira-node`);
    assert(!exists("/tmp/mira-pid"), "failed update started a previously stopped service");
    assert(!exists("/tmp/mira-enabled"), "failed update enabled a previously disabled service");

    reset();
    shell("printf '#!/bin/sh\\n# unrelated service\\n' > /etc/init.d/mira-node");
    fail(/independently managed service/, "--server", "http://127.0.0.1:9", "--version", current);
    assert(!exists("/root/.local/share/mira"));

    reset();
    shell("rm -f /usr/bin/script; touch /tmp/fail-packages");
    fail(/dependency installation failed/, "--server", "http://127.0.0.1:9", "--version", current);
    assert(!exists("/root/.local/share/mira"));
    assert(!exists("/etc/init.d/mira-node"));
    assert(!exists("/root/.config/mira/node.json"));

    reset();
    shell("touch /tmp/incomplete-packages");
    fail(/missing or incompatible.*script/, "--server", "http://127.0.0.1:9", "--version", current);
    assert(!exists("/root/.local/share/mira"));
    shell("rm /tmp/incomplete-packages");
    // A BusyBox-like script without util-linux flags is not a usable dependency.
    shell("printf '#!/bin/sh\\necho BusyBox\\n' > /usr/bin/script; chmod +x /usr/bin/script");

    // apk-based OpenWrt uses its own package-manager syntax. This fixture also
    // covers the existing prefix-only portable default and explicit opt-in later.
    reset();
    shell("mv /usr/bin/opkg /sbin/apk");
    install("--server", "http://127.0.0.1:9", "--version", current, "--prefix", "/opt/portable");
    assert.equal(read("/tmp/package-events"), "apk update\napk add script-utils\n");
    assert(!exists("/etc/init.d/mira-node"));
    assert(exists("/opt/portable/share/mira/no-service"));
    install("--update", "--version", previous, "--prefix", "/opt/portable");
    assert(!exists("/etc/init.d/mira-node"));
    install("--update", "--version", current, "--prefix", "/opt/portable", "--service", "procd");
    assert(exists("/etc/init.d/mira-node"));
    assert(!exists("/opt/portable/share/mira/no-service"));

    reset();
    shell(`printf '%s' '${current}' > /tmp/fail-start`);
    fail(/procd startup failed/, "--server", "http://127.0.0.1:9", "--version", current);
    assert(!exists("/etc/init.d/mira-node"));
    assert(!exists("/tmp/mira-enabled"));

    reset();
    install("--server", "http://127.0.0.1:9", "--version", current, "--no-service");
    install("--update", "--version", previous);
    assert(!exists("/etc/init.d/mira-node"), "--no-service was lost on update");
    shell("rm /root/.local/share/mira/no-service");
    install("--update", "--version", current);
    assert(!exists("/etc/init.d/mira-node"), "upgrade took over an older manually supervised installation");

    reset();
    install("--server", "http://127.0.0.1:9", "--version", current);
    assert(exists("/etc/init.d/mira-node"), "OpenWrt did not automatically select procd");
    assert.equal(read("/root/.local/share/mira/service-manager"), "procd\n");
    console.log("PASS OpenWrt installer: opkg/apk dependencies, procd startup/update/rollback, identity/config retention, quoted paths, service ownership, portable mode");
  } finally {
    try { docker(["rm", "--force", container]); } catch { /* Container may not have started. */ }
    await fs.rm(fixture, { recursive: true, force: true });
  }
}

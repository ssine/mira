import crypto from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const posix = path.posix;
const maxFileBytes = 4 * 1024 * 1024;
const maxScreenshotBytes = 10 * 1024 * 1024;
const maxAdbOutputBytes = 16 * 1024 * 1024;
const maxBufferedOutput = 1024 * 1024;
const maxProcessCount = 128;

function shellQuote(value) {
  return `'${String(value).replaceAll("'", `'"'"'`)}'`;
}

function contained(root, candidate) {
  const relative = posix.relative(root, candidate);
  return relative === "" || (!relative.startsWith("..") && !posix.isAbsolute(relative));
}

function integer(value, fallback, minimum, maximum) {
  return Number.isSafeInteger(value) && value >= minimum && value <= maximum ? value : fallback;
}

function stringArray(value) {
  if (
    !Array.isArray(value) ||
    value.length > 128 ||
    value.some(
      (item) => typeof item !== "string" || item.length > 32_768 || item.includes("\0"),
    )
  ) {
    throw new Error("args must be an array of at most 128 strings");
  }
  return value;
}

function commandError(error) {
  const stderr = Buffer.isBuffer(error.stderr) ? error.stderr.toString("utf8") : error.stderr;
  const stdout = Buffer.isBuffer(error.stdout) ? error.stdout.toString("utf8") : error.stdout;
  const detail = String(stderr || stdout || error.message).trim().slice(-2_000);
  return new Error(`ADB command failed: ${detail}`);
}

function configuredRoots() {
  const encoded = process.env.ANDROID_ADB_ALLOWED_ROOTS;
  const values = encoded ? JSON.parse(encoded) : ["/sdcard", "/data/local/tmp"];
  if (
    !Array.isArray(values) ||
    values.length === 0 ||
    values.length > 32 ||
    values.some((value) => typeof value !== "string" || !value.startsWith("/") || value.includes("\0"))
  ) {
    throw new Error("ANDROID_ADB_ALLOWED_ROOTS must be a non-empty JSON array of absolute paths");
  }
  return [...new Set(values.map((value) => posix.resolve(value)))];
}

function parseProperties(output) {
  const properties = {};
  for (const line of output.split("\n")) {
    const match = line.match(/^\[([^\]]+)\]: \[(.*)\]$/);
    if (match) properties[match[1]] = match[2];
  }
  return properties;
}

function parseDisplaySize(output) {
  const override = output.match(/Override size:\s*(\d+)x(\d+)/i);
  const physical = output.match(/Physical size:\s*(\d+)x(\d+)/i);
  const match = override ?? physical;
  return match ? { width: Number(match[1]), height: Number(match[2]) } : null;
}

function remoteStat(target, output) {
  const [kind, size, mode, modified, changed] = output.trim().split("\t");
  if (!kind || !/^\d+$/.test(size ?? "")) {
    throw new Error(`unexpected Android stat output for ${target}`);
  }
  return {
    path: target,
    type: kind.includes("directory")
      ? "directory"
      : kind.includes("regular file")
        ? "file"
        : kind.includes("symbolic link")
          ? "symlink"
          : "other",
    size: Number(size),
    mode,
    modifiedAt: new Date(Number(modified) * 1_000).toISOString(),
    changedAt: new Date(Number(changed) * 1_000).toISOString(),
  };
}

class OutputBuffer {
  constructor() {
    this.chunks = [];
    this.nextCursor = 0;
    this.size = 0;
  }

  push(stream, value) {
    const text = String(value);
    if (text.length === 0) return;
    const chunk = { cursor: this.nextCursor, stream, text };
    this.nextCursor += text.length;
    this.size += text.length;
    this.chunks.push(chunk);
    while (this.size > maxBufferedOutput && this.chunks.length > 1) {
      this.size -= this.chunks.shift().text.length;
    }
  }

  read(cursorValue) {
    const cursor = integer(cursorValue, 0, 0, Number.MAX_SAFE_INTEGER);
    const firstCursor = this.chunks[0]?.cursor ?? this.nextCursor;
    const effectiveCursor = Math.max(cursor, firstCursor);
    const chunks = [];
    for (const chunk of this.chunks) {
      const end = chunk.cursor + chunk.text.length;
      if (end <= effectiveCursor) continue;
      const skip = Math.max(0, effectiveCursor - chunk.cursor);
      chunks.push({ ...chunk, cursor: chunk.cursor + skip, text: chunk.text.slice(skip) });
    }
    return { cursor: this.nextCursor, lostOutput: cursor < firstCursor, chunks };
  }
}

export async function discoverAdbDevices(adbBinary = process.env.ADB_BINARY ?? "adb") {
  let stdout;
  try {
    ({ stdout } = await execFileAsync(adbBinary, ["devices", "-l"], {
      encoding: "utf8",
      timeout: 10_000,
      maxBuffer: maxAdbOutputBytes,
    }));
  } catch (error) {
    throw commandError(error);
  }
  return stdout
    .split("\n")
    .slice(1)
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      const [serial, state, ...fields] = line.split(/\s+/);
      return {
        serial,
        state,
        details: Object.fromEntries(
          fields.filter((field) => field.includes(":")).map((field) => field.split(/:(.*)/s).slice(0, 2)),
        ),
      };
    });
}

export class AndroidAdbCapabilityRuntime {
  constructor({ serial, adbBinary = process.env.ADB_BINARY ?? "adb", useRoot = false } = {}) {
    if (typeof serial !== "string" || serial.length === 0 || serial.length > 256) {
      throw new Error("an ADB serial is required");
    }
    this.serial = serial;
    this.adbBinary = adbBinary;
    this.useRoot = useRoot;
    this.roots = configuredRoots();
    this.realRootsPromise = null;
    this.processes = new Map();
    this.rootIdentity = null;
  }

  async initialize() {
    const state = await this.adbText(["get-state"]);
    if (state.trim() !== "device") throw new Error(`ADB device ${this.serial} is ${state.trim()}`);
    if (this.useRoot) {
      this.rootIdentity = (await this.shellText("id")).trim();
      if (!/uid=0\b/.test(this.rootIdentity)) {
        throw new Error("ANDROID_ADB_USE_ROOT is enabled but su did not grant uid 0");
      }
    }
    this.realRootsPromise = Promise.all(this.roots.map((root) => this.existingAncestor(root)));
    await this.realRootsPromise;
    return this;
  }

  adbArguments(args) {
    return ["-s", this.serial, ...args];
  }

  async adbBuffer(args, { timeout = 30_000, maxBuffer = maxAdbOutputBytes } = {}) {
    try {
      const { stdout } = await execFileAsync(this.adbBinary, this.adbArguments(args), {
        encoding: null,
        timeout,
        maxBuffer,
      });
      return stdout;
    } catch (error) {
      throw commandError(error);
    }
  }

  async adbText(args, options) {
    return (await this.adbBuffer(args, options)).toString("utf8").replace(/\r\n/g, "\n");
  }

  wrapRemote(command) {
    return this.useRoot ? `su -c ${shellQuote(command)}` : command;
  }

  async shellBuffer(command, options) {
    return this.adbBuffer(["shell", this.wrapRemote(command)], options);
  }

  async shellText(command, options) {
    return (await this.shellBuffer(command, options)).toString("utf8").replace(/\r\n/g, "\n");
  }

  async execOutBuffer(command, options) {
    return this.adbBuffer(["exec-out", this.wrapRemote(command)], options);
  }

  spawnShell(command) {
    return spawn(this.adbBinary, this.adbArguments(["shell", this.wrapRemote(command)]), {
      stdio: ["ignore", "pipe", "pipe"],
    });
  }

  async existingAncestor(input) {
    let current = posix.resolve(input);
    for (;;) {
      try {
        const resolved = (await this.shellText(`realpath -- ${shellQuote(current)}`)).trim();
        if (!resolved.startsWith("/")) throw new Error(`realpath returned a non-absolute path: ${resolved}`);
        return { input: current, real: posix.normalize(resolved) };
      } catch (error) {
        const parent = posix.dirname(current);
        if (parent === current) throw error;
        current = parent;
      }
    }
  }

  async authorize(input, { allowRoot = true } = {}) {
    if (
      typeof input !== "string" ||
      input.length === 0 ||
      input.length > 32_768 ||
      !input.startsWith("/") ||
      input.includes("\0")
    ) {
      throw new Error("path must be a non-empty absolute Android path");
    }
    const candidate = posix.resolve(input);
    const lexicalRoot = this.roots.find((root) => contained(root, candidate));
    if (!lexicalRoot) throw new Error(`path is outside configured Android roots: ${candidate}`);
    if (!allowRoot && candidate === lexicalRoot) throw new Error("operation on an allowed root is forbidden");

    const [ancestor, rootAncestors] = await Promise.all([
      this.existingAncestor(candidate),
      this.realRootsPromise,
    ]);
    const suffix = posix.relative(ancestor.input, candidate);
    const resolved = posix.resolve(ancestor.real, suffix);
    if (!rootAncestors.some((root) => contained(root.real, resolved))) {
      throw new Error(`path resolves outside configured Android roots: ${candidate}`);
    }
    return resolved;
  }

  async stat(target) {
    const output = await this.shellText(
      `stat -c '%F\t%s\t%a\t%Y\t%Z' -- ${shellQuote(target)}`,
    );
    return remoteStat(target, output);
  }

  async execute(capability, params) {
    if (params === null || typeof params !== "object" || Array.isArray(params)) {
      throw new Error("capability params must be an object");
    }
    if (capability === "status") return this.machineStatus();
    if (capability === "screen") return this.screen(params);
    if (capability === "file") return this.file(params);
    if (capability === "process") return this.process(params);
    throw new Error(`unsupported Android ADB capability: ${capability}`);
  }

  async displayInfo() {
    const [sizeOutput, densityOutput] = await Promise.all([
      this.shellText("wm size"),
      this.shellText("wm density"),
    ]);
    return {
      ...parseDisplaySize(sizeOutput),
      sizeOutput: sizeOutput.trim(),
      densityOutput: densityOutput.trim(),
    };
  }

  async screen(params) {
    if (params.action === "display") return { action: "display", ...(await this.displayInfo()) };
    if (params.action === "screenshot") {
      const [png, display] = await Promise.all([
        this.execOutBuffer("screencap -p", { timeout: 30_000, maxBuffer: maxScreenshotBytes }),
        this.displayInfo(),
      ]);
      if (png.length > maxScreenshotBytes) throw new Error("Android screenshot exceeds 10 MiB");
      if (png.subarray(0, 8).toString("hex") !== "89504e470d0a1a0a") {
        throw new Error("screencap did not return a PNG image");
      }
      return {
        action: "screenshot",
        mimeType: "image/png",
        encoding: "base64",
        content: png.toString("base64"),
        bytes: png.length,
        width: display.width,
        height: display.height,
        capturedAt: new Date().toISOString(),
      };
    }
    if (params.action === "hierarchy") {
      const temporary = `/data/local/tmp/codex-ui-${crypto.randomUUID()}.xml`;
      const command = [
        `uiautomator dump ${shellQuote(temporary)} >/dev/null`,
        `cat ${shellQuote(temporary)}`,
        `status=$?`,
        `rm -f ${shellQuote(temporary)}`,
        `exit $status`,
      ].join("; ");
      const xml = await this.shellText(command, { timeout: 30_000, maxBuffer: maxFileBytes });
      return { action: "hierarchy", format: "uiautomator-xml", content: xml };
    }

    const display = await this.displayInfo();
    const coordinate = (value, name, maximum) => {
      if (!Number.isSafeInteger(value) || value < 0 || (maximum && value >= maximum)) {
        throw new Error(`${name} must be an integer inside the current display`);
      }
      return value;
    };
    if (params.action === "tap") {
      const x = coordinate(params.x, "x", display.width);
      const y = coordinate(params.y, "y", display.height);
      await this.shellText(`input touchscreen tap ${x} ${y}`);
      return { action: "tap", x, y, accepted: true };
    }
    if (params.action === "swipe") {
      const startX = coordinate(params.startX, "startX", display.width);
      const startY = coordinate(params.startY, "startY", display.height);
      const endX = coordinate(params.endX, "endX", display.width);
      const endY = coordinate(params.endY, "endY", display.height);
      const durationMs = integer(params.durationMs, 300, 1, 60_000);
      await this.shellText(
        `input touchscreen swipe ${startX} ${startY} ${endX} ${endY} ${durationMs}`,
      );
      return { action: "swipe", startX, startY, endX, endY, durationMs, accepted: true };
    }
    if (params.action === "key") {
      const keyCode = params.keyCode;
      if (
        !(Number.isSafeInteger(keyCode) && keyCode >= 0 && keyCode <= 999) &&
        !(typeof keyCode === "string" && /^KEYCODE_[A-Z0-9_]+$/.test(keyCode))
      ) {
        throw new Error("keyCode must be an Android keycode integer or KEYCODE_* name");
      }
      await this.shellText(`input keyevent ${keyCode}`);
      return { action: "key", keyCode, accepted: true };
    }
    if (params.action === "text") {
      if (
        typeof params.text !== "string" ||
        params.text.length === 0 ||
        params.text.length > 4_096 ||
        params.text.includes("\0")
      ) {
        throw new Error("text must contain between 1 and 4096 characters");
      }
      const encoded = params.text.replaceAll(" ", "%s");
      await this.shellText(`input text ${shellQuote(encoded)}`);
      return { action: "text", characters: params.text.length, accepted: true };
    }
    throw new Error(`unsupported screen action: ${params.action}`);
  }

  async file(params) {
    if (params.action === "roots") {
      const realRoots = await this.realRootsPromise;
      return {
        roots: this.roots.map((root, index) => ({ configured: root, resolved: realRoots[index].real })),
      };
    }
    const target = await this.authorize(params.path, {
      allowRoot: !["move", "remove"].includes(params.action),
    });
    if (params.action === "stat") return this.stat(target);
    if (params.action === "list") {
      const targetStat = await this.stat(target);
      if (targetStat.type !== "directory") throw new Error("list target is not a directory");
      const output = await this.execOutBuffer(
        `find ${shellQuote(target)} -mindepth 1 -maxdepth 1 -print0`,
        { maxBuffer: maxAdbOutputBytes },
      );
      const paths = output.toString("utf8").split("\0").filter(Boolean);
      if (paths.length > 10_000) throw new Error("directory contains more than 10,000 entries");
      const entries = [];
      for (let index = 0; index < paths.length; index += 16) {
        entries.push(...(await Promise.all(paths.slice(index, index + 16).map((entry) => this.stat(entry)))));
      }
      return { path: target, entries };
    }
    if (params.action === "read") {
      const stat = await this.stat(target);
      if (stat.type !== "file") throw new Error("read target is not a regular file");
      const offset = integer(params.offset, 0, 0, Number.MAX_SAFE_INTEGER);
      const length = integer(
        params.length,
        Math.min(maxFileBytes, Math.max(0, stat.size - offset)),
        0,
        maxFileBytes,
      );
      const content = await this.execOutBuffer(
        `tail -c +${offset + 1} -- ${shellQuote(target)} | head -c ${length}`,
        { maxBuffer: maxFileBytes },
      );
      const encoding = params.encoding === "base64" ? "base64" : "utf8";
      return {
        path: target,
        offset,
        bytesRead: content.length,
        size: stat.size,
        encoding,
        content: content.toString(encoding),
        eof: offset + content.length >= stat.size,
      };
    }
    if (params.action === "write") {
      if (typeof params.content !== "string") throw new Error("content is required");
      const encoding = params.encoding === "base64" ? "base64" : "utf8";
      const content = Buffer.from(params.content, encoding);
      if (content.length > maxFileBytes) throw new Error("write exceeds 4 MiB");
      if (params.overwrite === false) {
        try {
          await this.stat(target);
          throw new Error(`destination already exists: ${target}`);
        } catch (error) {
          if (!error.message.includes("No such file") && !error.message.includes("cannot stat")) throw error;
        }
      }
      const localDirectory = await fs.mkdtemp(path.join(os.tmpdir(), "codex-adb-"));
      const localFile = path.join(localDirectory, "payload");
      const remoteFile = `/data/local/tmp/codex-adb-${crypto.randomUUID()}.tmp`;
      try {
        await fs.writeFile(localFile, content);
        await this.adbBuffer(["push", localFile, remoteFile], { timeout: 60_000 });
        await this.shellText(
          `cp ${shellQuote(remoteFile)} ${shellQuote(target)} && rm -f ${shellQuote(remoteFile)}`,
          { timeout: 60_000 },
        );
      } finally {
        await fs.rm(localDirectory, { recursive: true, force: true });
        await this.shellText(`rm -f ${shellQuote(remoteFile)}`).catch(() => {});
      }
      return { path: target, bytesWritten: content.length };
    }
    if (params.action === "mkdir") {
      await this.shellText(`${params.recursive === true ? "mkdir -p" : "mkdir"} -- ${shellQuote(target)}`);
      return { path: target, created: true };
    }
    if (params.action === "move") {
      const destination = await this.authorize(params.destination, { allowRoot: false });
      if (params.overwrite !== true) {
        try {
          await this.stat(destination);
          throw new Error(`destination already exists: ${destination}`);
        } catch (error) {
          if (!error.message.includes("No such file") && !error.message.includes("cannot stat")) throw error;
        }
      }
      await this.shellText(`mv -- ${shellQuote(target)} ${shellQuote(destination)}`);
      return { path: target, destination };
    }
    if (params.action === "remove") {
      await this.shellText(`${params.recursive === true ? "rm -r" : "rm"} -- ${shellQuote(target)}`);
      return { path: target, removed: true };
    }
    throw new Error(`unsupported file action: ${params.action}`);
  }

  ensureProcessCapacity() {
    for (const [id, entry] of this.processes) {
      if (this.processes.size < maxProcessCount) break;
      if (entry.exitCode !== null || entry.signal !== null) this.processes.delete(id);
    }
    if (this.processes.size >= maxProcessCount) {
      throw new Error(`process session limit of ${maxProcessCount} reached`);
    }
  }

  processView(id, entry, cursor = 0) {
    return {
      processId: id,
      pid: entry.remotePid,
      transportPid: entry.child.pid,
      command: entry.command,
      args: entry.args,
      cwd: entry.cwd,
      startedAt: entry.startedAt,
      exitCode: entry.exitCode,
      signal: entry.signal,
      running: entry.exitCode === null && entry.signal === null,
      output: entry.output.read(cursor),
    };
  }

  async startProcess(params) {
    this.ensureProcessCapacity();
    if (
      typeof params.command !== "string" ||
      params.command.length === 0 ||
      params.command.length > 4_096 ||
      params.command.includes("\0")
    ) {
      throw new Error("command is required");
    }
    const args = stringArray(params.args ?? []);
    const cwd = await this.authorize(params.cwd ?? this.roots[0]);
    const extraEnv = params.env ?? {};
    if (
      extraEnv === null ||
      typeof extraEnv !== "object" ||
      Array.isArray(extraEnv) ||
      Object.keys(extraEnv).length > 100 ||
      Object.entries(extraEnv).some(
        ([name, value]) =>
          !/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) ||
          typeof value !== "string" ||
          value.includes("\0"),
      )
    ) {
      throw new Error("env must contain at most 100 valid string environment values");
    }
    const environmentCommand = Object.keys(extraEnv).length
      ? `env ${Object.entries(extraEnv)
          .map(([name, value]) => `${name}=${shellQuote(value)}`)
          .join(" ")} `
      : "";
    const marker = `__CODEX_REMOTE_PID_${crypto.randomUUID()}__`;
    const command = [params.command, ...args].map(shellQuote).join(" ");
    const script =
      `printf '${marker}%s\\n' "$$"; ` +
      `cd ${shellQuote(cwd)} && exec ${environmentCommand}${command}`;
    const child = this.spawnShell(script);
    const id = crypto.randomUUID();
    const entry = {
      child,
      command: params.command,
      args,
      cwd,
      startedAt: new Date().toISOString(),
      remotePid: null,
      exitCode: null,
      signal: null,
      output: new OutputBuffer(),
      markerBuffer: "",
    };
    this.processes.set(id, entry);
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      if (entry.remotePid === null) {
        entry.markerBuffer += chunk;
        const newline = entry.markerBuffer.indexOf("\n");
        if (newline === -1) return;
        const firstLine = entry.markerBuffer.slice(0, newline);
        const pid = firstLine.startsWith(marker) ? Number(firstLine.slice(marker.length)) : NaN;
        if (Number.isSafeInteger(pid) && pid > 0) entry.remotePid = pid;
        else entry.output.push("stdout", `${firstLine}\n`);
        entry.output.push("stdout", entry.markerBuffer.slice(newline + 1));
        entry.markerBuffer = "";
        return;
      }
      entry.output.push("stdout", chunk);
    });
    child.stderr.on("data", (chunk) => entry.output.push("stderr", chunk));
    child.on("error", (error) => entry.output.push("error", error.message));
    child.on("exit", (code, signal) => {
      if (entry.markerBuffer) entry.output.push("stdout", entry.markerBuffer);
      entry.markerBuffer = "";
      entry.exitCode = code;
      entry.signal = signal;
    });
    const deadline = Date.now() + 1_000;
    while (entry.remotePid === null && entry.exitCode === null && Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    return this.processView(id, entry);
  }

  async process(params) {
    if (params.action === "list" && params.system === true) {
      const output = await this.shellText(
        "ps -A -o PID,PPID,USER,STAT,NAME,ARGS",
        { timeout: 10_000, maxBuffer: maxBufferedOutput },
      );
      return { format: "android-ps", output };
    }
    if (params.action === "list") {
      return { processes: [...this.processes].map(([id, entry]) => this.processView(id, entry)) };
    }
    if (params.action === "start") return this.startProcess(params);
    const entry = this.processes.get(params.processId);
    if (!entry) throw new Error("managed Android process not found");
    if (params.action === "poll") return this.processView(params.processId, entry, params.cursor);
    if (params.action === "signal") {
      const signal = params.signal ?? "SIGTERM";
      if (!["SIGINT", "SIGTERM", "SIGKILL"].includes(signal)) throw new Error("invalid signal");
      if (entry.remotePid) {
        await this.shellText(`kill -${signal.slice(3)} ${entry.remotePid}`);
      } else {
        entry.child.kill(signal);
      }
      return { processId: params.processId, pid: entry.remotePid, signal, accepted: true };
    }
    throw new Error(`unsupported process action: ${params.action}`);
  }

  async deviceInfo() {
    const properties = parseProperties(await this.shellText("getprop"));
    return {
      serial: this.serial,
      manufacturer: properties["ro.product.manufacturer"] ?? null,
      model: properties["ro.product.model"] ?? null,
      device: properties["ro.product.device"] ?? null,
      release: properties["ro.build.version.release"] ?? null,
      sdk: properties["ro.build.version.sdk"] ?? null,
      fingerprint: properties["ro.build.fingerprint"] ?? null,
      abi: properties["ro.product.cpu.abi"] ?? null,
    };
  }

  async machineStatus() {
    const [device, identity, uptime, memory, disk, battery, display, focus, networks] = await Promise.all([
      this.deviceInfo(),
      this.shellText("id"),
      this.shellText("cat /proc/uptime"),
      this.shellText("cat /proc/meminfo"),
      this.shellText(`df -kP ${this.roots.map(shellQuote).join(" ")}`),
      this.shellText("dumpsys battery", { maxBuffer: maxBufferedOutput }),
      this.displayInfo(),
      this.shellText("dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -4"),
      this.shellText("ip -brief address 2>/dev/null || ip address", { maxBuffer: maxBufferedOutput }),
    ]);
    const memoryValue = (name) =>
      Number(memory.match(new RegExp(`^${name}:\\s+(\\d+)`, "m"))?.[1] ?? 0) * 1_024;
    return {
      sampledAt: new Date().toISOString(),
      hostname: `${device.manufacturer ?? "android"}-${device.model ?? device.device ?? this.serial}`,
      platform: "android",
      release: device.release,
      architecture: device.abi,
      uptimeSeconds: Number.parseFloat(uptime),
      memory: {
        totalBytes: memoryValue("MemTotal"),
        freeBytes: memoryValue("MemFree"),
        availableBytes: memoryValue("MemAvailable"),
      },
      adb: { serial: this.serial, transport: "adb", identity: identity.trim() },
      device,
      display,
      disk: { format: "df-kP", output: disk.trim() },
      battery: battery.trim(),
      focus: focus.trim(),
      networks: networks.trim(),
      allowedRoots: this.roots,
      rootEnabled: this.useRoot,
      rootIdentity: this.rootIdentity,
      managedProcesses: this.processes.size,
      processLimit: maxProcessCount,
    };
  }

  async close() {
    for (const entry of this.processes.values()) {
      if (entry.exitCode === null) {
        if (entry.remotePid) {
          await this.shellText(`kill -TERM ${entry.remotePid}`).catch(() => {});
        }
        entry.child.kill("SIGTERM");
      }
    }
  }
}

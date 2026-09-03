import crypto from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const maxFileReadBytes = 4 * 1024 * 1024;
const maxBufferedOutput = 1024 * 1024;
const maxSessionCount = 128;
const maxPtyInputBytes = 1024 * 1024;

function contained(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === "" || (!relative.startsWith("..") && !path.isAbsolute(relative));
}

function integer(value, fallback, minimum, maximum) {
  return Number.isSafeInteger(value) && value >= minimum && value <= maximum ? value : fallback;
}

function stringArray(value) {
  if (!Array.isArray(value) || value.length > 128 || value.some((item) => typeof item !== "string")) {
    throw new Error("args must be an array of at most 128 strings");
  }
  return value;
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
    const lostOutput = cursor < firstCursor;
    const effectiveCursor = Math.max(cursor, firstCursor);
    const chunks = [];
    for (const chunk of this.chunks) {
      const end = chunk.cursor + chunk.text.length;
      if (end <= effectiveCursor) continue;
      const skip = Math.max(0, effectiveCursor - chunk.cursor);
      chunks.push({ ...chunk, text: chunk.text.slice(skip), cursor: chunk.cursor + skip });
    }
    return { cursor: this.nextCursor, lostOutput, chunks };
  }
}

async function existingAncestor(candidate) {
  let current = candidate;
  for (;;) {
    try {
      return await fs.realpath(current);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
      const parent = path.dirname(current);
      if (parent === current) throw error;
      current = parent;
    }
  }
}

function configuredRoots() {
  const encoded = process.env.NODE_AGENT_ALLOWED_ROOTS;
  const values = encoded ? JSON.parse(encoded) : [process.cwd(), os.homedir()];
  if (
    !Array.isArray(values) ||
    values.length === 0 ||
    values.length > 32 ||
    values.some((value) => typeof value !== "string" || value.length === 0)
  ) {
    throw new Error("NODE_AGENT_ALLOWED_ROOTS must be a non-empty JSON array of paths");
  }
  return [...new Set(values.map((value) => path.resolve(value)))];
}

function statResult(targetPath, stat) {
  return {
    path: targetPath,
    type: stat.isDirectory()
      ? "directory"
      : stat.isFile()
        ? "file"
        : stat.isSymbolicLink()
          ? "symlink"
          : "other",
    size: stat.size,
    mode: stat.mode,
    modifiedAt: stat.mtime.toISOString(),
    createdAt: stat.birthtime.toISOString(),
  };
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", `'"'"'`)}'`;
}

export class CapabilityRuntime {
  constructor() {
    this.roots = configuredRoots();
    this.realRootsPromise = Promise.all(this.roots.map((root) => existingAncestor(root)));
    this.processes = new Map();
    this.ptys = new Map();
  }

  ensureSessionCapacity(collection, kind) {
    for (const [id, entry] of collection) {
      if (collection.size < maxSessionCount) break;
      if (entry.exitCode !== null || entry.signal !== null) collection.delete(id);
    }
    if (collection.size >= maxSessionCount) {
      throw new Error(`${kind} session limit of ${maxSessionCount} reached`);
    }
  }

  async authorize(input, { allowRoot = true } = {}) {
    if (typeof input !== "string" || input.length === 0 || input.length > 32_768) {
      throw new Error("path must be a non-empty string");
    }
    const candidate = path.resolve(input);
    const lexicalRoot = this.roots.find((root) => contained(root, candidate));
    if (!lexicalRoot) throw new Error(`path is outside configured roots: ${candidate}`);
    if (!allowRoot && candidate === lexicalRoot) throw new Error("operation on an allowed root is forbidden");
    const [candidateReal, realRoots] = await Promise.all([
      existingAncestor(candidate),
      this.realRootsPromise,
    ]);
    if (!realRoots.some((root) => contained(root, candidateReal))) {
      throw new Error(`path resolves outside configured roots: ${candidate}`);
    }
    return candidate;
  }

  async execute(capability, params) {
    if (params === null || typeof params !== "object" || Array.isArray(params)) {
      throw new Error("capability params must be an object");
    }
    if (capability === "status") return this.machineStatus();
    if (capability === "file") return this.file(params);
    if (capability === "process") return this.process(params);
    if (capability === "pty") return this.pty(params);
    throw new Error(`unsupported capability: ${capability}`);
  }

  async file(params) {
    if (params.action === "roots") return { roots: this.roots };
    const target = await this.authorize(params.path, {
      allowRoot: !["move", "remove"].includes(params.action),
    });
    if (params.action === "stat") return statResult(target, await fs.lstat(target));
    if (params.action === "list") {
      const entries = await fs.readdir(target, { withFileTypes: true });
      if (entries.length > 10_000) throw new Error("directory contains more than 10,000 entries");
      const data = await Promise.all(
        entries.map(async (entry) => {
          const entryPath = path.join(target, entry.name);
          return statResult(entryPath, await fs.lstat(entryPath));
        }),
      );
      return { path: target, entries: data };
    }
    if (params.action === "read") {
      const stat = await fs.stat(target);
      const offset = integer(params.offset, 0, 0, Number.MAX_SAFE_INTEGER);
      const length = integer(
        params.length,
        Math.min(maxFileReadBytes, Math.max(0, stat.size - offset)),
        0,
        maxFileReadBytes,
      );
      const handle = await fs.open(target, "r");
      try {
        const buffer = Buffer.alloc(length);
        const { bytesRead } = await handle.read(buffer, 0, length, offset);
        const encoding = params.encoding === "base64" ? "base64" : "utf8";
        return {
          path: target,
          offset,
          bytesRead,
          size: stat.size,
          encoding,
          content: buffer.subarray(0, bytesRead).toString(encoding),
          eof: offset + bytesRead >= stat.size,
        };
      } finally {
        await handle.close();
      }
    }
    if (params.action === "write") {
      if (typeof params.content !== "string") throw new Error("content is required");
      const encoding = params.encoding === "base64" ? "base64" : "utf8";
      const content = Buffer.from(params.content, encoding);
      if (content.length > maxFileReadBytes) throw new Error("write exceeds 4 MiB");
      await fs.writeFile(target, content, { flag: params.overwrite === false ? "wx" : "w" });
      return { path: target, bytesWritten: content.length };
    }
    if (params.action === "mkdir") {
      await fs.mkdir(target, { recursive: params.recursive === true });
      return { path: target, created: true };
    }
    if (params.action === "move") {
      const destination = await this.authorize(params.destination, { allowRoot: false });
      if (params.overwrite !== true) {
        try {
          await fs.lstat(destination);
          throw new Error(`destination already exists: ${destination}`);
        } catch (error) {
          if (error.code !== "ENOENT") throw error;
        }
      }
      await fs.rename(target, destination);
      return { path: target, destination };
    }
    if (params.action === "remove") {
      await fs.rm(target, { recursive: params.recursive === true, force: false });
      return { path: target, removed: true };
    }
    throw new Error(`unsupported file action: ${params.action}`);
  }

  managedProcessView(id, entry, cursor = 0) {
    return {
      processId: id,
      pid: entry.child.pid,
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

  trackChild(collection, child, details) {
    const id = crypto.randomUUID();
    const entry = {
      child,
      ...details,
      startedAt: new Date().toISOString(),
      exitCode: null,
      signal: null,
      output: new OutputBuffer(),
    };
    collection.set(id, entry);
    child.stdout?.setEncoding("utf8");
    child.stderr?.setEncoding("utf8");
    child.stdout?.on("data", (chunk) => entry.output.push("stdout", chunk));
    child.stderr?.on("data", (chunk) => entry.output.push("stderr", chunk));
    child.on("error", (error) => entry.output.push("error", error.message));
    child.on("exit", (code, signal) => {
      entry.exitCode = code;
      entry.signal = signal;
    });
    return { id, entry };
  }

  async process(params) {
    if (params.action === "list" && params.system === true) {
      if (process.platform === "win32") {
        const { stdout } = await execFileAsync("tasklist", ["/FO", "CSV", "/NH"], {
          timeout: 10_000,
          maxBuffer: maxBufferedOutput,
        });
        return { format: "tasklist-csv", output: stdout };
      }
      const { stdout } = await execFileAsync(
        "ps",
        ["-eo", "pid,ppid,stat,%cpu,%mem,etime,comm,args", "--no-headers"],
        { timeout: 10_000, maxBuffer: maxBufferedOutput },
      );
      return { format: "ps", output: stdout };
    }
    if (params.action === "list") {
      return {
        processes: [...this.processes].map(([id, entry]) => this.managedProcessView(id, entry)),
      };
    }
    if (params.action === "start") {
      this.ensureSessionCapacity(this.processes, "process");
      if (typeof params.command !== "string" || params.command.length === 0) {
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
        Object.values(extraEnv).some((value) => typeof value !== "string")
      ) {
        throw new Error("env must contain at most 100 string values");
      }
      const child = spawn(params.command, args, {
        cwd,
        env: { ...process.env, ...extraEnv },
        shell: false,
        stdio: ["ignore", "pipe", "pipe"],
      });
      const { id, entry } = this.trackChild(this.processes, child, {
        command: params.command,
        args,
        cwd,
      });
      return this.managedProcessView(id, entry);
    }
    const entry = this.processes.get(params.processId);
    if (!entry) throw new Error("managed process not found");
    if (params.action === "poll") {
      return this.managedProcessView(params.processId, entry, params.cursor);
    }
    if (params.action === "signal") {
      const signal = params.signal ?? "SIGTERM";
      if (!["SIGINT", "SIGTERM", "SIGKILL"].includes(signal)) throw new Error("invalid signal");
      entry.child.kill(signal);
      return { processId: params.processId, signal, accepted: true };
    }
    throw new Error(`unsupported process action: ${params.action}`);
  }

  ptyView(id, entry, cursor = 0) {
    return {
      sessionId: id,
      pid: entry.child.pid,
      command: entry.command,
      args: entry.args,
      cwd: entry.cwd,
      backend: entry.backend,
      rows: entry.rows,
      cols: entry.cols,
      resizeSupported: false,
      startedAt: entry.startedAt,
      exitCode: entry.exitCode,
      signal: entry.signal,
      running: entry.exitCode === null && entry.signal === null,
      output: entry.output.read(cursor),
    };
  }

  async pty(params) {
    if (params.action === "list") {
      return { sessions: [...this.ptys].map(([id, entry]) => this.ptyView(id, entry)) };
    }
    if (params.action === "open") {
      this.ensureSessionCapacity(this.ptys, "PTY");
      const command = params.command ?? process.env.SHELL ?? (process.platform === "win32" ? "cmd.exe" : "/bin/sh");
      const args = stringArray(params.args ?? []);
      const cwd = await this.authorize(params.cwd ?? this.roots[0]);
      const rows = integer(params.rows, 24, 1, 500);
      const cols = integer(params.cols, 80, 1, 1_000);
      let child;
      let backend;
      if (process.platform !== "win32") {
        const commandLine = [command, ...args].map(shellQuote).join(" ");
        child = spawn("script", ["-qefc", commandLine, "/dev/null"], {
          cwd,
          env: {
            ...process.env,
            TERM: process.env.TERM ?? "xterm-256color",
            LINES: String(rows),
            COLUMNS: String(cols),
          },
          stdio: ["pipe", "pipe", "pipe"],
        });
        backend = "util-linux-script";
      } else {
        child = spawn(command, args, { cwd, shell: false, stdio: ["pipe", "pipe", "pipe"] });
        backend = "pipes-fallback";
      }
      const { id, entry } = this.trackChild(this.ptys, child, {
        command,
        args,
        cwd,
        backend,
        rows,
        cols,
      });
      return this.ptyView(id, entry);
    }
    const entry = this.ptys.get(params.sessionId);
    if (!entry) throw new Error("PTY session not found");
    if (params.action === "poll") return this.ptyView(params.sessionId, entry, params.cursor);
    if (params.action === "write") {
      if (typeof params.input !== "string") throw new Error("input is required");
      if (Buffer.byteLength(params.input) > maxPtyInputBytes) {
        throw new Error("PTY input exceeds 1 MiB");
      }
      if (!entry.child.stdin.writable) throw new Error("PTY input is closed");
      entry.child.stdin.write(params.input);
      return { sessionId: params.sessionId, bytesWritten: Buffer.byteLength(params.input) };
    }
    if (params.action === "close") {
      entry.child.kill("SIGTERM");
      return { sessionId: params.sessionId, closed: true };
    }
    throw new Error(`unsupported PTY action: ${params.action}`);
  }

  async machineStatus() {
    const disk = [];
    for (const root of this.roots) {
      try {
        const value = await fs.statfs(root);
        disk.push({
          path: root,
          totalBytes: value.blocks * value.bsize,
          availableBytes: value.bavail * value.bsize,
        });
      } catch (error) {
        disk.push({ path: root, error: error.message });
      }
    }
    const networks = Object.fromEntries(
      Object.entries(os.networkInterfaces()).map(([name, addresses]) => [
        name,
        (addresses ?? []).map(({ address, family, internal, mac, cidr }) => ({
          address,
          family,
          internal,
          mac,
          cidr,
        })),
      ]),
    );
    return {
      sampledAt: new Date().toISOString(),
      hostname: os.hostname(),
      platform: process.platform,
      release: os.release(),
      architecture: os.arch(),
      uptimeSeconds: os.uptime(),
      loadAverage: os.loadavg(),
      cpuCount: os.cpus().length,
      memory: { totalBytes: os.totalmem(), freeBytes: os.freemem() },
      disk,
      networks,
      allowedRoots: this.roots,
      managedProcesses: this.processes.size,
      ptySessions: this.ptys.size,
      sessionLimit: maxSessionCount,
      ptyBackend: process.platform === "win32" ? "pipes-fallback" : "util-linux-script",
    };
  }

  async close() {
    for (const entry of [...this.processes.values(), ...this.ptys.values()]) {
      if (entry.child.exitCode === null) entry.child.kill("SIGTERM");
    }
  }
}

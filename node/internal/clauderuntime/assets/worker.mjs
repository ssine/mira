import { randomUUID } from "node:crypto";
import { query } from "@anthropic-ai/claude-agent-sdk";
import { createInterface } from "node:readline";
import { readFile, stat } from "node:fs/promises";
import { createMiraTools } from "./mira-tools.mjs";
import { serverClient } from "./store.mjs";
import { TurnLifecycle } from "./turn-lifecycle.mjs";

const lines = createInterface({ input: process.stdin });
let run,
  started = false,
  stopping = false,
  wake;
const input = [];
const questions = new Map();
let client,
  lifecycle,
  interruptRecord,
  interruptRecordFailed = false,
  interrupted = false;
// Retried steer commands share one outcome; Mira stores the request by its ID.
const steers = new Map();
let markReady;
const ready = new Promise((resolve) => {
  markReady = resolve;
});
async function* prompts() {
  while (!stopping) {
    if (!input.length)
      await new Promise((resolve) => {
        wake = resolve;
      });
    if (input.length) yield input.shift();
  }
}
async function message(spec, text, attachments = []) {
  const content = [{ type: "text", text }];
  for (const file of attachments ?? []) {
    if (
      ["image/png", "image/jpeg", "image/gif", "image/webp"].includes(file.mime)
    ) {
      const info = await stat(file.path);
      if (!info.isFile() || info.size > 32 * 1024 * 1024)
        throw new Error(
          "Inline images must be regular files of at most 32 MiB; attach larger files as ordinary files",
        );
      content.push({
        type: "image",
        source: {
          type: "base64",
          media_type: file.mime,
          data: (await readFile(file.path)).toString("base64"),
        },
      });
    } else
      content.push({
        type: "text",
        text: `Attached file on this execution Node: ${file.path}`,
      });
  }
  return {
    type: "user",
    session_id: spec.sessionId,
    parent_tool_use_id: null,
    message: { role: "user", content },
  };
}
let spec;
lines.on("line", async (line) => {
  try {
    const command = JSON.parse(line);
    if (command.action === "describe") {
      const q = query({
        prompt: prompts(),
        options: {
          settingSources: ["user", "project", "local"],
          ...(process.env.MIRA_NODE_CLAUDE_CONFIG_DIR
            ? {
                env: {
                  ...process.env,
                  CLAUDE_CONFIG_DIR: process.env.MIRA_NODE_CLAUDE_CONFIG_DIR,
                },
              }
            : {}),
          ...(process.env.MIRA_NODE_CLAUDE_BINARY
            ? {
                pathToClaudeCodeExecutable: process.env.MIRA_NODE_CLAUDE_BINARY,
              }
            : {}),
        },
      });
      try {
        process.stdout.write(
          JSON.stringify({
            models: await q.supportedModels(),
            account: await q.accountInfo(),
          }),
        );
      } finally {
        stopping = true;
        wake?.();
        q.close();
        lines.close();
        process.stdin.destroy();
      }
    } else if (command.action === "answer") {
      const resolve = questions.get(command.questionId);
      if (resolve) {
        questions.delete(command.questionId);
        resolve(command.answers ?? {});
      }
    } else if (command.action === "steer") {
      // A failed steer never stops the running turn; Mira times out its request.
      await steer(command).catch(() => {});
    } else if (command.action === "start" && !started) {
      started = true;
      spec = command;
      await main(command);
    } else if (command.action === "interrupt") {
      interrupted = true;
      stopping = true;
      wake?.();
      // Stop model/tool work immediately, even while Server writes are retrying.
      // Still drain the event before recording completion; never abort a write.
      interruptRecord ??= client?.event({ type: "mira_interrupt_requested" })
        .catch(() => { interruptRecordFailed = true; });
      await run?.interrupt({ cancelQueued: true });
    }
  } catch {
    process.exitCode = 1;
    stopping = true;
    wake?.();
    run?.close();
    lines.close();
  }
});
lines.on("close", () => {
  if (!stopping) {
    stopping = true;
    wake?.();
    run?.close();
  }
});
// A steer joins the running turn at the SDK's next safe point. Mira records it
// before queueing; a turn that finished meanwhile rejects it so the Web can
// start the next turn with the same message instead.
async function steer({ steerId, text, attachments }) {
  let outcome = steers.get(steerId);
  if (!outcome) {
    outcome = (async () => {
      // Queue behind the first prompt, which the SDK receives once the query exists.
      if (started) await ready;
      if (!run || stopping || interrupted || lifecycle.finished)
        return { accepted: false, reason: "turn_finishing" };
      let next;
      try {
        next = await message(spec, text, attachments);
      } catch (error) {
        return { accepted: false, reason: "invalid_input", message: String(error?.message || "").slice(0, 500) };
      }
      next.uuid = steerId;
      await client.event(
        { type: "mira_steer", steerId, message: next.message, text, attachments: attachments ?? [] },
        steerId,
      );
      if (stopping || interrupted || !lifecycle.steer(steerId)) {
        await client.event({ type: "mira_steer_rejected", steerId }).catch(() => {});
        return { accepted: false, reason: "turn_finishing" };
      }
      input.push(next);
      wake?.();
      return { accepted: true };
    })().catch(async () => {
      // The request may have been stored before the failure; never leave it looking queued.
      await client?.event({ type: "mira_steer_rejected", steerId }).catch(() => {});
      return { accepted: false, reason: "unavailable" };
    });
    steers.set(steerId, outcome);
  }
  process.stdout.write(JSON.stringify({ steerId, ...(await outcome) }) + "\n");
}
async function main(spec) {
  client = serverClient(spec);
  let degraded = false,
    failed = false;
  lifecycle = new TurnLifecycle();
  try {
    input.push(await message(spec, spec.text, spec.attachments));
    await client.event({
      type: "mira_user",
      message: input[0].message,
      text: spec.text,
      attachments: spec.attachments ?? [],
    });
    if (interrupted) return;
    const options = {
      cwd: spec.cwd,
      model: spec.model || undefined,
      effort: spec.effort || undefined,
      // Without an explicit display the API omits thinking text and returns
      // only signatures, which would render as empty reasoning cards.
      thinking: { type: "adaptive", display: "summarized" },
      ...(spec.resume
        ? { resume: spec.sessionId }
        : { sessionId: spec.sessionId }),
      sessionStore: client.store,
      sessionStoreFlush: "eager",
      includePartialMessages: true,
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      settingSources: ["user", "project", "local"],
      canUseTool: async (name, input, { signal }) => {
        if (name !== "AskUserQuestion")
          return { behavior: "allow", updatedInput: input };
        const questionId = randomUUID();
        const answers = await new Promise((resolve, reject) => {
          questions.set(questionId, resolve);
          signal.addEventListener(
            "abort",
            () => {
              questions.delete(questionId);
              reject(new Error("Question cancelled"));
            },
            { once: true },
          );
          client
            .event({
              type: "mira_question",
              questionId,
              questions: input.questions,
            })
            .catch(reject);
        });
        await client.event({ type: "mira_answer", questionId, answers });
        return { behavior: "allow", updatedInput: { ...input, answers } };
      },
      mcpServers: { home_nodes: createMiraTools(spec) },
      systemPrompt: {
        type: "preset",
        preset: "claude_code",
        append: spec.instructions || "",
      },
      env: {
        ...process.env,
        // A result may precede an already queued background notification. Wait
        // for the native idle signal before closing streaming input.
        CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS: "1",
        ...(process.env.MIRA_NODE_CLAUDE_CONFIG_DIR
          ? { CLAUDE_CONFIG_DIR: process.env.MIRA_NODE_CLAUDE_CONFIG_DIR }
          : {}),
      },
      ...(process.env.MIRA_NODE_CLAUDE_BINARY
        ? { pathToClaudeCodeExecutable: process.env.MIRA_NODE_CLAUDE_BINARY }
        : {}),
      stderr: () => {},
    };
    run = query({ prompt: prompts(), options });
    markReady();
    for await (const event of run) {
      if (event.type === "system" && event.subtype === "mirror_error")
        degraded = true;
      await client.event(event);
      if (event.type === "result" && !event.parent_tool_use_id) {
        // A background notification can recover from an earlier API error in
        // this same worker. Completion reflects the latest native response.
        failed = event.is_error === true && !interrupted;
      }
      if (lifecycle.observe(event)) {
        stopping = true;
        wake?.();
      }
    }
    if (!lifecycle.finished && !interrupted)
      throw new Error("Claude exited before its background work and final response completed");
  } catch (error) {
    failed = !interrupted;
    // SDK errors may contain provider output, but never include the Mira credential.
    const text = String(error?.message || "Claude runtime failed").replaceAll(
      spec.credential,
      "[redacted]",
    );
    try {
      if (!interrupted)
        await client.event({
          type: "mira_error",
          message: text.slice(0, 4000),
        });
    } catch {
      degraded = true;
    }
  } finally {
    stopping = true;
    markReady();
    wake?.();
    run?.close();
    await interruptRecord;
    try {
      await client.event({
        type: "mira_completed",
        failed,
        degraded: degraded || interruptRecordFailed,
        interrupted,
      });
    } catch {
      process.exitCode = 1;
    }
    lines.close();
    process.stdin.destroy();
  }
}

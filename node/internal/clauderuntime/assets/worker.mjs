import { randomUUID } from "node:crypto";
import { query } from "@anthropic-ai/claude-agent-sdk";
import { createInterface } from "node:readline";
import { readFile, stat } from "node:fs/promises";
import { createMiraTools } from "./mira-tools.mjs";
import { serverClient } from "./store.mjs";

const lines = createInterface({ input: process.stdin });
let run,
  started = false,
  stopping = false,
  wake;
const input = [];
const questions = new Map();
let client,
  interrupted = false;
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
    } else if (command.action === "start" && !started) {
      started = true;
      spec = command;
      await main(command);
    } else if (command.action === "interrupt") {
      interrupted = true;
      try {
        await client?.event({ type: "mira_interrupt_requested" });
      } finally {
        stopping = true;
        wake?.();
        await run?.interrupt();
      }
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
async function main(spec) {
  client = serverClient(spec);
  let degraded = false,
    failed = false;
  try {
    input.push(await message(spec, spec.text, spec.attachments));
    await client.event({
      type: "mira_user",
      message: input[0].message,
      text: spec.text,
      attachments: spec.attachments ?? [],
    });
    const options = {
      cwd: spec.cwd,
      model: spec.model || undefined,
      effort: spec.effort || undefined,
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
    for await (const event of run) {
      if (event.type === "system" && event.subtype === "mirror_error")
        degraded = true;
      await client.event(event);
      if (event.type === "result") {
        failed ||= event.is_error === true && !interrupted;
        stopping = true;
        wake?.();
      }
    }
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
    wake?.();
    run?.close();
    try {
      await client.event({
        type: "mira_completed",
        failed,
        degraded,
        interrupted,
      });
    } catch {
      process.exitCode = 1;
    }
    lines.close();
    process.stdin.destroy();
  }
}

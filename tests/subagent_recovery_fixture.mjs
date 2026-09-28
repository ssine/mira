import assert from "node:assert/strict";

// Real native multi-agent turns; only the model's Responses transport is fake.
export function subagentRecoveryFixture() {
  const held = new Map(), calls = [];
  let holding = true;
  const send = (response, items) => {
    const id = `tree-${calls.length}`;
    response.writeHead(200, { "content-type": "text/event-stream" });
    response.end([
      { type: "response.created", response: { id } },
      ...items.map(item => ({ type: "response.output_item.done", item })),
      { type: "response.completed", response: { id, usage: { input_tokens: 10, output_tokens: 10, total_tokens: 20 } } },
    ].map(event => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join(""));
  };
  const message = text => ({ type: "message", id: `msg-tree-${calls.length}`, role: "assistant", content: [{ type: "output_text", text }] });
  const tool = (name, call_id, args) => ({ type: "function_call", namespace: "mira_collaboration", name, call_id, arguments: JSON.stringify(args) });
  return {
    async respond(body, response) {
      const user = JSON.stringify(body.input.filter(item => item.role === "user").at(-1)?.content ?? "");
      const agent = body.input.filter(item => item.type === "agent_message" && item.recipient !== "/root");
      const child = agent.some(item => JSON.stringify(item.content).includes("RECOVERY_TREE_CHILD"));
      const sibling = agent.some(item => JSON.stringify(item.content).includes("RECOVERY_TREE_SIBLING"));
      const parent = user.includes("RECOVERY_TREE_PARENT");
      if (!child && !sibling && !parent) return false;
      const kind = child ? "child" : sibling ? "sibling" : "parent";
      calls.push({ kind, body });
      if (child) {
        const cold = agent.some(item => JSON.stringify(item.content).includes("RECOVERY_TREE_CHILD_COLD"));
        const notify = cold ? "tree-child-cold-notify" : "tree-child-notify";
        const rejected = cold ? "recovery-tree-cold-rejected" : "recovery-tree-rejected";
        if (body.input.some(item => item.encrypted_content === rejected)) {
          response.writeHead(400, { "content-type": "application/json" });
          response.end(JSON.stringify({ error: { code: "invalid_encrypted_content", message: "The encrypted content for item rs_tree_bad could not be verified. Reason: Encrypted content could not be decrypted or parsed." } }));
        } else if (!body.input.some(item => item.call_id === notify)) {
          send(response, [
            { type: "reasoning", id: cold ? "rs_tree_cold_bad" : "rs_tree_bad", summary: [], encrypted_content: rejected },
            tool("send_message", notify, { target: "/root", message: "RECOVERY_TREE_CHILD_TOOL_COMPLETED" }),
          ]);
        } else send(response, [message(cold ? "RECOVERY_TREE_CHILD_COLD_RECOVERED" : "RECOVERY_TREE_CHILD_RECOVERED")]);
      } else if (parent && user.includes("_COLD") && !body.input.some(item => item.call_id === "tree-follow-cold")) {
        send(response, [tool("followup_task", "tree-follow-cold", { target: "/root/recovering", message: "RECOVERY_TREE_CHILD_COLD" })]);
      } else if (parent && !body.input.some(item => item.call_id === "tree-spawn-child")) {
        send(response, [
          tool("spawn_agent", "tree-spawn-child", { task_name: "recovering", message: "RECOVERY_TREE_CHILD", fork_turns: "none" }),
          tool("spawn_agent", "tree-spawn-sibling", { task_name: "working", message: "RECOVERY_TREE_SIBLING", fork_turns: "none" }),
        ]);
      } else if (holding) held.set(kind, () => send(response, [message(`RECOVERY_TREE_${kind.toUpperCase()}_DONE`)]));
      else send(response, [message(`RECOVERY_TREE_${kind.toUpperCase()}_DONE`)]);
      return true;
    },
    async run({ connect, binding, temporary, agentConfig, waitFor, admin, store, history, automaticEndpoint }) {
      const client = await connect(binding);
      const parent = (await client.call("thread/start", { cwd: temporary, model: "gpt-5.1-codex", config: agentConfig(), approvalPolicy: "never", sandbox: "danger-full-access" })).thread.id;
      await client.call("turn/start", { threadId: parent, input: [{ type: "text", text: "RECOVERY_TREE_PARENT" }] });
      try {
        await waitFor(() => held.has("parent") && held.has("sibling"), "parent and sibling actively sampling");
        const child = await waitFor(async () => {
          const state = (await admin(`/v2/stores/${store}`)).state;
          for (const [id, value] of Object.entries(state.created_threads)) {
            if (value.parent_thread_id === parent && JSON.stringify((await history(id)).items).includes("tree-child-notify")) return id;
          }
        }, "failed child persisted");
        await waitFor(async () => {
          const state = await admin(automaticEndpoint(child));
          assert.notEqual(state.status, "stopped", JSON.stringify(state));
          return state.status === "completed";
        }, "child recovers while parent and sibling remain active");
        assert.equal((await client.call("thread/read", { threadId: parent, includeTurns: false })).thread.status.type, "active");
        assert(!client.events.some(event => event.method === "thread/closed" && event.params.threadId === parent));
        const childCalls = calls.filter(call => call.kind === "child");
        assert.equal(childCalls.length, 3, "one initial request, one failure, one retry");
        const before = childCalls[1].body.input.filter(item => item.type !== "reasoning");
        const after = childCalls[2].body.input.filter(item => item.type !== "reasoning");
        assert.deepEqual(after, before, "retry preserves all input except incompatible reasoning");
        const items = (await history(child)).items;
        const rootTurns = items.filter(item => item.type === "turn_context").map(item => item.payload.root_turn_id);
        assert(rootTurns[0], "child records the parent task's root turn");
        assert(rootTurns.every(id => id === rootTurns[0]), "retry preserves the original root-turn lineage");
        assert.equal(items.filter(item => item.type === "response_item" && item.payload.type === "function_call" && item.payload.call_id === "tree-child-notify").length, 1);
        assert(items.some(item => item.type === "response_item" && item.payload.encrypted_content === "recovery-tree-rejected"), "canonical encrypted history is retained");
        console.log("Subagent recovery passed with active parent and sibling, preserved tool result and no added message");
        holding = false;
        for (const release of held.values()) release();
        held.clear();
        const state = (await admin(`/v2/stores/${store}`)).state;
        const family = [parent, ...Object.entries(state.created_threads).filter(([, value]) => value.parent_thread_id === parent).map(([id]) => id)];
        const idle = async () => {
          for (const id of family) {
            if (!["idle", "notLoaded", "systemError"].includes((await client.call("thread/read", { threadId: id, includeTurns: false })).thread.status.type)) return false;
          }
          return true;
        };
        await waitFor(idle, "completed first tree");
        await admin(automaticEndpoint(child), { generation: 1, enabled: false }, "PUT");
        await client.call("turn/start", { threadId: parent, input: [{ type: "text", text: "RECOVERY_TREE_PARENT_COLD" }] });
        await waitFor(async () => JSON.stringify((await history(child)).items).includes("tree-child-cold-notify") && await idle(), "second child failure with recovery disabled");
        const coldPlan = await admin(`/v1/codex/threads/${child}/input-recovery?storeId=${store}&nodeAccountId=${binding}`);
        const failedTurn = (await client.call("thread/read", { threadId: child, includeTurns: true })).thread.turns.at(-1).id;
        const coldRootTurn = (await history(child)).items.filter(item => item.type === "turn_context").at(-1).payload.root_turn_id;
        // Evict the idle native family without changing its account, durable
        // identity, generation, or the parent's intended child task.
        const native = await connect(binding, store, true);
        await native.call("mira/thread/unload", { threadId: parent, threadIds: family });
        assert.equal((await native.call("thread/read", { threadId: parent, includeTurns: false })).thread.status.type, "notLoaded");
        await admin(automaticEndpoint(child), { generation: 1, enabled: true }, "PUT");
        await waitFor(async () => JSON.stringify((await history(child)).items).includes("RECOVERY_TREE_CHILD_COLD_RECOVERED"), "cold child resumes through restored parent");
        await waitFor(idle, "cold recovery completes before testing a stale retry");
        assert.equal((await history(child)).items.filter(item => item.type === "turn_context").at(-1).payload.root_turn_id, coldRootTurn);
        assert.equal((await history(parent)).items.filter(item => item.type === "response_item" && item.payload.type === "function_call" && item.payload.call_id === "tree-follow-cold").length, 1);
        const beforeRejectedRetry = calls.length;
        await assert.rejects(native.call("mira/thread/recover", { threadId: child, expectedTurnId: "superseded", failureId: "unconfirmed" }), /recovery rejected/);
        const oldRequest = { threadId: child, expectedTurnId: failedTurn, failureId: coldPlan.failureId };
        await assert.rejects(native.call("mira/thread/recover", oldRequest), /failed turn was superseded/);
        await assert.rejects(client.call("mira/thread/recover", oldRequest), /controlled by Mira Server/);
        assert.equal(calls.length, beforeRejectedRetry, "unconfirmed recovery must not sample");
        console.log("Cold child recovery restored its owner without replaying the parent's followup; stale requests were rejected");
      } finally {
        holding = false;
        for (const release of held.values()) release();
        held.clear();
      }
    },
  };
}

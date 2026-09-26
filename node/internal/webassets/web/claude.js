import { ComposerDrafts } from "/composer-drafts.js";
import DOMPurify from "/vendor/dompurify.js";
import { marked } from "/vendor/marked.js";

// A native Claude view: shared visual components, without pretending SDK events
// or transcripts are Codex rollout records. All mutations use the console's CSRF client.
export function createClaudeConsole({ api, show, navigateCodex, toast }) {
  const root = document.querySelector("#claudeView");
  const $ = (s) => root.querySelector(s);
  let earliest = 0,
    historyJobs = 0;
  let session,
    nodes = [],
    list = [],
    cursor = 0,
    epoch = 0,
    loading = false,
    timer,
    busy = false,
    nextOffset,
    archived = false;
  let createRequest,
    uploadController,
    streamID = "",
    turnInterrupted = false,
    subpath = "",
    runtimeNode = "",
    models = [];
  const drafts = new ComposerDrafts();
  // A lost turn response must keep its exact request on the owning conversation.
  const turnRequests = new Map();
  let draftKey = "claude:new",
    draftLoading = false,
    draftReady = false;
  function saveDraft() {
    if (draftLoading || !draftReady) return;
    void drafts
      .write(draftKey, {
        text: $("[data-input]").value,
        files: [...$("[data-files]").files],
      })
      .catch(() => notify("草稿未能保存到浏览器，请保留输入。"));
  }
  async function restoreDraft(key) {
    draftKey = key;
    draftLoading = true;
    try {
      const value = await drafts.read(key);
      if (draftKey !== key) return;
      $("[data-input]").value = value?.text || "";
      const dt = new DataTransfer();
      for (const file of value?.files || []) dt.items.add(file);
      $("[data-files]").files = dt.files;
      $("[data-file-names]").textContent = (value?.files || [])
        .map((f) => f.name)
        .join(" · ");
    } catch {
      notify("无法读取本地草稿");
    } finally {
      draftLoading = false;
      draftReady = true;
    }
  }
  const cards = new Map(),
    textByKey = new Map();
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    e.className = cls || "";
    if (text !== undefined) e.textContent = text;
    return e;
  };
  const call = (path, body) =>
    api(
      "/v1/claude/" + path,
      body === undefined ? {} : { method: "POST", body: JSON.stringify(body) },
    );
  const notify = (text) => {
    $("[data-notice]").textContent = text;
  };
  function route(id) {
    const url = new URL(location.href);
    url.searchParams.delete("thread");
    url.searchParams.set("view", "claude");
    if (id) url.searchParams.set("claude", id);
    else url.searchParams.delete("claude");
    history.pushState(null, "", url);
  }
  function sidebar(open) {
    $(".claude-shell").classList.toggle("sidebar-open", open);
    $("[data-sidebar-toggle]").setAttribute("aria-expanded", String(open));
  }
  function state(s) {
    session = s;
    for (const button of $("[data-list]").querySelectorAll(
      "[data-session-id]",
    )) {
      const selected = button.dataset.sessionId === s?.sessionId;
      button.classList.toggle("active", selected);
      if (selected)
        button.textContent = `${s.activeTurn ? "◌ " : ""}${s.title || "Claude 对话"}`;
    }
    $("[data-title]").textContent = s?.title || "新 Claude 对话";
    const persistence =
      s?.persistence === "incomplete"
        ? "历史未完整保存"
        : s?.activeTurn
          ? "运行中 · 正在保存"
          : s?.persistence === "saved"
            ? "已保存到 Mira"
            : "使用所选 Node 上的 Claude 登录或 API 配置";
    $("[data-state]").textContent = persistence;
    $("[data-incomplete]").hidden = s?.persistence !== "incomplete";
    $("[data-stop]").hidden = !s?.activeTurn;
    $("[data-send]").disabled = busy || !!s?.activeTurn || !!subpath;
    for (const field of root.querySelectorAll(
      "[data-node],[data-cwd],[data-model],[data-files],[data-input],[data-connect],[data-new]",
    ))
      field.disabled = busy;

    $("[data-effort]").disabled =
      busy ||
      models.find(
        (m) =>
          m.value === $("[data-model]").value ||
          m.resolvedModel === $("[data-model]").value,
      )?.supportsEffort === false;
    $("[data-reconcile]").hidden = !s?.activeTurn;
    document.title = `${s?.title || "Claude Code"} · Mira`;
  }
  function card(key, kind, title, text, markdown = false) {
    let c = cards.get(key);
    if (!c) {
      c = el("article", `trace-card ${kind}`);
      let body;
      if (["tool", "reasoning", "system"].includes(kind)) {
        const details = el("details", "trace-detail"),
          head = el("summary", "trace-head");
        head.append(el("span", "trace-kind", title));
        body = el("div", "trace-body");
        details.append(head, body);
        c.append(details);
      } else {
        const head = el("div", "trace-head");
        head.append(el("span", "trace-kind", title));
        body = el("div", "trace-body");
        c.append(head, body);
      }
      cards.set(key, c);
      $("[data-trace]").append(c);
    }
    const body = c.querySelector(".trace-body");
    if (markdown) {
      body.classList.add("markdown-body");
      body.innerHTML = DOMPurify.sanitize(marked.parse(text || ""));
    } else body.textContent = text || "";
    return c;
  }
  function blocks(message, key, role) {
    const content =
      typeof message?.content === "string"
        ? [{ type: "text", text: message.content }]
        : message?.content || [];
    for (const [i, b] of content.entries()) {
      const k = `${key}:${i}`;
      if (b.type === "text") {
        textByKey.set(k, b.text);
        card(
          k,
          role,
          role === "user" ? "你" : "Claude",
          b.text,
          role !== "user",
        );
      } else if (b.type === "thinking")
        card(k, "reasoning", "思考", b.thinking);
      else if (b.type === "tool_use")
        card(`tool:${b.id}`, "tool", b.name, JSON.stringify(b.input, null, 2));
      else if (b.type === "tool_result") {
        const details =
          typeof b.content === "string"
            ? b.content
            : JSON.stringify(
                (b.content || []).filter((item) => item.type !== "image"),
                null,
                2,
              );
        card(
          `result:${b.tool_use_id}`,
          "tool",
          b.is_error ? "工具错误" : "工具结果",
          details,
        );
        if (Array.isArray(b.content))
          b.content.forEach((item, index) => {
            if (item.type === "image")
              blocks({ content: [item] }, `${k}:result:${index}`, "assistant");
          });
      } else if (
        b.type === "image" &&
        b.source?.type === "base64" &&
        /^image\/(png|jpeg|gif|webp)$/.test(b.source.media_type)
      ) {
        const c = card(k, role, "图片", "");
        const body = c.querySelector(".trace-body");
        if (!body.querySelector("img")) {
          const image = el("img");
          image.alt = "会话图片";
          image.loading = "lazy";
          image.src = `data:${b.source.media_type};base64,${b.source.data}`;
          image.style.maxWidth = "100%";
          body.append(image);
        }
      }
    }
  }
  function question(event) {
    const c = card(
      `question:${event.questionId}`,
      "assistant",
      "Claude 需要你的选择",
      "",
    );
    const body = c.querySelector(".trace-body");
    if (body.querySelector("form")) return;
    const form = el("form");
    for (const q of event.questions || []) {
      const label = el("label", "", q.question),
        input = el("input");
      input.name = q.question;
      input.placeholder = (q.options || []).map((o) => o.label).join(" / ");
      input.required = true;
      label.append(input);
      form.append(label);
      for (const o of q.options || []) {
        const button = el("button", "secondary", o.label);
        button.type = "button";
        button.title = o.description || "";
        button.onclick = () => {
          input.value = q.multiSelect
            ? [input.value, o.label].filter(Boolean).join(", ")
            : o.label;
        };
        form.append(button);
      }
    }
    const submit = el("button", "primary", "回答");
    form.append(submit);
    form.onsubmit = async (e) => {
      e.preventDefault();
      submit.disabled = true;
      try {
        await call(`sessions/${session.sessionId}/answer`, {
          questionId: event.questionId,
          answers: Object.fromEntries(new FormData(form)),
        });
      } catch (error) {
        toast(error.message);
        submit.disabled = false;
      }
    };
    body.append(form);
  }
  function event(e, seq) {
    if (
      e.parent_tool_use_id &&
      ["assistant", "user", "stream_event"].includes(e.type)
    )
      return;
    if (e.type === "mira_interrupt_requested") {
      turnInterrupted = true;
      notify("正在停止…");
      return;
    }
    if (e.type === "mira_completed" && e.interrupted) {
      notify("已停止");
      return;
    }
    if (e.type === "mira_user") {
      turnInterrupted = false;
      notify("");
      card(
        `user:${seq}`,
        "user",
        "你",
        [
          e.text,
          ...(e.attachments || []).map((f) => `附件：${f.name || f.path}`),
        ].join("\n"),
      );
      (e.message?.content || []).forEach((block, index) => {
        if (block.type === "image")
          blocks({ content: [block] }, `user-image:${seq}:${index}`, "user");
      });
    } else if (e.type === "assistant") {
      // A native API message may arrive as several completed SDK blocks. Its
      // event UUID identifies the immutable block, while live deltas are temporary.
      const prefix = `stream:${e.message?.id}:`;
      for (const [key, c] of cards) {
        if (key.startsWith(prefix)) {
          c.remove();
          cards.delete(key);
          textByKey.delete(key);
        }
      }
      blocks(e.message, e.uuid || seq, "assistant");
    } else if (e.type === "user") {
      const content = e.message?.content;
      if (
        Array.isArray(content) &&
        content.some((b) => b.type === "tool_result")
      )
        blocks(e.message, e.uuid || seq, "user");
    } else if (e.type === "stream_event") {
      const raw = e.event;
      if (raw?.type === "message_start") streamID = raw.message.id;
      if (
        raw?.type === "content_block_delta" &&
        raw.delta?.type === "text_delta"
      ) {
        const key = `stream:${streamID}:${raw.index}`,
          text = (textByKey.get(key) || "") + raw.delta.text;
        textByKey.set(key, text);
        card(key, "assistant", "Claude", text, true);
      }
    } else if (e.type === "mira_question") question(e);
    else if (e.type === "mira_answer") {
      const c = cards.get(`question:${e.questionId}`);
      if (c)
        c.querySelector(".trace-body").textContent = Object.values(
          e.answers,
        ).join(" · ");
    } else if (e.type === "mira_error")
      card(`error:${seq}`, "error", "运行错误", e.message);
    else if (e.type === "system" && e.subtype === "mirror_error")
      card(
        `error:${seq}`,
        "error",
        "历史未完整保存",
        "部分 Claude 原生记录未能镜像到 Mira。工具不会因此重跑。",
      );
    else if (e.type === "result") {
      const usage = e.usage || {};
      $("[data-usage]").textContent =
        `最近结果：输入 ${usage.input_tokens ?? "—"} · 输出 ${usage.output_tokens ?? "—"} · 本次运行报告费用 ${typeof e.total_cost_usd === "number" ? "$" + e.total_cost_usd.toFixed(4) : "未提供"}`;
      if (e.is_error && !turnInterrupted)
        card(
          `error:${seq}`,
          "error",
          "Claude 返回错误",
          (e.errors || [e.subtype]).join("\n"),
        );
    } else if (
      e.type === "system" &&
      ["task_started", "task_progress", "task_notification"].includes(e.subtype)
    )
      card(
        `task:${e.task_id || seq}`,
        "system",
        "子任务",
        e.description || e.summary || e.status || "",
      );
  }
  function renderRows(result) {
    for (const item of result.data) {
      if (subpath) {
        if (["assistant", "user"].includes(item.payload.type))
          blocks(
            item.payload.message,
            item.payload.message?.id || item.payload.uuid || item.seq,
            item.payload.type,
          );
      } else event(item.payload, item.seq);
    }
  }
  async function loadHistory(older = false) {
    if (!session) return;
    historyJobs++;
    try {
      const generation = epoch,
        selected = session.sessionId,
        scroll = $("[data-scroll]"),
        height = scroll.scrollHeight,
        top = scroll.scrollTop;
      const known = new Set($("[data-trace]").children),
        interruptedBefore = turnInterrupted,
        usageBefore = $("[data-usage]").textContent;
      const result = await call(
        `sessions/${selected}/${subpath ? "history" : "events"}?view=transcript&before=${older ? earliest : 0}&subpath=${encodeURIComponent(subpath)}`,
      );
      if (generation !== epoch) return;
      renderRows(result);
      earliest = result.earliest;
      if (!older) cursor = result.cursor;
      $("[data-history-more]").hidden = !result.data.length;
      if (older) {
        const added = [...$("[data-trace]").children].filter(
          (e) => !known.has(e),
        );
        $("[data-trace]").prepend(...added);
        scroll.scrollTop = top + scroll.scrollHeight - height;
        turnInterrupted = interruptedBefore;
        $("[data-usage]").textContent = usageBefore;
      } else {
        state(result.session);
        scroll.scrollTop = scroll.scrollHeight;
      }
    } finally {
      historyJobs--;
    }
  }
  async function poll() {
    if (loading || historyJobs || !session || root.classList.contains("hidden"))
      return;
    const selected = session.sessionId,
      generation = epoch;
    loading = true;
    const scroll = $("[data-scroll]"),
      follow =
        scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 100;
    try {
      const result = await call(
        `sessions/${selected}/${subpath ? "history" : "events"}?after=${cursor}&subpath=${encodeURIComponent(subpath)}`,
      );
      if (generation !== epoch) return;
      renderRows(result);
      cursor = result.cursor;
      state(result.session);
      if (follow) scroll.scrollTop = scroll.scrollHeight;
      if (result.data.length) {
        loading = false;
        queueMicrotask(() => void poll());
        return;
      }
      if (!session.activeTurn) await loadChildren(generation);
    } catch (error) {
      if (generation === epoch)
        notify(`读取中断：${error.message}；连接恢复后会继续加载。`);
    } finally {
      loading = false;
    }
  }
  async function loadChildren(generation = epoch) {
    const result = await call(`sessions/${session.sessionId}/children`);
    if (generation !== epoch) return;
    const select = $("[data-child]"),
      value = select.value;
    select.replaceChildren(new Option("主会话", ""));
    for (const child of result.data)
      select.add(
        new Option(
          `子 Agent · ${child.subpath
            .split("/")
            .at(-1)
            .replace(/^agent-/, "")
            .slice(0, 16)}`,
          child.subpath,
        ),
      );
    select.value = value;
    select.hidden = !result.data.length;
  }
  async function refreshList(append = false) {
    const result = await call(
      `sessions?archived=${archived ? 1 : 0}&offset=${append ? nextOffset || 0 : 0}`,
    );
    list = append ? [...list, ...result.data] : result.data;
    nextOffset = result.nextOffset;
    const target = $("[data-list]");
    target.replaceChildren();
    // Keep every loaded conversation; group by workspace without hiding other projects.
    const groups = new Map();
    for (const item of list) {
      if (!groups.has(item.cwd)) groups.set(item.cwd, []);
      groups.get(item.cwd).push(item);
    }
    for (const [path, items] of groups) {
      target.append(el("p", "muted claude-project", path));
      for (const item of items) {
        const button = el(
          "button",
          "claude-thread",
          `${item.activeTurn ? "◌ " : ""}${item.title || "Claude 对话"}`,
        );
        button.title = item.title || item.cwd;
        button.dataset.sessionId = item.sessionId;
        button.classList.toggle(
          "active",
          item.sessionId === session?.sessionId,
        );
        button.onclick = () => select(item).catch((e) => toast(e.message));
        target.append(button);
      }
    }
    $("[data-more]").hidden = nextOffset == null;
  }
  async function select(s, { updateRoute = true } = {}) {
    if (busy) {
      notify("正在提交消息，请稍候再切换会话。");
      return;
    }
    saveDraft();
    sidebar(false);
    $("[data-continue]").checked = false;
    epoch++;
    cursor = 0;
    subpath = "";
    cards.clear();
    textByKey.clear();
    $("[data-trace]").replaceChildren();
    $("[data-child]").value = "";
    state(s);
    $("[data-node]").value = s.nodeId;
    $("[data-cwd]").value = s.cwd;
    $("[data-model]").value = s.model;
    $("[data-effort]").value = s.effort || "";
    if (updateRoute) route(s.sessionId);
    notify("");
    await restoreDraft("claude:" + s.sessionId);
    await loadHistory();
    await poll();
    await refreshList();
  }
  function draft() {
    if (busy) {
      notify("正在提交消息，请稍候再新建会话。");
      return;
    }
    saveDraft();
    epoch++;
    session = null;
    sidebar(false);
    $("[data-continue]").checked = false;
    cursor = 0;
    subpath = "";
    createRequest = null;
    cards.clear();
    textByKey.clear();
    $("[data-trace]").replaceChildren();
    $("[data-child]").hidden = true;
    state(null);
    route();
    notify("");
    void restoreDraft("claude:new");
    $("[data-input]").focus();
  }
  function renderEffort() {
    const chosen = models.find(
      (m) =>
        m.value === $("[data-model]").value ||
        m.resolvedModel === $("[data-model]").value,
    );
    const select = $("[data-effort]"),
      previous = select.value;
    select.replaceChildren(new Option("思考强度：默认", ""));
    const labels = {
      low: "低",
      medium: "中",
      high: "高",
      xhigh: "很高",
      max: "最高",
    };
    for (const level of chosen?.supportedEffortLevels || [
      "low",
      "medium",
      "high",
      "max",
    ]) {
      select.add(new Option(labels[level] || level, level));
    }
    select.disabled = chosen?.supportsEffort === false;
    if (
      !select.disabled &&
      [...select.options].some((o) => o.value === previous)
    )
      select.value = previous;
  }
  async function prepare() {
    const id = $("[data-node]").value;
    if (!id) throw new Error("请选择可运行 Claude 的在线 Node");
    let result = await call(`runtimes/${id}/prepare`, {});
    const deadline = Date.now() + 11 * 60_000;
    while (result.status === "preparing") {
      notify("首次准备 Claude SDK，节点的其他功能可继续使用…");
      await new Promise((r) => setTimeout(r, 1000));
      if (Date.now() > deadline) throw new Error("Claude SDK 准备超时");
      result = await call(`runtimes/${id}/status`, {});
    }
    if ($("[data-node]").value !== id) throw new Error("已切换执行 Node");
    if (result.status !== "ready")
      throw new Error(result.error || "Claude 未就绪");
    notify("");
    if (runtimeNode !== id) {
      runtimeNode = id;
      try {
        const info = await call(`runtimes/${id}/describe`, {});
        models = info.models || [];
        const current = $("[data-model]").value;
        $("[data-models]").replaceChildren();
        for (const model of models) {
          const o = el("option");
          o.value = model.value || model.id;
          o.label = model.displayName || model.value;
          $("[data-models]").append(o);
        }
        $("[data-model]").value = current;
        renderEffort();
      } catch (error) {
        notify(error.message);
      }
    }
  }
  async function attachments() {
    const files = [...$("[data-files]").files];
    if (!files.length) return [];
    const result = [],
      id = $("[data-node]").value,
      base =
        $("[data-cwd]").value.replace(/[\\/]+$/, "") +
        `/.mira/attachments/${session.sessionId}/${crypto.randomUUID()}`;
    const invoke = (params) =>
      api(`/v1/nodes/${id}/invoke`, {
        method: "POST",
        body: JSON.stringify({ capability: "file", params }),
        signal: uploadController.signal,
      });
    await invoke({ action: "mkdir", path: base, recursive: true });
    for (const [index, file] of files.entries()) {
      const path =
        base + `/${index}-${file.name.replace(/[^\p{L}\p{N}._-]/gu, "_")}`;
      let offset = 0;
      do {
        const chunk = new Uint8Array(
          await file.slice(offset, offset + 1024 * 1024).arrayBuffer(),
        );
        let binary = "";
        for (let i = 0; i < chunk.length; i += 8192)
          binary += String.fromCharCode(...chunk.subarray(i, i + 8192));
        await invoke({
          action: "write",
          path,
          encoding: "base64",
          content: btoa(binary),
          append: offset > 0,
          offset,
          overwrite: offset === 0,
        });
        offset += chunk.length;
        notify(
          `上传 ${file.name} · ${Math.round((offset / Math.max(file.size, 1)) * 100)}%`,
        );
      } while (offset < file.size);
      result.push({ path, mime: file.type, name: file.name });
    }
    return result;
  }
  async function send(e) {
    e.preventDefault();
    if (busy || session?.activeTurn) return;
    const text = $("[data-input]").value.trim();
    if (!text) return;
    busy = true;
    state(session);
    uploadController = new AbortController();
    $("[data-cancel-upload]").hidden = false;
    try {
      await prepare();
      if (!session) {
        createRequest ??= {
          requestId: crypto.randomUUID(),
          nodeId: $("[data-node]").value,
          cwd: $("[data-cwd]").value.trim(),
          title: text.slice(0, 100),
          model: $("[data-model]").value,
        };
        const created = await call("sessions", createRequest);
        state(created);
        route(created.sessionId);
        await drafts.write(
          "claude:" + created.sessionId,
          {
            text: $("[data-input]").value,
            files: [...$("[data-files]").files],
          },
          { removeKey: draftKey },
        );
        draftKey = "claude:" + created.sessionId;
      }
      let turnRequest = turnRequests.get(session.sessionId);
      if (!turnRequest) {
        const uploaded = await attachments();
        turnRequest = {
          requestId: crypto.randomUUID(),
          text,
          nodeId: $("[data-node]").value,
          cwd: $("[data-cwd]").value.trim(),
          model: $("[data-model]").value,
          effort: $("[data-effort]").value,
          attachments: uploaded,
          continueAcknowledgedHistory: $("[data-continue]").checked,
        };
        turnRequests.set(session.sessionId, turnRequest);
      }
      const result = await call(
        `sessions/${session.sessionId}/turns`,
        turnRequest,
      );
      session.activeTurn = result.turnId;
      turnRequests.delete(session.sessionId);
      await drafts.write(draftKey, undefined);
      $("[data-input]").value = "";
      $("[data-files]").value = "";
      $("[data-file-names]").textContent = "";
      notify("");
      await refreshList();
      await poll();
    } catch (error) {
      if (error.status >= 400 && error.status < 500 && error.status !== 408)
        turnRequests.delete(session?.sessionId);
      notify(error.message);
      if (session) {
        const s = await call(`sessions/${session.sessionId}`).catch(() => null);
        if (s) state(s);
      }
    } finally {
      busy = false;
      $("[data-cancel-upload]").hidden = true;
      state(session);
    }
  }
  $("[data-sidebar-toggle]").onclick = () =>
    sidebar(!$(".claude-shell").classList.contains("sidebar-open"));
  $("[data-sidebar-backdrop]").onclick = () => sidebar(false);
  $("[data-form]").onsubmit = send;
  $("[data-connect]").onclick = () => prepare().catch((e) => notify(e.message));
  $("[data-model]").onchange = renderEffort;
  $("[data-input]").oninput = saveDraft;
  $("[data-input]").onkeydown = (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $("[data-form]").requestSubmit();
    }
  };
  $("[data-new]").onclick = draft;
  $("[data-codex]").onclick = () => navigateCodex();
  $("[data-history-more]").onclick = () =>
    loadHistory(true).catch((e) => toast(e.message));
  $("[data-more]").onclick = () =>
    refreshList(true).catch((e) => toast(e.message));
  $("[data-archived]").onchange = (e) => {
    archived = e.target.checked;
    refreshList().catch((e) => toast(e.message));
  };
  $("[data-files]").onchange = () => {
    $("[data-file-names]").textContent = [...$("[data-files]").files]
      .map((f) => f.name)
      .join(" · ");
    saveDraft();
  };
  $("[data-cancel-upload]").onclick = () => uploadController?.abort();
  $("[data-stop]").onclick = () =>
    call(`sessions/${session.sessionId}/interrupt`, {})
      .then(() => notify("正在停止 Claude 并等待已开始的镜像写入…"))
      .catch((e) => toast(e.message));
  $("[data-reconcile]").onclick = () =>
    call(`sessions/${session.sessionId}/reconcile`, {})
      .then(poll)
      .catch((e) => toast(e.message));
  $("[data-child]").onchange = () => {
    epoch++;
    subpath = $("[data-child]").value;
    cursor = 0;
    cards.clear();
    textByKey.clear();
    $("[data-trace]").replaceChildren();
    state(session);
    void loadHistory()
      .then(poll)
      .catch((e) => toast(e.message));
  };
  $("[data-rename]").onclick = async () => {
    if (!session) return;
    const title = prompt("会话标题", session.title);
    if (title == null) return;
    try {
      await api(`/v1/claude/sessions/${session.sessionId}`, {
        method: "PATCH",
        body: JSON.stringify({ title }),
      });
      session.title = title;
      state(session);
      await refreshList();
    } catch (e) {
      toast(e.message);
    }
  };
  $("[data-archive]").onclick = async () => {
    if (!session) return;
    try {
      await api(`/v1/claude/sessions/${session.sessionId}`, {
        method: "PATCH",
        body: JSON.stringify({ archived: !session.archived }),
      });
      session.archived = !session.archived;
      await refreshList();
      notify(session.archived ? "已归档" : "已恢复");
    } catch (e) {
      toast(e.message);
    }
  };
  $("[data-node]").onchange = () => {
    const node = nodes.find((n) => n.nodeId === $("[data-node]").value);
    if (!session)
      $("[data-cwd]").value = node?.desiredAppServer?.defaultCwd || "";
  };
  return {
    async open(id) {
      show("claudeView");
      const result = await api("/v1/nodes");
      nodes = (result.nodes || result.data || result).filter(
        (n) => n.capabilities?.claudeRuntimeV1,
      );
      const previous = $("[data-node]").value;
      $("[data-node]").replaceChildren();
      for (const n of nodes)
        $("[data-node]").add(
          new Option(n.displayName || n.hostname || n.nodeId, n.nodeId),
        );
      if (nodes.some((n) => n.nodeId === previous))
        $("[data-node]").value = previous;
      if (!session) $("[data-node]").onchange();
      await refreshList();
      clearInterval(timer);
      timer = setInterval(() => void poll(), 750);
      if (id)
        await select(await call(`sessions/${id}`), { updateRoute: false });
      else if (!session) draft();
      else {
        route(session.sessionId);
        await poll();
      }
    },
    hide() {
      saveDraft();
      clearInterval(timer);
      epoch++;
    },
  };
}

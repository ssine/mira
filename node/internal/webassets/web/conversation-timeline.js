// Conversation timeline: one lane per agent (the root and its subagents) on a
// shared time axis. Continuous activity forms a segment; idle gaps longer than
// the threshold are cut out and replaced by a fixed-width break, so a resumed
// conversation continues right next to the previous segment.

const svgNS = "http://www.w3.org/2000/svg";
export const idleBreakMs = 10 * 60 * 1000;
const breakWidth = 60;
const chartPad = 10;
const minSegmentWidth = 28;
const laneHeight = 14;
const axisHeight = 16;
const labelWidth = () => matchMedia("(max-width: 640px)").matches ? 56 : 72;
const heightKey = "mira.timeline.height";
const tickSteps = [1e3, 5e3, 15e3, 30e3, 60e3, 5 * 60e3, 15 * 60e3, 30 * 60e3, 3600e3, 2 * 3600e3, 6 * 3600e3, 12 * 3600e3, 86400e3];

export function formatDuration(ms) {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分${seconds % 60 ? ` ${seconds % 60} 秒` : ""}`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时${minutes % 60 ? ` ${minutes % 60} 分` : ""}`;
  const days = Math.floor(hours / 24);
  return `${days} 天${hours % 24 ? ` ${hours % 24} 小时` : ""}`;
}

// Break labels: at most two units, without spaces (e.g. 1小时55分, 3天2小时).
export function compactDuration(ms) {
  return formatDuration(ms).replace(/\s+/g, "");
}

const clock = (at, seconds = false) => new Date(at).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", ...(seconds ? { second: "2-digit" } : {}), hour12: false });
const day = at => new Date(at).toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });

// Union of intervals, sorted. Inputs are [start, end] pairs in milliseconds.
export function unionIntervals(intervals) {
  const sorted = intervals.filter(([start, end]) => Number.isFinite(start) && Number.isFinite(end))
    .map(([start, end]) => [start, Math.max(start, end)]).sort((a, b) => a[0] - b[0]);
  const result = [];
  for (const [start, end] of sorted) {
    const last = result.at(-1);
    if (last && start <= last[1]) last[1] = Math.max(last[1], end);
    else result.push([start, end]);
  }
  return result;
}

// Activity of the whole tree, including message instants, joined across idle
// gaps shorter than the threshold.
export function activitySegments(lanes, gapMs = idleBreakMs) {
  const intervals = [];
  for (const lane of lanes) {
    for (const turn of lane.turns ?? []) intervals.push([turn.start, turn.end]);
    for (const tool of lane.tools ?? []) intervals.push([tool.start, tool.end]);
    for (const message of lane.messages ?? []) intervals.push([message.at, message.at]);
  }
  const segments = [];
  for (const [start, end] of unionIntervals(intervals)) {
    const last = segments.at(-1);
    if (last && start - last.end <= gapMs) last.end = Math.max(last.end, end);
    else segments.push({ start, end });
  }
  return segments;
}

// Model phases: turn time not covered by tools or explicit waits.
export function modelPhases(lane) {
  const busy = unionIntervals((lane.tools ?? []).map(tool => [tool.start, tool.end]));
  const result = [];
  for (const turn of lane.turns ?? []) {
    let cursor = turn.start;
    for (const [start, end] of busy) {
      if (end <= cursor || start >= turn.end) continue;
      if (start > cursor) result.push([cursor, start]);
      cursor = Math.max(cursor, end);
    }
    if (cursor < turn.end) result.push([cursor, turn.end]);
  }
  return result.filter(([start, end]) => end > start);
}

// Real time inside segments, fixed-width breaks between them. Every segment
// shares one ms-per-pixel scale (bounded below by a minimum readable width).
export function timelineLayout(segments, width, zoom = 1) {
  const durations = segments.map(segment => Math.max(1, segment.end - segment.start));
  const total = durations.reduce((sum, value) => sum + value, 0);
  const available = Math.max(120, width - breakWidth * Math.max(0, segments.length - 1));
  const scale = available * zoom / Math.max(total, 1);
  let x = 0;
  const placed = segments.map((segment, index) => {
    const start = x, segmentWidth = Math.max(minSegmentWidth, durations[index] * scale);
    x += segmentWidth + (index < segments.length - 1 ? breakWidth : 0);
    return { ...segment, x: start, width: segmentWidth, scale: segmentWidth / durations[index] };
  });
  const position = at => {
    let previous = null;
    for (const segment of placed) {
      if (at < segment.start) return previous ? previous.x + previous.width : segment.x;
      if (at <= segment.end) return segment.x + (at - segment.start) * segment.scale;
      previous = segment;
    }
    return previous ? previous.x + previous.width : 0;
  };
  return { segments: placed, width: Math.max(x, 1), position };
}

function svg(tag, attributes = {}, title) {
  const node = document.createElementNS(svgNS, tag);
  for (const [name, value] of Object.entries(attributes)) if (value !== undefined && value !== null) node.setAttribute(name, String(value));
  if (title) {
    const label = document.createElementNS(svgNS, "title");
    label.textContent = title;
    node.append(label);
  }
  return node;
}

// While the conversation runs, unfinished turns and tool calls grow to "now".
// Stale unfinished intervals (no evidence for 15 minutes) keep their last
// evidence, so an abandoned call never stretches across hours.
const liveHorizonMs = 15 * 60 * 1000;
export function liveLanes(lanes, now, running) {
  if (!running) return lanes;
  const extend = item => item.open && now >= item.end && now - item.end < liveHorizonMs ? { ...item, end: now, live: true } : item;
  return lanes.map(lane => ({ ...lane, turns: (lane.turns ?? []).map(extend), tools: (lane.tools ?? []).map(extend) }));
}

export const hourMs = 60 * 60 * 1000;
// The fixed window ends now while running, otherwise at the last activity.
export function hourWindow(lanes, now, running) {
  const segments = activitySegments(lanes);
  const end = running ? now : segments.at(-1)?.end ?? now;
  return { start: end - hourMs, end };
}

export function timelineSummary(data) {
  const segments = activitySegments(data.lanes ?? []);
  const active = segments.reduce((sum, segment) => sum + segment.end - segment.start, 0);
  return `${segments.length} 段 · 活跃 ${formatDuration(active)}`;
}

// Tool and subagent counts live in the info popover to keep the header short.
export function timelineDetails(data) {
  const tools = (data.lanes ?? []).reduce((sum, lane) => sum + (lane.tools?.length ?? 0), 0);
  const children = Math.max(0, (data.lanes?.length ?? 1) - 1);
  return [`工具 ${tools} 次`, children ? `子 Agent ${children} 个` : ""].filter(Boolean).join(" · ");
}

const lastActivity = lane => Math.max(0, ...(lane.turns ?? []).map(turn => turn.end), ...(lane.tools ?? []).map(tool => tool.end),
  ...(lane.messages ?? []).map(message => message.at));

// Root first, then each agent's children newest first (by latest activity),
// each followed by its own descendants.
export function orderLanes(lanes) {
  const ids = new Set(lanes.map(lane => lane.threadId));
  const children = new Map();
  const roots = [];
  for (const lane of lanes) {
    const parent = lane.parentThreadId && ids.has(lane.parentThreadId) && lane.depth > 0 ? lane.parentThreadId : null;
    if (parent) children.set(parent, [...(children.get(parent) ?? []), lane]); else roots.push(lane);
  }
  const ordered = [];
  const visit = lane => {
    ordered.push(lane);
    for (const child of (children.get(lane.threadId) ?? []).sort((a, b) => lastActivity(b) - lastActivity(a))) visit(child);
  };
  roots.forEach(visit);
  return ordered;
}

export class ConversationTimeline {
  // The timeline is a pinned, non-modal panel at the top of the conversation.
  // Its visibility is a remembered preference shared by all conversations;
  // the navigation menu toggles it and onPinnedChange reflects the state.
  constructor({ root, load, onJump, onOpenThread, onPinnedChange, returnFocus }) {
    this.root = root;
    this.onPinnedChange = onPinnedChange;
    this.returnFocus = returnFocus;
    this.load = load;
    this.onJump = onJump;
    this.onOpenThread = onOpenThread;
    this.summary = root.querySelector("[data-timeline-summary]");
    this.body = root.querySelector("[data-timeline-body]");
    this.controls = root.querySelector("[data-timeline-controls]");
    this.info = root.querySelector("[data-timeline-info]");
    this.resizer = root.querySelector("[data-timeline-resize]");
    this.installResize();
    this.threadId = null;
    this.data = null;
    this.zoom = 1;
    this.request = 0;
    this.timer = null;
    this.ticker = null;
    try { this.mode = localStorage.getItem("mira.timeline.mode") === "hour" ? "hour" : "all"; } catch { this.mode = "all"; }
    try { this.pinned = localStorage.getItem("mira.timeline.pinned") === "1"; } catch { this.pinned = false; }
    root.querySelector("[data-timeline-close]")?.addEventListener("click", () => { this.setPinned(false); this.returnFocus?.(); });
    root.addEventListener("keydown", event => {
      if (event.key !== "Escape") return;
      // Handled here: do not also close the conversation drawer.
      event.preventDefault(); event.stopPropagation();
      this.setPinned(false); this.returnFocus?.();
    });
    root.addEventListener("click", event => {
      const info = event.target.closest("[data-timeline-info-toggle]");
      if (info) { this.info.hidden = !this.info.hidden; info.setAttribute("aria-expanded", String(!this.info.hidden)); return; }
      const zoom = event.target.closest("[data-timeline-zoom]");
      if (zoom) { this.setZoom(zoom.dataset.timelineZoom); return; }
      const mode = event.target.closest("[data-timeline-mode]");
      if (mode) { this.setMode(mode.dataset.timelineMode); return; }
      const message = event.target.closest("[data-timeline-seq]");
      if (message) { this.onJump?.(message.dataset.timelineThread, Number(message.dataset.timelineSeq), message.dataset.timelineRole); return; }
      const lane = event.target.closest("[data-timeline-open]");
      if (lane) this.onOpenThread?.(lane.dataset.timelineOpen);
    });
    this.body.addEventListener("keydown", event => {
      if ((event.key === "Enter" || event.key === " ") && event.target.closest("[data-timeline-seq]")) {
        event.preventDefault();
        event.target.closest("[data-timeline-seq]").dispatchEvent(new MouseEvent("click", { bubbles: true }));
      }
    });
    this.body.addEventListener("wheel", event => {
      if (!event.ctrlKey && !event.metaKey) return;
      event.preventDefault();
      this.setZoom(event.deltaY < 0 ? "in" : "out");
    }, { passive: false });
  }

  get open() { return !this.root.hidden; }

  // Drag the bottom edge to choose the panel height (remembered); double-click
  // or Home returns to the automatic height that fits all lanes.
  installResize() {
    if (!this.resizer) return;
    const apply = height => {
      if (height == null) { this.root.style.height = ""; this.root.dataset.sized = "false"; return; }
      const max = Math.round(innerHeight * 0.8);
      const value = Math.max(64, Math.min(max, Math.round(height)));
      this.root.style.height = `${value}px`;
      this.root.dataset.sized = "true";
      this.resizer.setAttribute("aria-valuenow", String(value));
      return value;
    };
    const save = value => { try { if (value == null) localStorage.removeItem(heightKey); else localStorage.setItem(heightKey, String(value)); } catch { /* Preference only. */ } };
    let saved = null;
    try { saved = Number(localStorage.getItem(heightKey)) || null; } catch { saved = null; }
    apply(saved);
    let drag = null;
    this.resizer.addEventListener("pointerdown", event => {
      if (event.button !== 0) return;
      event.preventDefault();
      drag = { id: event.pointerId, y: event.clientY, height: this.root.getBoundingClientRect().height };
      this.resizer.setPointerCapture?.(event.pointerId);
      this.root.dataset.resizing = "true";
    });
    this.resizer.addEventListener("pointermove", event => {
      if (drag?.id === event.pointerId) apply(drag.height + event.clientY - drag.y);
    });
    const end = event => {
      if (drag?.id !== event.pointerId) return;
      drag = null;
      delete this.root.dataset.resizing;
      save(this.root.dataset.sized === "true" ? Math.round(this.root.getBoundingClientRect().height) : null);
    };
    this.resizer.addEventListener("pointerup", end);
    this.resizer.addEventListener("pointercancel", end);
    this.resizer.addEventListener("dblclick", () => { apply(null); save(null); });
    this.resizer.addEventListener("keydown", event => {
      const step = event.shiftKey ? 48 : 16;
      const current = this.root.getBoundingClientRect().height;
      if (event.key === "ArrowDown") save(apply(current + step));
      else if (event.key === "ArrowUp") save(apply(current - step));
      else if (event.key === "Home") { apply(null); save(null); }
      else return;
      event.preventDefault();
    });
  }

  setPinned(pinned) {
    this.pinned = !!pinned;
    try { localStorage.setItem("mira.timeline.pinned", this.pinned ? "1" : "0"); } catch { /* Preference only. */ }
    this.show();
    this.onPinnedChange?.(this.pinned);
  }

  // Visible while pinned and a conversation is open; nothing is read otherwise.
  show() {
    const open = this.pinned && !!this.threadId;
    const was = this.open;
    this.root.hidden = !open;
    if (open && !was) void this.refresh();
    else if (!open) this.stop();
  }

  select(threadId) {
    if (threadId === this.threadId) return;
    this.threadId = threadId;
    this.data = null;
    this.zoom = 1;
    this.request++;
    this.running = false;
    this.summary.textContent = "";
    this.body.replaceChildren();
    this.root.hidden = true;
    this.stop();
    this.show();
  }

  stop() { clearTimeout(this.timer); this.timer = null; this.setTicker(false); }

  // Re-render every second while something is growing (or the hour window
  // slides) and the panel is open; the data itself refreshes every 10 s.
  setTicker(on) {
    if (on && !this.ticker && this.open) this.ticker = setInterval(() => { if (this.open) this.render(); else this.setTicker(false); }, 1000);
    else if (!on && this.ticker) { clearInterval(this.ticker); this.ticker = null; }
  }

  setMode(mode) {
    this.mode = mode === "hour" ? "hour" : "all";
    try { localStorage.setItem("mira.timeline.mode", this.mode); } catch { /* Preference only. */ }
    this.zoom = 1;
    this.render();
  }

  // running: keep following an active conversation while expanded.
  async refresh({ running = false } = {}) {
    this.running = running || this.running;
    if (!this.open || !this.threadId) return;
    const threadId = this.threadId, request = ++this.request;
    if (!this.data) this.body.replaceChildren(Object.assign(document.createElement("p"), { className: "timeline-status", textContent: "正在读取时间线…" }));
    try {
      const data = await this.load(threadId);
      if (request !== this.request || threadId !== this.threadId) return;
      this.data = data;
      this.render();
    } catch (error) {
      if (request !== this.request) return;
      if (!this.data) this.body.replaceChildren(Object.assign(document.createElement("p"), { className: "timeline-status", textContent: `时间线读取失败：${error.message}` }));
    }
    this.stop();
    if (this.open && this.running) this.timer = setTimeout(() => void this.refresh(), 10_000);
  }

  setRunning(running) {
    this.running = running;
    if (!running) { this.setTicker(false); if (this.data) this.render(); }
    if (running && this.open && !this.timer) this.timer = setTimeout(() => void this.refresh(), 2_000);
  }

  setZoom(direction) {
    this.zoom = direction === "in" ? Math.min(64, this.zoom * 1.6) : direction === "out" ? Math.max(1, this.zoom / 1.6) : 1;
    this.render();
  }

  render() {
    const data = this.data;
    if (!data) return;
    const now = Date.now(), hour = this.mode === "hour";
    const lanes = liveLanes(data.lanes ?? [], now, this.running);
    const live = lanes.some(lane => [...(lane.turns ?? []), ...(lane.tools ?? [])].some(item => item.live));
    this.setTicker(live);
    const all = activitySegments(lanes);
    const range = hour && all.length ? hourWindow(lanes, now, this.running) : null;
    // The hour view is one real-time segment: idle time is shown, not folded.
    const segments = range ? [range] : all;
    const visible = item => !range || item.end >= range.start && item.start <= range.end;
    this.summary.textContent = all.length ? (hour ? "最近 1 小时" : timelineSummary({ ...data, lanes })) : "";
    if (!segments.length) {
      this.body.replaceChildren(Object.assign(document.createElement("p"), { className: "timeline-status", textContent: "还没有可显示的活动。" }));
      return;
    }
    const scroll = this.body.querySelector(".timeline-scroll");
    const previousScroll = scroll?.scrollLeft ?? null;
    const labelsWidth = labelWidth();
    const available = Math.max(200, (this.body.clientWidth || this.root.clientWidth || 720) - labelsWidth - chartPad * 2 - 2);
    const layout = timelineLayout(segments, available, this.zoom);
    const ordered = orderLanes(lanes);
    const height = axisHeight + ordered.length * laneHeight + 2;

    const legend = document.createElement("ul");
    legend.className = "timeline-legend";
    const modes = document.createElement("div");
    modes.className = "timeline-modes";
    modes.setAttribute("role", "group");
    modes.setAttribute("aria-label", "时间范围");
    for (const [value, text] of [["all", "全部"], ["hour", "最近 1 小时"]]) {
      const button = document.createElement("button");
      button.type = "button";
      button.dataset.timelineMode = value;
      button.textContent = text;
      button.setAttribute("aria-pressed", String(this.mode === value));
      modes.append(button);
    }
    for (const [kind, label] of [["model", "模型"], ["tool", "工具"], ["wait", "等待"], ["user", "你的消息"], ...(hour ? [] : [["break", "空闲已折叠"]]), ...(live ? [["live", "进行中"]] : [])]) {
      const item = document.createElement("li");
      item.dataset.kind = kind;
      item.textContent = label;
      legend.append(item);
    }
    const zoom = document.createElement("div");
    zoom.className = "timeline-zoom";
    for (const [value, text, label] of [["out", "−", "缩小"], ["fit", "适应", "适应宽度"], ["in", "＋", "放大"]]) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "chat-icon-button";
      button.dataset.timelineZoom = value;
      button.textContent = text;
      button.title = label;
      button.setAttribute("aria-label", `时间线${label}`);
      if (value === "out" || value === "fit") button.disabled = this.zoom === 1;
      zoom.append(button);
    }
    const infoToggle = document.createElement("button");
    infoToggle.type = "button";
    infoToggle.className = "chat-icon-button timeline-info-toggle";
    infoToggle.dataset.timelineInfoToggle = "";
    infoToggle.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="7"></circle><path d="M10 9v4.5M10 6.6v.1"></path></svg>';
    infoToggle.title = "图例与统计";
    infoToggle.setAttribute("aria-label", "时间线图例与统计");
    infoToggle.setAttribute("aria-expanded", String(!this.info.hidden));
    if (data.partial) infoToggle.dataset.partial = "true";
    this.controls.replaceChildren(modes, zoom, infoToggle);
    const details = document.createElement("p");
    details.className = "timeline-details";
    details.textContent = [hour ? timelineSummary({ ...data, lanes }) : "", timelineDetails({ ...data, lanes })].filter(Boolean).join(" · ");
    const infoChildren = [details, legend];
    if (data.partial) {
      const note = document.createElement("p");
      note.className = "timeline-partial";
      note.textContent = "部分历史仍在建立索引";
      infoChildren.push(note);
    }
    this.info.replaceChildren(...infoChildren);

    const grid = document.createElement("div");
    grid.className = "timeline-grid";
    grid.style.gridTemplateColumns = `${labelsWidth}px minmax(0, 1fr)`;
    const labels = document.createElement("div");
    labels.className = "timeline-labels";
    labels.style.height = `${height}px`;
    const axisLabel = document.createElement("div");
    axisLabel.className = "timeline-axis-label";
    labels.append(axisLabel);
    for (const lane of ordered) {
      const label = document.createElement(lane.depth > 0 ? "button" : "div");
      label.className = "timeline-lane-label";
      label.style.paddingInlineStart = `${6 + Math.min(lane.depth, 4) * 6}px`;
      label.textContent = lane.depth > 0 ? (lane.title || "子 Agent") : (lane.title || "本对话");
      label.title = label.textContent;
      if (lane.depth > 0 && data.engine === "codex") {
        label.type = "button";
        label.dataset.timelineOpen = lane.threadId;
        label.title = `${label.textContent}（打开子 Agent）`;
      } else if (lane.depth > 0) label.disabled = true;
      labels.append(label);
    }

    const scroller = document.createElement("div");
    scroller.className = "timeline-scroll";
    const chart = svg("svg", { width: layout.width + chartPad * 2, height, class: "timeline-chart", role: "img",
      "aria-label": `会话时间线：${timelineSummary(data)}` });
    const x = at => chartPad + layout.position(at);
    // Axis: date at each segment start, then ticks at a readable step.
    const axis = svg("g", { class: "timeline-axis" });
    let previousDay = null;
    layout.segments.forEach((segment, index) => {
      const step = tickSteps.find(value => value * segment.scale >= 72) ?? tickSteps.at(-1);
      const left = chartPad + segment.x, right = left + segment.width;
      axis.append(svg("line", { x1: left, x2: right, y1: axisHeight - 1, y2: axisHeight - 1, class: "timeline-axis-line" }));
      const startDay = day(segment.start);
      // One row: the bold segment start, then ticks clear of it.
      const startLabel = svg("text", { x: left, y: 11, class: "timeline-axis-start" });
      startLabel.textContent = `${startDay !== previousDay ? `${startDay} ` : ""}${clock(segment.start)}`;
      previousDay = startDay;
      axis.append(startLabel);
      for (let at = Math.ceil(segment.start / step) * step; at <= segment.end; at += step) {
        const position = x(at);
        // Keep tick labels clear of the segment's start label and its break.
        const startWidth = (startDay !== day(layout.segments[index - 1]?.start ?? 0) ? 72 : 40);
        if (position - left < startWidth + 18 || right - position < 22) continue;
        axis.append(svg("line", { x1: position, x2: position, y1: axisHeight - 4, y2: axisHeight - 1, class: "timeline-axis-tick" }));
        const label = svg("text", { x: position, y: 11, class: "timeline-axis-text", "text-anchor": "middle" });
        label.textContent = clock(at, step < 60e3);
        axis.append(label);
      }
      const next = layout.segments[index + 1];
      if (next) {
        const gap = svg("g", { class: "timeline-break" }, `空闲 ${formatDuration(next.start - segment.end)}（已折叠）`);
        gap.append(svg("rect", { x: right + 3, y: 0, width: breakWidth - 6, height, class: "timeline-break-band" }));
        const zig = [];
        for (let y = axisHeight; y < height; y += 8) zig.push(`${right + breakWidth / 2 - 3},${y} ${right + breakWidth / 2 + 3},${y + 4}`);
        gap.append(svg("polyline", { points: zig.join(" "), class: "timeline-break-zigzag" }));
        // The gap length sits on the tick row; segment start labels use the row above.
        const text = svg("text", { x: right + breakWidth / 2, y: 11, class: "timeline-break-text", "text-anchor": "middle" });
        text.textContent = compactDuration(next.start - segment.end);
        gap.append(text);
        axis.append(gap);
      }
    });
    chart.append(axis);
    ordered.forEach((lane, index) => {
      const top = axisHeight + index * laneHeight;
      const row = svg("g", { class: "timeline-lane", "data-thread-id": lane.threadId });
      row.append(svg("rect", { x: 0, y: top, width: layout.width + chartPad * 2, height: laneHeight, class: index % 2 ? "timeline-row odd" : "timeline-row" }));
      for (const turn of (lane.turns ?? []).filter(visible)) {
        row.append(svg("rect", { x: x(turn.start), y: top + laneHeight / 2 - 1, width: Math.max(1, x(turn.end) - x(turn.start)), height: 2, class: `timeline-turn${turn.live ? " live" : turn.open ? " open" : ""}` },
          `轮次 · ${clock(turn.start, true)} 起 · ${formatDuration(turn.end - turn.start)}${turn.live ? " · 进行中" : turn.open ? " · 未观测到结束" : turn.status === "aborted" ? " · 已中断" : ""}`));
      }
      for (const [start, end] of modelPhases(lane).filter(([start, end]) => visible({ start, end }))) {
        const growing = (lane.turns ?? []).some(turn => turn.live && end === turn.end);
        row.append(svg("rect", { x: x(start), y: top + 3, width: Math.max(1, x(end) - x(start)), height: laneHeight - 6, class: `timeline-span model${growing ? " live" : ""}` },
          `模型阶段 · ${formatDuration(end - start)}${growing ? " · 进行中" : ""}（按工具边界归因）`));
      }
      for (const tool of (lane.tools ?? []).filter(visible)) {
        row.append(svg("rect", { x: x(tool.start), y: top + 2, width: Math.max(2, x(tool.end) - x(tool.start)), height: laneHeight - 4,
          class: `timeline-span ${tool.kind === "wait" ? "wait" : "tool"}${tool.live ? " live" : tool.open ? " open" : ""}` },
          `${tool.name} · ${clock(tool.start, true)} · ${formatDuration(tool.end - tool.start)}${tool.live ? " · 进行中" : tool.open ? " · 未观测到返回" : ""}`));
      }
      for (const message of (lane.messages ?? []).filter(message => visible({ start: message.at, end: message.at }))) {
        const cx = x(message.at);
        if (message.role === "user") {
          const marker = svg("path", { d: `M${cx} ${top + 1} l4 6 l-4 6 l-4 -6 z`, class: "timeline-message user", tabindex: 0, role: "button",
            "data-timeline-seq": message.seq, "data-timeline-thread": lane.threadId, "data-timeline-role": "user",
            "aria-label": `跳到你的消息：${message.preview}` }, `${clock(message.at, true)} · ${message.preview}`);
          row.append(marker);
        } else {
          row.append(svg("line", { x1: cx, x2: cx, y1: top + 2, y2: top + laneHeight - 2, class: "timeline-message assistant",
            "data-timeline-seq": message.seq, "data-timeline-thread": lane.threadId, "data-timeline-role": "assistant" }, `回复 · ${clock(message.at, true)} · ${message.preview}`));
        }
      }
      chart.append(row);
    });
    scroller.append(chart);
    grid.append(labels, scroller);
    this.body.replaceChildren(grid);
    // Zoomed views follow the growing end unless the reader scrolled away.
    const following = scroll && scroll.scrollLeft + scroll.clientWidth >= scroll.scrollWidth - 4;
    if (previousScroll !== null && !(live && following)) scroller.scrollLeft = previousScroll;
    else if (this.zoom > 1) scroller.scrollLeft = scroller.scrollWidth;
  }
}

// Conversation timeline: one lane per agent (the root and its subagents) on a
// shared time axis. Continuous activity forms a segment; idle gaps longer than
// the threshold are cut out and replaced by a fixed-width break, so a resumed
// conversation continues right next to the previous segment.

const svgNS = "http://www.w3.org/2000/svg";
export const idleBreakMs = 10 * 60 * 1000;
const breakWidth = 60;
const chartPad = 10;
const minSegmentWidth = 28;
const laneHeight = 12;
const axisHeight = 14;
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

// Formatters are reused: a long conversation labels thousands of elements.
const clockFormat = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
const clockSecondsFormat = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
const dayFormat = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric" });
const clock = (at, seconds = false) => (seconds ? clockSecondsFormat : clockFormat).format(at);
const day = at => dayFormat.format(at);

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
const windowKey = "mira.timeline.window";
export const minWindowMs = 60e3;
export const maxWindowMs = 30 * 86400e3;
export const windowPresets = [10 * 60e3, 30 * 60e3, hourMs, 3 * hourMs, 6 * hourMs, 12 * hourMs, 24 * hourMs];
export const clampWindow = ms => Number.isFinite(ms) && ms > 0 ? Math.min(maxWindowMs, Math.max(minWindowMs, Math.round(ms / 60e3) * 60e3)) : hourMs;
// "1 小时", "90 分钟" → "1 小时 30 分钟", "2 天".
export function formatWindow(ms) {
  const minutes = Math.round(clampWindow(ms) / 60e3);
  if (minutes < 60) return `${minutes} 分钟`;
  if (minutes % 1440 === 0) return `${minutes / 1440} 天`;
  return `${Math.floor(minutes / 60)} 小时${minutes % 60 ? ` ${minutes % 60} 分钟` : ""}`;
}
// The recent window ends now while running, otherwise at the last activity.
export function hourWindow(lanes, now, running, windowMs = hourMs) {
  const segments = activitySegments(lanes);
  const end = running ? now : segments.at(-1)?.end ?? now;
  return { start: end - windowMs, end };
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

// Incremental reads (see GetCodexTimeline/GetClaudeTimeline) carry only what
// changed. Turns and tools merge by ID (tools without one by sequence),
// messages by sequence; a Close item only ends a tool the client already has.
// Lanes missing from an update keep their items; Reset lanes start over.
const toolKey = tool => tool.id ? `id:${tool.id}` : `seq:${tool.seq}`;
const messageKey = message => `${message.seq}:${message.role}:${message.preview}`;
function mergeItems(previous = [], updates = [], key) {
  const items = previous.slice();
  const index = new Map(items.map((item, position) => [key(item), position]));
  for (const item of updates ?? []) {
    const position = index.get(key(item));
    if (position === undefined) {
      if (item.close) continue;
      index.set(key(item), items.length);
      items.push(item);
      continue;
    }
    const prior = items[position];
    items[position] = item.close ? { ...prior, end: Math.max(prior.end, item.end), open: false }
      : "start" in item ? { ...prior, ...item, start: Math.min(prior.start, item.start), end: Math.max(prior.end, item.end), open: item.open === true }
      : item;
  }
  return items.sort((a, b) => (a.start ?? a.at) - (b.start ?? b.at) || (a.seq ?? 0) - (b.seq ?? 0));
}

export function mergeTimeline(data, update) {
  if (!data || !update?.delta) return update;
  const previous = new Map((data.lanes ?? []).map(lane => [lane.threadId, lane]));
  const lanes = [], seen = new Set();
  for (const lane of update.lanes ?? []) {
    seen.add(lane.threadId);
    const old = previous.get(lane.threadId);
    const meta = { threadId: lane.threadId, parentThreadId: lane.parentThreadId ?? old?.parentThreadId ?? null,
      depth: lane.depth ?? old?.depth ?? 0, title: lane.title ?? old?.title ?? null };
    if (lane.unchanged && old) { lanes.push({ ...old, ...meta }); continue; }
    const base = old && !lane.reset ? old : {};
    lanes.push({ ...meta, ...(lane.partial ? { partial: true } : {}), turns: mergeItems(base.turns, lane.turns, turn => turn.id),
      tools: mergeItems(base.tools, lane.tools, toolKey), messages: mergeItems(base.messages, lane.messages, messageKey) });
  }
  for (const lane of data.lanes ?? []) if (!seen.has(lane.threadId)) lanes.push(lane);
  // A Claude subagent first seen in an increment names itself after its Task call.
  const tools = new Map(lanes.flatMap(lane => (lane.tools ?? []).map(tool => [tool.id, tool])));
  for (const lane of lanes) {
    if (lane.title == null && lane.depth > 0) lane.title = tools.get(lane.threadId.split(":")[1])?.name ?? null;
  }
  return { ...data, ...update, lanes, delta: false, partial: lanes.some(lane => lane.partial) };
}

// The live pulse restarts whenever an element is created. Align every live
// element to one global phase so re-renders continue it seamlessly.
const pulsePeriodMs = 2400;
const alignPulse = root => {
  const delay = `-${Math.round(performance.now() % pulsePeriodMs)}ms`;
  for (const element of root.querySelectorAll(".timeline-span.live, .timeline-turn.live")) element.style.animationDelay = delay;
};
// The recent view drawn this far past now, so it can slide without redrawing.
const panLeadMs = 30_000;
let clipCounter = 0;

// A long conversation packs thousands of spans into one panel width. Adjacent
// spans of one kind that are too narrow to tell apart are drawn as one, so the
// chart has about as many elements as pixels rather than as tool calls.
export function drawnSpans(items, x, { kind = () => "", narrow = 2, gap = 1 } = {}) {
  const drawn = [], open = new Map();
  for (const item of [...items].sort((left, right) => left.start - right.start)) {
    const left = x(item.start), right = Math.max(left, x(item.end)), type = kind(item), last = open.get(type);
    if (last && right - left < narrow && left - last.right < gap && last.mergeable) {
      last.right = Math.max(last.right, right);
      last.items.push(item);
      continue;
    }
    const span = { left, right, kind: type, items: [item], mergeable: right - left < narrow };
    open.set(type, span);
    drawn.push(span);
  }
  return drawn;
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
    this.inlineInfo = root.querySelector("[data-timeline-inline-info]");
    this.head = root.querySelector(".timeline-head");
    this.resizer = root.querySelector("[data-timeline-resize]");
    this.installResize();
    this.threadId = null;
    this.data = null;
    this.zoom = 1;
    this.request = 0;
    this.timer = null;
    this.ticker = null;
    this.settle = null;
    this.inflight = false;
    this.pending = false;
    this.lastRead = 0;
    this.pan = null;
    this.panFrame = 0;
    // "hour" is the stored name of the recent mode; its length is separate.
    try { this.mode = localStorage.getItem("mira.timeline.mode") === "hour" ? "hour" : "all"; } catch { this.mode = "all"; }
    try { this.windowMs = clampWindow(Number(localStorage.getItem(windowKey)) || hourMs); } catch { this.windowMs = hourMs; }
    this.installWindowMenu();
    // Show the legend and counts in the header whenever they fit there.
    if (typeof ResizeObserver === "function") {
      let frame = 0;
      new ResizeObserver(() => {
        cancelAnimationFrame(frame);
        frame = requestAnimationFrame(() => this.fitInfo());
      }).observe(root);
    }
    try { this.pinned = localStorage.getItem("mira.timeline.pinned") === "1"; } catch { this.pinned = false; }
    root.querySelector("[data-timeline-close]")?.addEventListener("click", () => { this.setPinned(false); this.returnFocus?.(); });
    root.addEventListener("keydown", event => {
      if (event.key !== "Escape" || this.windowMenu?.contains(event.target)) return;
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
      const value = Math.max(52, Math.min(max, Math.round(height)));
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
    if (open && !was) void this.refresh({ full: true });
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

  stop() {
    clearTimeout(this.timer); clearTimeout(this.settle); clearTimeout(this.fallback);
    this.timer = this.settle = this.fallback = null;
    this.pending = false;
    this.setTicker(false);
    this.stopPan();
  }

  // Whether a notification about this thread can change the timeline.
  follows(threadId) {
    return !!threadId && (threadId === this.threadId || (this.data?.lanes ?? []).some(lane => lane.threadId === threadId));
  }

  // New durable activity (a Claude durable signal, a Codex item/turn
  // notification or a changed item count): read what changed. At most one read
  // is in flight and reads start at most once a second; a burst of signals
  // coalesces into one read, and one more read follows the last signal
  // because a notification can arrive just before its record is stored.
  notify() {
    if (!this.open || !this.threadId || !this.data) return;
    this.pending = true;
    this.schedule();
    clearTimeout(this.settle);
    this.settle = setTimeout(() => { this.settle = null; this.pending = true; this.schedule(); }, 2500);
  }

  schedule() {
    if (this.inflight || this.timer || !this.pending) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      if (!this.pending) return;
      this.pending = false;
      void this.refresh();
    }, Math.max(0, this.lastRead + 1000 - Date.now()));
  }

  // Re-render while something is growing and the panel is open.
  setTicker(on, interval = 1000) {
    if (this.ticker && (!on || this.tickerInterval !== interval)) { clearInterval(this.ticker); this.ticker = null; }
    if (!on || this.ticker || !this.open) return;
    this.tickerInterval = interval;
    this.ticker = setInterval(() => { if (this.open) this.render(); else this.setTicker(false); }, interval);
  }

  setMode(mode) {
    this.mode = mode === "hour" ? "hour" : "all";
    try { localStorage.setItem("mira.timeline.mode", this.mode); } catch { /* Preference only. */ }
    this.zoom = 1;
    this.render();
  }

  // Choose the recent window's length (remembered) and switch to it.
  setWindow(ms) {
    this.windowMs = clampWindow(ms);
    try { localStorage.setItem(windowKey, String(this.windowMs)); } catch { /* Preference only. */ }
    this.setMode("hour");
  }

  // A popover (top layer, so the short panel never clips it) with preset
  // lengths and a custom number of minutes, hours or days.
  installWindowMenu() {
    const menu = document.createElement("div");
    menu.id = `${this.root.id || "conversationTimeline"}Window`;
    menu.className = "timeline-window-menu";
    menu.popover = "auto";
    menu.setAttribute("role", "dialog");
    menu.setAttribute("aria-label", "最近时间长度");
    const presets = document.createElement("div");
    presets.className = "timeline-window-presets";
    const form = document.createElement("form");
    form.className = "timeline-window-custom";
    form.innerHTML = '<label>自定义 <input type="number" name="amount" min="1" step="1" inputmode="numeric" required aria-label="时间长度"></label>'
      + '<select name="unit" aria-label="时间单位"><option value="60000">分钟</option><option value="3600000">小时</option><option value="86400000">天</option></select>'
      + '<button type="submit">应用</button>';
    menu.append(presets, form);
    this.root.append(menu);
    this.windowMenu = menu;
    menu.addEventListener("beforetoggle", event => {
      // toggle events are asynchronous; reflect the state as it changes.
      this.root.querySelector("[data-timeline-window-toggle]")?.setAttribute("aria-expanded", String(event.newState === "open"));
      if (event.newState !== "open") return;
      presets.replaceChildren(...windowPresets.map(ms => {
        const button = document.createElement("button");
        button.type = "button";
        button.dataset.timelineWindow = String(ms);
        button.textContent = formatWindow(ms);
        button.setAttribute("aria-pressed", String(this.mode === "hour" && this.windowMs === ms));
        return button;
      }));
      const minutes = this.windowMs / 60e3;
      const [amount, unit] = minutes % 1440 === 0 ? [minutes / 1440, 86400e3] : minutes % 60 === 0 ? [minutes / 60, 3600e3] : [minutes, 60e3];
      form.elements.amount.value = String(amount);
      form.elements.unit.value = String(unit);
      const anchor = this.root.querySelector("[data-timeline-window-toggle]")?.getBoundingClientRect();
      if (anchor) {
        menu.style.top = `${Math.round(anchor.bottom + 4)}px`;
        menu.style.left = `${Math.round(Math.max(8, Math.min(innerWidth - 228, anchor.right - 220)))}px`;
      }
    });
    presets.addEventListener("click", event => {
      const choice = event.target.closest("[data-timeline-window]");
      if (!choice) return;
      menu.hidePopover();
      this.setWindow(Number(choice.dataset.timelineWindow));
    });
    form.addEventListener("submit", event => {
      event.preventDefault();
      const amount = Number(form.elements.amount.value), unit = Number(form.elements.unit.value);
      if (!(amount > 0)) { form.elements.amount.focus(); return; }
      menu.hidePopover();
      this.setWindow(amount * unit);
    });
  }

  // The header shows counts and legend inline when they fit without
  // truncating the summary; otherwise the ⓘ button opens them below.
  fitInfo() {
    if (!this.inlineInfo || !this.head || this.root.hidden || !this.inlineInfo.childElementCount) return;
    this.root.dataset.infoInline = "true";
    const summary = this.summary;
    const fits = this.head.scrollWidth <= this.head.clientWidth + 1 && (!summary || summary.scrollWidth <= summary.clientWidth + 1);
    this.root.dataset.infoInline = String(fits);
    if (fits) this.info.hidden = true;
  }

  // Reads the whole timeline the first time (or with full), and afterwards
  // only what changed since the last response's cursor.
  async refresh({ full = false } = {}) {
    if (!this.open || !this.threadId) return;
    const threadId = this.threadId, request = ++this.request, base = full ? null : this.data;
    if (!this.data) this.body.replaceChildren(Object.assign(document.createElement("p"), { className: "timeline-status", textContent: "正在读取时间线…" }));
    this.inflight = true;
    this.lastRead = Date.now();
    try {
      const update = await this.load(threadId, base?.cursor ?? null);
      if (request !== this.request || threadId !== this.threadId || this.data !== base && base) return;
      this.data = base ? mergeTimeline(base, update) : update;
      this.render();
    } catch (error) {
      if (request !== this.request) return;
      if (!this.data) this.body.replaceChildren(Object.assign(document.createElement("p"), { className: "timeline-status", textContent: `时间线读取失败：${error.message}` }));
    } finally {
      if (request === this.request) {
        this.inflight = false;
        this.schedule();
        // A missed signal (a dropped stream, a subagent on another Node) is
        // caught by a slow read while the conversation runs.
        clearTimeout(this.fallback);
        this.fallback = this.open && this.running ? setTimeout(() => { this.pending = true; this.schedule(); }, 30_000) : null;
      }
    }
  }

  setRunning(running) {
    const was = this.running;
    this.running = running;
    if (!running) { this.setTicker(false); clearTimeout(this.fallback); this.fallback = null; if (this.data) this.render(); }
    if (running && !was) this.notify();
  }

  setZoom(direction) {
    this.zoom = direction === "in" ? Math.min(64, this.zoom * 1.6) : direction === "out" ? Math.max(1, this.zoom / 1.6) : 1;
    this.render();
  }

  render() {
    const data = this.data;
    if (!data) return;
    const now = Date.now(), hour = this.mode === "hour";
    // While running, the recent view is drawn panLeadMs past now and slides
    // left every frame (one transform; see startPan). Live items reach the
    // lead, and a clip at now hides what has not happened yet.
    const panning = hour && this.running;
    const drawnTo = panning ? now + panLeadMs : now;
    const lanes = liveLanes(data.lanes ?? [], drawnTo, this.running);
    const live = lanes.some(lane => [...(lane.turns ?? []), ...(lane.tools ?? [])].some(item => item.live));
    const all = activitySegments(lanes);
    const range = hour && all.length ? hourWindow(lanes, now, this.running, this.windowMs) : null;
    // The hour view is one real-time segment: idle time is shown, not folded.
    const segments = range ? [range] : all;
    const visible = item => !range || item.end >= range.start && item.start <= (panning ? drawnTo : range.end);
    if (!panning || !segments.length) this.stopPan();
    if (!segments.length) this.setTicker(false);
    const recent = `最近 ${formatWindow(this.windowMs)}`;
    this.summary.textContent = all.length ? (hour ? recent : timelineSummary({ ...data, lanes })) : "";
    if (!segments.length) {
      this.body.replaceChildren(Object.assign(document.createElement("p"), { className: "timeline-status", textContent: "还没有可显示的活动。" }));
      return;
    }
    const scroll = this.body.querySelector(".timeline-scroll");
    const previousScroll = scroll?.scrollLeft ?? null;
    const labelsWidth = labelWidth();
    const available = Math.max(200, (this.body.clientWidth || this.root.clientWidth || 720) - labelsWidth - chartPad * 2 - 2);
    const layout = timelineLayout(segments, available, this.zoom);
    // Redraw as often as a growing span gains half a pixel, at most each second.
    this.setTicker(live && !panning, Math.min(30_000, Math.max(1000, 0.5 / (layout.segments.at(-1)?.scale || 1))));
    const ordered = orderLanes(lanes);
    const height = axisHeight + ordered.length * laneHeight + 2;

    const legend = document.createElement("ul");
    legend.className = "timeline-legend";
    const modes = document.createElement("div");
    modes.className = "timeline-modes";
    modes.setAttribute("role", "group");
    modes.setAttribute("aria-label", "时间范围");
    for (const [value, text] of [["all", "全部"], ["hour", recent]]) {
      const button = document.createElement("button");
      button.type = "button";
      button.dataset.timelineMode = value;
      button.textContent = text;
      button.setAttribute("aria-pressed", String(this.mode === value));
      modes.append(button);
    }
    const windowToggle = document.createElement("button");
    windowToggle.type = "button";
    windowToggle.className = "timeline-window-toggle";
    windowToggle.dataset.timelineWindowToggle = "";
    windowToggle.popoverTargetElement = this.windowMenu;
    windowToggle.setAttribute("aria-haspopup", "dialog");
    windowToggle.setAttribute("aria-expanded", String(this.windowMenu.matches(":popover-open")));
    windowToggle.setAttribute("aria-label", "选择最近时间长度");
    windowToggle.title = "选择最近时间长度";
    windowToggle.innerHTML = '<svg viewBox="0 0 10 10" aria-hidden="true"><path d="M2 3.5 5 6.5 8 3.5"></path></svg>';
    modes.append(windowToggle);
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
    if (this.inlineInfo) {
      const inlineDetails = Object.assign(document.createElement("span"), { className: "timeline-details", textContent: details.textContent });
      this.inlineInfo.replaceChildren(inlineDetails, legend.cloneNode(true), ...(data.partial ? [infoChildren.at(-1).cloneNode(true)] : []));
      this.fitInfo();
    }

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
    const panScale = panning ? layout.segments[0].scale : 0;
    const x = panning ? at => chartPad + layout.segments[0].x + (at - range.start) * panScale : at => chartPad + layout.position(at);
    // Row stripes stay put; everything time-based is drawn into content.
    const stripes = svg("g", { class: "timeline-stripes" });
    ordered.forEach((lane, index) => stripes.append(svg("rect", { x: 0, y: axisHeight + index * laneHeight, width: layout.width + chartPad * 2, height: laneHeight,
      class: index % 2 ? "timeline-row odd" : "timeline-row" })));
    chart.append(stripes);
    let content = chart;
    if (panning) {
      const id = `timeline-clip-${++clipCounter}`;
      const defs = svg("defs");
      const clip = svg("clipPath", { id });
      clip.append(svg("rect", { x: 0, y: 0, width: x(now) + 1, height }));
      defs.append(clip);
      const clipped = svg("g", { "clip-path": `url(#${id})` });
      content = svg("g", { class: "timeline-pan" });
      clipped.append(content);
      chart.append(defs, clipped);
    }
    // Axis: date at each segment start, then ticks at a readable step.
    const axis = svg("g", { class: "timeline-axis" });
    let previousDay = null;
    if (panning) {
      // Sliding axis: plain ticks from before the window to the lead; the
      // date joins the first tick of each day.
      const step = tickSteps.find(value => value * panScale >= 72) ?? tickSteps.at(-1);
      axis.append(svg("line", { x1: x(range.start - step), x2: x(drawnTo), y1: axisHeight - 1, y2: axisHeight - 1, class: "timeline-axis-line" }));
      let previous = null;
      for (let at = Math.floor(range.start / step) * step; at <= drawnTo; at += step) {
        const position = x(at);
        axis.append(svg("line", { x1: position, x2: position, y1: axisHeight - 4, y2: axisHeight - 1, class: "timeline-axis-tick" }));
        const label = svg("text", { x: position, y: 10, class: "timeline-axis-text", "text-anchor": "middle" });
        label.textContent = `${day(at) !== previous && previous !== null ? `${day(at)} ` : ""}${clock(at, step < 60e3)}`;
        previous = day(at);
        axis.append(label);
      }
    } else layout.segments.forEach((segment, index) => {
      const step = tickSteps.find(value => value * segment.scale >= 72) ?? tickSteps.at(-1);
      const left = chartPad + segment.x, right = left + segment.width;
      axis.append(svg("line", { x1: left, x2: right, y1: axisHeight - 1, y2: axisHeight - 1, class: "timeline-axis-line" }));
      const startDay = day(segment.start);
      // One row: the bold segment start, then ticks clear of it.
      const startLabel = svg("text", { x: left, y: 10, class: "timeline-axis-start" });
      startLabel.textContent = `${startDay !== previousDay ? `${startDay} ` : ""}${clock(segment.start)}`;
      previousDay = startDay;
      axis.append(startLabel);
      for (let at = Math.ceil(segment.start / step) * step; at <= segment.end; at += step) {
        const position = x(at);
        // Keep tick labels clear of the segment's start label and its break.
        const startWidth = (startDay !== day(layout.segments[index - 1]?.start ?? 0) ? 72 : 40);
        if (position - left < startWidth + 18 || right - position < 22) continue;
        axis.append(svg("line", { x1: position, x2: position, y1: axisHeight - 4, y2: axisHeight - 1, class: "timeline-axis-tick" }));
        const label = svg("text", { x: position, y: 10, class: "timeline-axis-text", "text-anchor": "middle" });
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
        const text = svg("text", { x: right + breakWidth / 2, y: 10, class: "timeline-break-text", "text-anchor": "middle" });
        text.textContent = compactDuration(next.start - segment.end);
        gap.append(text);
        axis.append(gap);
      }
    });
    content.append(axis);
    ordered.forEach((lane, index) => {
      const top = axisHeight + index * laneHeight;
      const row = svg("g", { class: "timeline-lane", "data-thread-id": lane.threadId });
      for (const turn of (lane.turns ?? []).filter(visible)) {
        row.append(svg("rect", { x: x(turn.start), y: top + laneHeight / 2 - 1, width: Math.max(1, x(turn.end) - x(turn.start)), height: 2, class: `timeline-turn${turn.live ? " live" : turn.open ? " open" : ""}` },
          `轮次 · ${clock(turn.start, true)} 起 · ${formatDuration(turn.end - turn.start)}${turn.live ? " · 进行中" : turn.open ? " · 未观测到结束" : turn.status === "aborted" ? " · 已中断" : ""}`));
      }
      const phases = modelPhases(lane).map(([start, end]) => ({ start, end })).filter(visible);
      const liveEnds = new Set((lane.turns ?? []).filter(turn => turn.live).map(turn => turn.end));
      for (const { left, right, kind, items } of drawnSpans(phases, x, { kind: phase => liveEnds.has(phase.end) ? " live" : "" })) {
        const total = items.reduce((sum, phase) => sum + phase.end - phase.start, 0);
        row.append(svg("rect", { x: left, y: top + 3, width: Math.max(1, right - left), height: laneHeight - 6, class: `timeline-span model${kind}` },
          `模型阶段${items.length > 1 ? ` · ${items.length} 段` : ""} · ${formatDuration(total)}${kind ? " · 进行中" : ""}（按工具边界归因）`));
      }
      const toolClass = tool => `${tool.kind === "wait" ? "wait" : "tool"}${tool.live ? " live" : tool.open ? " open" : ""}`;
      for (const { left, right, kind, items } of drawnSpans((lane.tools ?? []).filter(visible), x, { kind: toolClass })) {
        const [tool] = items;
        row.append(svg("rect", { x: left, y: top + 2, width: Math.max(2, right - left), height: laneHeight - 4, class: `timeline-span ${kind}` },
          items.length > 1
            ? `${items.length} 次调用 · ${clock(tool.start, true)} 起 · 共 ${formatDuration(items.reduce((sum, item) => sum + item.end - item.start, 0))}`
            : `${tool.name} · ${clock(tool.start, true)} · ${formatDuration(tool.end - tool.start)}${tool.live ? " · 进行中" : tool.open ? " · 未观测到返回" : ""}`));
      }
      let replyX = -Infinity;
      for (const message of (lane.messages ?? []).filter(message => visible({ start: message.at, end: message.at }))) {
        const cx = x(message.at);
        // Replies on the same pixel share one mark; the user's own stay distinct.
        if (message.role !== "user" && Math.abs(cx - replyX) < 1) continue;
        if (message.role !== "user") replyX = cx;
        if (message.role === "user") {
          const marker = svg("path", { d: `M${cx} ${top + 1} l3.5 5 l-3.5 5 l-3.5 -5 z`, class: "timeline-message user", tabindex: 0, role: "button",
            "data-timeline-seq": message.seq, "data-timeline-thread": lane.threadId, "data-timeline-role": "user",
            "aria-label": `跳到你的消息：${message.preview}` }, `${clock(message.at, true)} · ${message.preview}`);
          row.append(marker);
        } else {
          row.append(svg("line", { x1: cx, x2: cx, y1: top + 2, y2: top + laneHeight - 2, class: "timeline-message assistant",
            "data-timeline-seq": message.seq, "data-timeline-thread": lane.threadId, "data-timeline-role": "assistant" }, `回复 · ${clock(message.at, true)} · ${message.preview}`));
        }
      }
      content.append(row);
    });
    alignPulse(chart);
    scroller.append(chart);
    grid.append(labels, scroller);
    this.body.replaceChildren(grid);
    // Zoomed views follow the growing end unless the reader scrolled away.
    const following = scroll && scroll.scrollLeft + scroll.clientWidth >= scroll.scrollWidth - 4;
    if (previousScroll !== null && !(live && following)) scroller.scrollLeft = previousScroll;
    else if (this.zoom > 1) scroller.scrollLeft = scroller.scrollWidth;
    if (panning) {
      this.pan = { group: content, scale: panScale, base: now, dx: 0 };
      this.startPan();
    }
  }

  // One transform per frame while the recent view runs; it changes only when
  // the shift moves by a tenth of a pixel. Before the drawn lead runs out the
  // view is redrawn from the current time.
  startPan() {
    if (this.panFrame) return;
    const frame = () => {
      this.panFrame = 0;
      const pan = this.pan;
      if (!pan || !this.open) return;
      const elapsed = Date.now() - pan.base;
      // A redraw restarts the loop itself (or ends it).
      if (elapsed > panLeadMs * 0.6) { this.render(); return; }
      const dx = Math.round(elapsed * pan.scale * 10) / 10;
      if (dx !== pan.dx) { pan.dx = dx; pan.group.setAttribute("transform", `translate(${-dx} 0)`); }
      this.panFrame = requestAnimationFrame(frame);
    };
    this.panFrame = requestAnimationFrame(frame);
  }

  stopPan() {
    if (this.panFrame) cancelAnimationFrame(this.panFrame);
    this.panFrame = 0;
    this.pan = null;
  }
}

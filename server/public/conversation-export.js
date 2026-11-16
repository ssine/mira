// Single-file HTML export of one conversation. The export is a presentation
// copy built from the same transcript projections the console shows; it never
// writes back to, or replaces, the authoritative history.
import { activitySummary, formatActivityDuration, summarizeActivities } from "./trace-activity.js";

const escapeMap = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
export const escapeHtml = value => String(value ?? "").replace(/[&<>"']/g, char => escapeMap[char]);

// Only inline data URLs of raster images reach the file; the file loads nothing.
const safeImage = value => typeof value === "string" &&
  /^data:image\/(?:png|jpe?g|webp|gif|bmp|avif);base64,[a-z0-9+/=\r\n]+$/i.test(value) ? value : "";

export function exportFilename(title, date = new Date()) {
  const stem = String(title ?? "").replace(/[\\/:*?"<>|\u0000-\u001f]+/g, " ").replace(/\s+/g, " ").trim().slice(0, 80) || "conversation";
  const pad = n => String(n).padStart(2, "0");
  return `${stem} ${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}.html`;
}

// Tool calls and the thinking between them form one folded group in the
// console. A tool's output images belong to that group as well.
const groupedKind = item => item.kind === "tool" || item.kind === "reasoning" && item.title !== "计划";

export function exportBlocks(items) {
  const blocks = [];
  let group = null, previous = null;
  for (const item of items) {
    const toolImage = item.kind === "image" && previous && (previous.kind === "tool" || previous.toolImage);
    if (groupedKind(item) || toolImage) {
      if (!group || group.turnId !== (item.turnId ?? "")) blocks.push(group = { kind: "group", turnId: item.turnId ?? "", items: [] });
      group.items.push(toolImage ? { ...item, toolImage: true } : item);
      previous = toolImage ? { ...item, toolImage: true } : item;
      continue;
    }
    group = null;
    previous = item;
    blocks.push(item);
  }
  return blocks;
}

// The image items an export shows: a brief export omits tool output images.
export function exportedImages(items, includeTools) {
  return exportBlocks(items).flatMap(block => block.kind === "group" ? includeTools ? block.items : [] : [block])
    .filter(item => item.kind === "image");
}

function groupSummary(items) {
  const tools = items.filter(item => item.kind === "tool");
  if (!tools.length) {
    const thoughts = items.filter(item => item.kind === "reasoning");
    return thoughts.length > 1 ? `思考 · ${thoughts.length} 段` : "思考";
  }
  const activities = tools.map(item => item.activity ?? { actions: [{ kind: "tool", label: item.title || "工具" }] });
  const counts = summarizeActivities(activities);
  return `工具调用 · ${tools.length} 次${counts ? ` · ${counts}` : ""}`;
}

function timestamp(value) {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return "";
  return date.toLocaleString("zh-CN", { hour12: false });
}

function imageFigure(item, images) {
  const url = safeImage(images.get(item.key) ?? item.image?.url);
  return url ? `<figure class="image"><img src="${escapeHtml(url)}" alt="对话中的图片"></figure>`
    : `<p class="muted">[图片无法导出]</p>`;
}

function renderGroup(group, options) {
  const summary = escapeHtml(groupSummary(group.items));
  if (!options.includeTools) {
    const tools = group.items.filter(item => item.kind === "tool").length;
    // A thinking-only group carries no tool call; the brief export omits it.
    return tools ? `<div class="group brief">${summary}</div>` : "";
  }
  const entries = group.items.map(item => {
    if (item.kind === "image") return imageFigure(item, options.images);
    if (item.kind === "reasoning") {
      return `<div class="entry reasoning"><div class="label">思考</div><div class="markdown">${options.markdown(item.body ?? "")}</div></div>`;
    }
    const failed = ["failed", "declined", "interrupted"].includes(item.activity?.status);
    const label = activitySummary(item.activity) || item.title || "工具";
    const duration = formatActivityDuration(item.activity?.durationMs);
    const body = item.body ? `<pre>${escapeHtml(item.body)}</pre>` : `<p class="muted">没有记录工具详情。</p>`;
    return `<details class="entry tool${failed ? " failed" : ""}"><summary><span>${escapeHtml(label)}</span>` +
      `${duration ? `<span class="muted">${escapeHtml(duration)}</span>` : ""}</summary>${body}</details>`;
  }).join("");
  return `<details class="group"><summary>${summary}</summary>${entries}</details>`;
}

function renderBlock(block, options) {
  if (block.kind === "group") return renderGroup(block, options);
  const md = text => options.markdown(text ?? "");
  switch (block.kind) {
    case "user":
      return block.body?.trim() ? `<section class="message user"><div class="bubble markdown">${md(block.body)}</div></section>` : "";
    case "assistant": {
      if (!block.body?.trim()) return "";
      const done = timestamp(block.turnCompletedAt);
      return `<section class="message assistant"><div class="markdown">${md(block.body)}</div>` +
        `${done ? `<footer class="muted">${escapeHtml(done)}</footer>` : ""}</section>`;
    }
    case "image": return `<section class="message">${imageFigure(block, options.images)}</section>`;
    case "reasoning": // A proposed plan stays in the conversation.
      return `<section class="message plan"><div class="label">${escapeHtml(block.title || "计划")}</div><div class="markdown">${md(block.body)}</div></section>`;
    case "compaction": case "recovery": {
      const summary = options.includeTools && block.compactionSummary?.trim()
        ? `<details><summary>压缩摘要</summary><div class="markdown">${md(block.compactionSummary)}</div></details>` : "";
      return `<div class="notice">${escapeHtml(block.body || block.title)}${summary}</div>`;
    }
    case "question":
      return `<section class="message question"><div class="label">${escapeHtml(block.title)}</div><pre class="plain">${escapeHtml(block.body)}</pre></section>`;
    case "error":
      return `<section class="message error"><div class="label">${escapeHtml(block.title || "错误")}</div><pre class="plain">${escapeHtml(block.body)}</pre></section>`;
    default:
      if (!options.includeTools || !block.body?.trim()) return "";
      return `<section class="message other"><div class="label">${escapeHtml(block.title || block.kind)}</div><pre>${escapeHtml(block.body)}</pre></section>`;
  }
}

// markdown(text) must return sanitized HTML. images maps item keys to data URLs.
export function renderConversationHtml({ title, engine, threadId, exportedAt = new Date(), items, includeTools = false,
  markdown, images = new Map() }) {
  const options = { includeTools, markdown, images };
  const body = exportBlocks(items).map(block => renderBlock(block, options)).filter(Boolean).join("\n");
  const name = title || "未命名对话";
  const facts = [engine === "claude" ? "Claude" : "Codex", `导出于 ${timestamp(exportedAt)}`,
    includeTools ? "包含工具调用详情" : "不含工具调用详情"].map(escapeHtml).join(" · ");
  return `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'">
<meta name="generator" content="Mira">
<title>${escapeHtml(name)}</title>
<style>${exportStyles}</style>
</head>
<body>
<main>
<header class="head"><h1>${escapeHtml(name)}</h1><p class="muted">${facts}</p>${threadId ? `<p class="muted mono">${escapeHtml(threadId)}</p>` : ""}</header>
${body || `<p class="muted">此会话还没有可显示的消息。</p>`}
</main>
</body>
</html>
`;
}

const exportStyles = `
:root { color-scheme: light dark; --canvas: #f7f8fa; --surface: #fff; --text: #1d2733; --muted: #687587; --line: #dde2e8;
  --accent: #2f6fdf; --user: #eaf1fd; --danger: #c33b3b; --code: #f1f3f6;
  --font: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace; }
@media (prefers-color-scheme: dark) { :root { --canvas: #15191e; --surface: #1c2128; --text: #e3e8ee; --muted: #8d99a8;
  --line: #2f3742; --accent: #79a6f2; --user: #22324a; --danger: #ef7d7d; --code: #232a33; } }
* { box-sizing: border-box; }
body { margin: 0; background: var(--canvas); color: var(--text); font: 15px/1.7 var(--font); }
main { max-width: 808px; margin: 0 auto; padding: 32px 20px 64px; }
.head { margin-bottom: 28px; padding-bottom: 16px; border-bottom: 1px solid var(--line); }
h1 { margin: 0 0 6px; font-size: 22px; line-height: 1.35; }
.head p { margin: 2px 0; }
.muted { color: var(--muted); font-size: 12px; }
.mono { font-family: var(--mono); }
.message { margin: 18px 0; }
.user { display: flex; justify-content: flex-end; }
.bubble { max-width: 85%; padding: 8px 14px; border-radius: 12px; background: var(--user); }
.assistant footer { margin-top: 4px; }
.label { margin-bottom: 4px; font-size: 12px; font-weight: 600; color: var(--muted); }
.error .label, .error pre { color: var(--danger); }
.question, .plan, .other { padding: 10px 14px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); }
.notice { margin: 18px 0; padding: 6px 0; text-align: center; color: var(--muted); font-size: 12px; border-top: 1px dashed var(--line); border-bottom: 1px dashed var(--line); }
.notice details { text-align: left; }
.group { margin: 10px 0; padding: 6px 12px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); font-size: 13px; }
.group.brief { color: var(--muted); }
.group > summary { color: var(--muted); cursor: pointer; }
.entry { margin: 8px 0; border-top: 1px solid var(--line); padding-top: 6px; }
.entry.reasoning .label { margin: 0; }
.entry > summary { display: flex; gap: 10px; justify-content: space-between; cursor: pointer; }
.entry.failed > summary span:first-child { color: var(--danger); }
summary { overflow-wrap: anywhere; }
pre { margin: 6px 0; padding: 10px 12px; overflow-x: auto; border-radius: 6px; background: var(--code); font: 12px/1.5 var(--mono); white-space: pre-wrap; overflow-wrap: anywhere; }
pre.plain { padding: 0; background: none; font: inherit; }
.markdown > :first-child { margin-top: 0; } .markdown > :last-child { margin-bottom: 0; }
.markdown code { padding: 1px 4px; border-radius: 4px; background: var(--code); font: 0.9em var(--mono); }
.markdown pre code { padding: 0; background: none; }
.markdown blockquote { margin: 8px 0; padding-left: 12px; border-left: 3px solid var(--line); color: var(--muted); }
.markdown table { display: block; max-width: 100%; overflow-x: auto; border-collapse: collapse; }
.markdown th, .markdown td { padding: 4px 10px; border: 1px solid var(--line); }
.markdown a { color: var(--accent); }
.markdown img, figure img { max-width: 100%; height: auto; }
figure { margin: 8px 0; }
figure img { border: 1px solid var(--line); border-radius: 6px; }
.diagram { overflow-x: auto; }
.diagram img { border: 0; }
.diagram-dark { display: none; }
@media (prefers-color-scheme: dark) { .diagram-light { display: none; } .diagram-dark { display: inline; } }
.diagram summary { color: var(--muted); font-size: 12px; cursor: pointer; }
.file-ref { font-family: var(--mono); font-size: 0.9em; }
@media print { body { background: #fff; } details > summary { list-style: none; } .diagram-light { display: inline; } .diagram-dark { display: none; } }
`;

// Reads every page of a Codex transcript, oldest last. Tool bodies are only
// requested when they will be exported.
export async function collectCodexTranscript(api, threadId, { includeTools, merge, signal, onProgress = () => {} }) {
  const read = async () => {
    let items = [], cursor = null, pages = 0, generation = null;
    do {
      const query = new URLSearchParams({ storeId: "personal", tail: "1", toolDetails: includeTools ? "1" : "0", limit: "200" });
      if (cursor) query.set("cursor", cursor);
      const page = await api(`/v1/codex/threads/${encodeURIComponent(threadId)}/transcript?${query}`, { signal });
      generation ??= page.generation;
      if (page.generation !== generation) throw Object.assign(new Error("会话历史已更新"), { code: "stale_transcript_cursor" });
      items = merge(page.trace ?? [], items);
      if (page.nextCursor === cursor) break; // No progress: never spin.
      cursor = page.nextCursor ?? null;
      onProgress(++pages);
    } while (cursor);
    return items;
  };
  // A running conversation can advance between pages; restart once from its latest state.
  try { return await read(); }
  catch (error) {
    if (error.code !== "stale_transcript_cursor" || signal?.aborted) throw error;
    return read();
  }
}

// Resolves Codex image references into data URLs, two at a time.
export async function collectImages(api, items, { signal, onProgress = () => {} } = {}) {
  const images = new Map();
  const pending = items.filter(item => item.kind === "image" && !safeImage(item.image?.url) && item.image?.href);
  let next = 0, done = 0;
  const worker = async () => {
    while (next < pending.length) {
      const item = pending[next++];
      signal?.throwIfAborted();
      try {
        const url = safeImage((await api(item.image.href, { signal })).url);
        if (url) images.set(item.key, url);
      } catch (error) { if (signal?.aborted) throw error; }
      onProgress(++done, pending.length);
    }
  };
  await Promise.all([worker(), worker()]);
  return images;
}

// Browser Markdown renderer for the exported file: sanitized, with local paths
// shown as text, since the file cannot open Node files. Mermaid blocks become
// placeholders that embedDiagrams later replaces with rendered images.
export function exportMarkdownRenderer(marked, DOMPurify) {
  const diagrams = new Map(), token = `mira-diagram-${crypto.randomUUID()}`;
  const render = text => {
    const template = document.createElement("template");
    template.innerHTML = DOMPurify.sanitize(marked.parse(String(text ?? "")));
    for (const link of template.content.querySelectorAll("a[href]")) {
      if (/^(?:https?:|mailto:|#)/i.test(link.getAttribute("href"))) {
        link.setAttribute("rel", "noopener noreferrer");
        continue;
      }
      const span = document.createElement("span");
      span.className = "file-ref";
      span.textContent = link.textContent;
      span.title = link.getAttribute("href");
      link.replaceWith(span);
    }
    for (const image of template.content.querySelectorAll("img")) {
      if (safeImage(image.getAttribute("src"))) continue;
      image.replaceWith(document.createTextNode(image.getAttribute("alt") ? `[图片：${image.getAttribute("alt")}]` : "[图片]"));
    }
    for (const code of template.content.querySelectorAll("pre > code.language-mermaid, pre > code.language-flowchart")) {
      const source = code.textContent;
      if (!diagrams.has(source)) diagrams.set(source, diagrams.size);
      code.parentElement.before(document.createComment(`${token}:${diagrams.get(source)}`));
    }
    return template.innerHTML;
  };
  return Object.assign(render, { diagrams, token });
}

// Mermaid colors matching the exported file's light and dark tokens.
export const diagramThemes = {
  light: { darkMode: false, background: "#ffffff", primaryColor: "#eaf1fd", primaryTextColor: "#1d2733", primaryBorderColor: "#2f6fdf",
    lineColor: "#687587", secondaryColor: "#f7f8fa", tertiaryColor: "#f1f3f6", edgeLabelBackground: "#ffffff" },
  dark: { darkMode: true, background: "#1c2128", primaryColor: "#22324a", primaryTextColor: "#e3e8ee", primaryBorderColor: "#79a6f2",
    lineColor: "#8d99a8", secondaryColor: "#15191e", tertiaryColor: "#232a33", edgeLabelBackground: "#1c2128" },
};

const safeDiagram = value => typeof value === "string" && /^data:image\/svg\+xml;charset=utf-8,[^\s"<>]+$/.test(value) ? value : "";

// Replaces diagram placeholders with light and dark SVG images, one diagram at
// a time. A diagram that cannot render keeps its source code block visible.
export async function embedDiagrams(html, { diagrams, token }, render, { signal, onProgress = () => {} } = {}) {
  const images = new Map();
  let done = 0;
  for (const [source, index] of diagrams) {
    signal?.throwIfAborted();
    try {
      const light = safeDiagram(await render(source, diagramThemes.light));
      const dark = safeDiagram(await render(source, diagramThemes.dark));
      if (light && dark) images.set(index, { light, dark });
    } catch (error) { if (signal?.aborted) throw error; }
    onProgress(++done, diagrams.size);
  }
  return html.replace(new RegExp(`<!--${token}:(\\d+)-->(<pre>[\\s\\S]*?</pre>)`, "g"), (match, index, code) => {
    const image = images.get(Number(index));
    if (!image) return code;
    return `<figure class="diagram"><img class="diagram-light" src="${image.light}" alt="Mermaid 图表">` +
      `<img class="diagram-dark" src="${image.dark}" alt="Mermaid 图表"><details><summary>查看图表源码</summary>${code}</details></figure>`;
  });
}

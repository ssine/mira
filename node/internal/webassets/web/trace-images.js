// Presentation only: retain the original image payload in the canonical history.
export function imagePath(value) {
  if (typeof value !== "string") return "";
  if (/^file:/i.test(value)) {
    try {
      const url = new URL(value);
      if (url.search || url.hash || url.username || url.password) return "";
      const path = decodeURIComponent(url.pathname);
      if (url.hostname && url.hostname !== "localhost") return `\\\\${url.hostname}${path.replaceAll("/", "\\")}`;
      return /^\/[A-Za-z]:\//.test(path) ? path.slice(1) : path;
    } catch { return ""; }
  }
  return /^(?:\/|[A-Za-z]:[\\/]|\\\\)/.test(value) ? value : "";
}

export function imageDataUrl(value) {
  if (typeof value !== "string") return null;
  return /^data:(?:image\/(?:png|jpe?g|webp|gif|bmp|avif|tiff)|application\/octet-stream);base64,[a-z0-9+/=\r\n]+$/i.test(value) ? value : null;
}

async function imageBlob(url, signal) {
  // fetch(data:) would violate the console's same-origin connect-src policy.
  const comma = url.indexOf(",");
  const encoded = url.slice(comma + 1).replace(/[\r\n]/g, "");
  const chunks = [];
  for (let offset = 0; offset < encoded.length; offset += 64 * 1024) {
    signal.throwIfAborted();
    chunks.push(Uint8Array.from(atob(encoded.slice(offset, offset + 64 * 1024)), (char) => char.charCodeAt(0)));
    if (offset % (256 * 1024) === 0) await new Promise((resolve) => setTimeout(resolve, 0));
  }
  return new Blob(chunks, { type: url.slice(5, url.indexOf(";")) });
}

export function outputImages(value, depth = 0) {
  if (depth > 8 || value == null) return [];
  if (typeof value === "string") {
    if (!/^[\[{]/.test(value.trim())) return [];
    try { return outputImages(JSON.parse(value), depth + 1); } catch { return []; }
  }
  if (Array.isArray(value)) return value.flatMap((part) => outputImages(part, depth + 1));
  if (typeof value !== "object") return [];
  const url = imageDataUrl(value.image_url ?? value.imageUrl) ??
    (value.type === "image" && typeof value.data === "string" ? imageDataUrl(`data:${value.mimeType};base64,${value.data}`) : null);
  if (url) return [{ url }];
  for (const key of ["content", "contentItems", "content_items", "output", "body", "result"]) {
    if (value[key] != null) return outputImages(value[key], depth + 1);
  }
  return [];
}

export function mergeImages(previous = [], next = []) {
  if (!previous.length) return next;
  if (!next.length) return previous;
  // A native imageView path and its returned snapshot describe the same image.
  if (previous.length === 1 && next.length === 1 && previous[0].path && next[0].url) return [{ ...previous[0], ...next[0] }];
  if (previous.length === 1 && next.length === 1 && previous[0].url && next[0].path) return [{ ...next[0], ...previous[0] }];
  return [...previous, ...next].filter((image, index, all) =>
    all.findIndex((other) => image.url ? other.url === image.url : other.path === image.path) === index);
}

export function imageJsonReplacer(key, value) {
  if (imageDataUrl(value)) return "[图片单独显示]";
  if (typeof value === "string" && ["result", "output", "content"].includes(key) && /^[\[{]/.test(value.trim())) {
    try { return JSON.parse(value); } catch { /* retain plain tool text */ }
  }
  if (value && typeof value === "object" && value.type === "image" && typeof value.data === "string") {
    return { ...value, data: "[图片单独显示]" };
  }
  return value;
}

// Images outside collapsed tool groups load as they approach the viewport.
// Bound simultaneous Node reads and release blobs when a conversation is removed.
export class TraceImages {
  constructor(root, readFile, followImage, preview) {
    this.root = root;
    this.readFile = readFile;
    this.followImage = followImage;
    this.preview = preview;
    this.entries = new Map();
    this.queue = [];
    this.running = 0;
    this.observer = new IntersectionObserver((entries) => {
      for (const entry of entries) if (entry.isIntersecting) {
        this.observer.unobserve(entry.target);
        this.enqueue(this.entries.get(entry.target));
      }
    }, { root: root.closest("#conversationScroll"), rootMargin: "300px" });
    this.cleanup = new MutationObserver(() => {
      for (const [body, entry] of this.entries) if (!root.contains(body)) this.remove(body, entry);
    });
    this.cleanup.observe(root, { childList: true, subtree: true });
  }

  remove(body, entry) {
    entry.controller.abort();
    this.observer.unobserve(body);
    if (entry.objectUrl) URL.revokeObjectURL(entry.objectUrl);
    this.entries.delete(body);
  }

  mount(body, source, nodeIds) {
    const previous = this.entries.get(body);
    if (previous) {
      // A late path-only notification must not downgrade a saved snapshot.
      const unchanged = source.url ? previous.source.url === source.url : previous.source.path === source.path;
      source = { ...source, path: source.path || previous.source.path };
      if (unchanged) {
        previous.source = { ...previous.source, ...source, url: source.url || previous.source.url };
        this.label(previous);
        return;
      }
      previous.controller.abort();
      const entry = { ...previous, source, nodeIds: [...nodeIds], controller: new AbortController(), pending: false };
      this.entries.set(body, entry);
      this.label(entry);
      this.observer.observe(body);
      return;
    }
    const figure = document.createElement("figure");
    const link = document.createElement("button");
    link.type = "button";
    link.className = "trace-image-preview";
    link.disabled = true;
    link.title = "放大预览";
    link.setAttribute("aria-haspopup", "dialog");
    const img = document.createElement("img");
    img.decoding = "async";
    img.hidden = true;
    link.append(img);
    const caption = document.createElement("figcaption");
    const status = document.createElement("span");
    status.className = "trace-image-status";
    status.textContent = "正在加载图片…";
    const retry = document.createElement("button");
    retry.type = "button";
    retry.className = "trace-image-retry";
    retry.textContent = "重新加载";
    retry.hidden = true;
    figure.append(link, status, retry, caption);
    body.replaceChildren(figure);
    const entry = { source, nodeIds: [...nodeIds], body, img, link, caption, status, retry, controller: new AbortController() };
    this.entries.set(body, entry);
    this.label(entry);
    link.addEventListener("click", () => {
      const current = this.entries.get(body);
      if (current?.blob) this.preview(current.blob, current.source.path);
    });
    retry.addEventListener("click", () => this.enqueue(this.entries.get(body)));
    this.observer.observe(body);
  }

  label(entry) {
    const label = entry.source.path?.split(/[\\/]/).at(-1) || "工具返回的图片";
    entry.img.alt = label;
    entry.link.setAttribute("aria-label", `预览 ${label}`);
    entry.caption.textContent = entry.source.path || label;
    entry.caption.title = entry.source.path || label;
  }

  enqueue(entry) {
    if (!entry || entry.controller.signal.aborted || entry.pending) return;
    entry.pending = true;
    this.queue.push(entry);
    this.drain();
  }

  drain() {
    while (this.running < 2 && this.queue.length) {
      const entry = this.queue.shift();
      if (entry.controller.signal.aborted) continue;
      this.running++;
      this.load(entry).finally(() => { entry.pending = false; this.running--; this.drain(); });
    }
  }

  async load(entry) {
    const { source, controller, img, status, retry, link } = entry;
    retry.hidden = true;
    status.hidden = Boolean(entry.blob);
    status.textContent = "正在加载图片…";
    let objectUrl;
    try {
      const url = imageDataUrl(source.url);
      const blob = url ? await imageBlob(url, controller.signal)
        : await this.readFile(source.path, entry.nodeIds, controller, (text) => { status.textContent = text; });
      controller.signal.throwIfAborted();
      objectUrl = URL.createObjectURL(blob);
      const nextImage = img.cloneNode(false);
      nextImage.src = objectUrl;
      await nextImage.decode();
      controller.signal.throwIfAborted();
      const follow = this.followImage();
      // Decode off-DOM and commit in one step: keep the visible image while a
      // path preview is upgraded to its immutable snapshot.
      nextImage.alt = entry.img.alt;
      nextImage.hidden = false;
      entry.img.replaceWith(nextImage);
      entry.img = nextImage;
      if (entry.objectUrl) URL.revokeObjectURL(entry.objectUrl);
      entry.objectUrl = objectUrl;
      objectUrl = null;
      entry.blob = blob;
      link.disabled = false;
      status.hidden = true;
      follow();
    } catch (error) {
      if (controller.signal.aborted) return;
      img.hidden = true;
      link.disabled = true;
      entry.blob = null;
      if (entry.objectUrl) { URL.revokeObjectURL(entry.objectUrl); entry.objectUrl = null; }
      status.textContent = `图片暂时无法显示：${error.message}`;
      status.hidden = false;
      retry.hidden = false;
    } finally {
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    }
  }
}

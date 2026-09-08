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
// Bound simultaneous history reads and release blobs when a conversation is removed.
export class TraceImages {
  constructor(root, readHistory, followImage, preview) {
    this.root = root;
    this.readHistory = readHistory;
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

  mount(body, source) {
    const previous = this.entries.get(body);
    if (previous?.source.href === source.href && previous?.source.url === source.url) return;
    if (previous) this.remove(body, previous);
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
    const entry = { source, body, img, link, caption, status, retry, controller: new AbortController() };
    this.entries.set(body, entry);
    this.label(entry);
    link.addEventListener("click", () => {
      const current = this.entries.get(body);
      if (current?.blob) this.preview(current.blob);
    });
    retry.addEventListener("click", () => this.enqueue(this.entries.get(body)));
    this.observer.observe(body);
  }

  label(entry) {
    const label = "对话中的图片";
    entry.img.alt = label;
    entry.link.setAttribute("aria-label", `预览 ${label}`);
    entry.caption.textContent = label;
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
      const url = imageDataUrl(source.url ?? (await this.readHistory(source.href, controller.signal)).url);
      if (!url) throw new Error("历史记录没有可显示的图片数据");
      const blob = await imageBlob(url, controller.signal);
      controller.signal.throwIfAborted();
      objectUrl = URL.createObjectURL(blob);
      const nextImage = img.cloneNode(false);
      nextImage.src = objectUrl;
      await nextImage.decode();
      controller.signal.throwIfAborted();
      const follow = this.followImage();
      // Commit only after decoding; request completion order never moves cards.
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

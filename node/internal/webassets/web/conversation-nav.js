// A vertical rail of the user's own messages in the open conversation. Each
// tick previews its message on hover/focus and scrolls the transcript to it;
// pressing and dragging along the rail scrubs through messages like a
// scrollbar.
// The rail reads only the transcript already held by the browser; with a full
// outline that is the whole conversation.

const previewLength = 160;
const flashMs = 1600;

export function messagePreview(text, limit = previewLength) {
  const line = String(text ?? "").replace(/\s+/g, " ").trim();
  return line.length > limit ? `${line.slice(0, limit)}…` : line;
}

// Scroll a transcript card to the top of the reading area, correcting once
// after lazily rendered neighbours settle their measured heights.
export function revealCard(card, scroller, { flash = true, offset = 12 } = {}) {
  if (!card || !scroller) return false;
  const place = () => {
    if (!card.isConnected) return;
    scroller.scrollTop += card.getBoundingClientRect().top - scroller.getBoundingClientRect().top - offset;
  };
  place();
  requestAnimationFrame(() => requestAnimationFrame(place));
  if (flash) {
    card.classList.remove("trace-card-flash");
    void card.offsetWidth; // Restart the highlight when jumping to the same card twice.
    card.classList.add("trace-card-flash");
    setTimeout(() => card.classList.remove("trace-card-flash"), flashMs);
  }
  return true;
}

export class MessageRail {
  constructor({ rail, scroller, resolveCard, readingOffset = () => 12 }) {
    this.rail = rail;
    this.scroller = scroller;
    this.resolveCard = resolveCard;
    this.readingOffset = readingOffset;
    this.entries = [];
    this.signature = "";
    this.active = -1;
    this.frame = null;
    this.list = document.createElement("ol");
    this.list.className = "message-rail-list";
    this.tip = document.createElement("div");
    this.tip.className = "message-rail-tip";
    this.tip.setAttribute("role", "tooltip");
    this.tip.id = `${rail.id || "message-rail"}-tip`;
    this.tip.hidden = true;
    rail.replaceChildren(this.list, this.tip);
    rail.hidden = true;
    scroller.addEventListener("scroll", () => this.schedule(), { passive: true });
    window.addEventListener("resize", () => this.schedule(), { passive: true });
    // Pointer presses are handled by the scrub below; click covers keyboard
    // activation (detail 0) and synthetic clicks.
    rail.addEventListener("click", event => {
      const tick = event.target.closest("[data-rail-index]");
      if (tick && event.detail === 0) this.jump(Number(tick.dataset.railIndex));
    });
    this.dragging = null;
    this.list.addEventListener("pointerdown", event => {
      if (event.button !== 0 || !this.entries.length) return;
      event.preventDefault();
      this.list.setPointerCapture?.(event.pointerId);
      this.dragging = { pointerId: event.pointerId, index: -1 };
      this.rail.dataset.scrubbing = "true";
      this.scrubTo(event.clientY);
    });
    this.list.addEventListener("pointermove", event => {
      if (this.dragging?.pointerId === event.pointerId) this.scrubTo(event.clientY);
    });
    const finish = event => {
      if (this.dragging?.pointerId !== event.pointerId) return;
      const { index } = this.dragging;
      this.dragging = null;
      delete this.rail.dataset.scrubbing;
      this.list.releasePointerCapture?.(event.pointerId);
      if (event.type === "pointerup" && index >= 0) this.jump(index);
      this.preview(null);
    };
    this.list.addEventListener("pointerup", finish);
    this.list.addEventListener("pointercancel", finish);
    rail.addEventListener("pointerover", event => { if (!this.dragging) this.preview(event.target.closest("[data-rail-index]")); });
    rail.addEventListener("focusin", event => this.preview(event.target.closest("[data-rail-index]")));
    rail.addEventListener("pointerleave", () => { if (!this.dragging) this.preview(null); });
    rail.addEventListener("focusout", event => { if (!rail.contains(event.relatedTarget)) this.preview(null); });
    rail.addEventListener("keydown", event => {
      if (!["ArrowUp", "ArrowDown", "Home", "End"].includes(event.key)) return;
      const ticks = [...this.list.querySelectorAll("button")];
      const current = ticks.indexOf(document.activeElement);
      const next = event.key === "Home" ? 0 : event.key === "End" ? ticks.length - 1
        : Math.max(0, Math.min(ticks.length - 1, current + (event.key === "ArrowUp" ? -1 : 1)));
      ticks[next]?.focus();
      event.preventDefault();
    });
  }

  // entries: [{ key, body }] in conversation order.
  update(entries) {
    const signature = JSON.stringify(entries.map(entry => [entry.key, entry.body?.length ?? 0]));
    if (signature === this.signature) { this.schedule(); return; }
    this.signature = signature;
    this.entries = entries;
    this.active = -1;
    this.rail.hidden = entries.length < 2;
    const ticks = entries.map((entry, index) => {
      const item = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.className = "message-rail-tick";
      button.dataset.railIndex = String(index);
      button.setAttribute("aria-label", `第 ${index + 1} 条消息：${messagePreview(entry.body, 60)}`);
      button.setAttribute("aria-describedby", this.tip.id);
      item.append(button);
      return item;
    });
    this.list.replaceChildren(...ticks);
    this.schedule();
  }

  clear() { this.update([]); }

  schedule() {
    if (this.frame !== null || this.rail.hidden) return;
    this.frame = requestAnimationFrame(() => { this.frame = null; this.sync(); });
  }

  // The active message is the last one whose card starts at or above the top
  // of the reading area (so a jumped-to message is the active one); at the
  // end of the conversation, the last one in view.
  // Cards are in document order, so binary search suffices.
  sync() {
    if (!this.entries.length) return;
    const viewport = this.scroller.getBoundingClientRect();
    const top = viewport.top + this.readingOffset();
    const atEnd = this.scroller.scrollHeight - this.scroller.clientHeight - this.scroller.scrollTop <= 2;
    const line = atEnd ? viewport.bottom - 24 : top + 48;
    let low = 0, high = this.entries.length - 1, active = -1;
    while (low <= high) {
      const middle = (low + high) >> 1;
      const card = this.resolveCard(this.entries[middle].key, false);
      if (!card) { high = middle - 1; continue; }
      if (card.getBoundingClientRect().top <= line) { active = middle; low = middle + 1; }
      else high = middle - 1;
    }
    if (active === this.active) return;
    this.list.querySelector('[aria-current="true"]')?.removeAttribute("aria-current");
    this.active = active;
    const tick = active >= 0 ? this.list.querySelector(`[data-rail-index="${active}"]`) : null;
    if (!tick) return;
    tick.setAttribute("aria-current", "true");
    // Long conversations scroll the rail itself; keep the active tick visible.
    const rail = this.list.getBoundingClientRect(), mark = tick.getBoundingClientRect();
    if (mark.top < rail.top || mark.bottom > rail.bottom) this.list.scrollTop += mark.top - rail.top - rail.height / 2;
  }

  preview(tick) {
    if (!tick) { this.tip.hidden = true; return; }
    const entry = this.entries[Number(tick.dataset.railIndex)];
    if (!entry) return;
    this.tip.textContent = messagePreview(entry.body);
    this.tip.hidden = false;
    const rail = this.rail.getBoundingClientRect(), mark = tick.getBoundingClientRect();
    const top = Math.max(8, Math.min(mark.top + mark.height / 2 - rail.top - this.tip.offsetHeight / 2, rail.height - this.tip.offsetHeight - 8));
    this.tip.style.top = `${top}px`;
  }

  // The tick nearest to a pointer position, also above or below the ticks.
  indexAt(clientY) {
    const ticks = this.list.querySelectorAll(".message-rail-tick");
    if (!ticks.length) return -1;
    let low = 0, high = ticks.length - 1;
    while (low < high) {
      const middle = (low + high) >> 1;
      if (ticks[middle].getBoundingClientRect().bottom < clientY) low = middle + 1;
      else high = middle;
    }
    return low;
  }

  scrubTo(clientY) {
    const index = this.indexAt(clientY);
    if (index < 0 || index === this.dragging.index) return;
    this.dragging.index = index;
    const tick = this.list.querySelector(`[data-rail-index="${index}"]`);
    // Follow the pointer when the rail itself scrolls (very long conversations).
    const list = this.list.getBoundingClientRect();
    if (clientY < list.top + 12) this.list.scrollTop -= 16;
    else if (clientY > list.bottom - 12) this.list.scrollTop += 16;
    this.preview(tick);
    this.jump(index, { flash: false });
  }

  jump(index, { flash = true } = {}) {
    const entry = this.entries[index];
    if (!entry) return;
    revealCard(this.resolveCard(entry.key, true), this.scroller, { offset: this.readingOffset(), flash });
    this.schedule();
  }
}

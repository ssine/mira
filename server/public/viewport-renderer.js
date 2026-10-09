// Keep expensive content within the reading viewport and half a screen on
// either side. Offscreen rows retain their measured height and source data.
export class ViewportRenderer {
  constructor(root) {
    this.root = root;
    this.rows = new Map();
    this.resize = new ResizeObserver(() => this.connect());
    this.resize.observe(root);
    this.connect();
  }

  connect() {
    const margin = Math.ceil(this.root.clientHeight / 2);
    if (margin === this.margin && this.observer) return;
    this.margin = margin;
    this.observer?.disconnect();
    this.observer = new IntersectionObserver(entries => {
      // Capture intent before changing any row. Measured placeholders preserve
      // readers above the tail; the latest reply continues following the bottom.
      const follow = this.root.scrollHeight - this.root.clientHeight - this.root.scrollTop <= 96;
      const top = this.root.scrollTop;
      const edge = this.root.getBoundingClientRect().top;
      let anchor;
      for (const [element, row] of this.rows) {
        if (!element.isConnected) continue;
        const rect = element.getBoundingClientRect();
        if (rect.bottom > edge && rect.top <= edge && rect.height) { anchor = { element, top: rect.top }; break; }
      }
      // Mounting/removing nested rows can coalesce observer notifications.
      // Recheck existing visible rows as well as notified rows, using current
      // geometry so an old notification cannot leave content far offscreen.
      const targets = new Set(entries.map(entry => entry.target));
      for (const [element, row] of this.rows) if (row.visible) targets.add(element);
      for (const element of targets) {
        const row = this.rows.get(element);
        if (!row || !element.isConnected) continue;
        const rect = element.getBoundingClientRect();
        const visible = rect.height > 0 && rect.bottom > edge - this.margin &&
          rect.top < edge + this.root.clientHeight + this.margin;
        if (row.visible === visible) continue;
        row.visible = visible;
        if (visible) row.show(); else row.hide();
      }
      if (follow) this.root.scrollTop = this.root.scrollHeight;
      else this.root.scrollTop = anchor?.element.isConnected
        ? top + anchor.element.getBoundingClientRect().top - anchor.top : top;
    }, { root: this.root, rootMargin: `${margin}px 0px` });
    for (const element of this.rows.keys()) this.observer.observe(element);
  }

  watch(element, show, hide) {
    if (this.rows.has(element)) return;
    this.rows.set(element, { show, hide, visible: null });
    this.observer.observe(element);
  }

  forget(element) {
    const row = this.rows.get(element);
    if (row?.visible) row.hide();
    this.rows.delete(element);
    this.observer.unobserve(element);
  }

  clear() {
    this.observer.disconnect();
    this.rows.clear();
  }
}

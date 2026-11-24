import { conversationScroll } from "./conversation-scroll.js";

// Keep expensive content within the reading viewport and half a screen on
// either side. Offscreen rows retain their measured height and source data.
//
// What is near follows from current geometry alone: rows are in page order, so
// a binary search finds them without visiting the rest of the conversation.
// There is no remembered observer state to disagree with, and therefore no
// notification whose absence could leave a message empty on screen.
const showMargin = 0.5, keepMargin = 1; // In viewport heights; the gap avoids flapping at the edge.

export class ViewportRenderer {
  constructor(root, content = root.firstElementChild) {
    this.root = root;
    this.content = content;
    this.scroll = conversationScroll(root);
    this.rows = new Map();
    this.shown = new Set();
    this.frame = null;
    // Cached so that preparing a row never forces a layout to learn it.
    this.width = root.clientWidth;
    // Scroll callbacks and frame callbacks run before that frame paints, so a
    // jump never shows empty rows. Content can also move by itself (an image or
    // diagram finished, an opened detail, a streamed reply).
    this.resize = new ResizeObserver(() => { this.width = root.clientWidth; this.schedule(); });
    this.resize.observe(root);
    if (content) this.resize.observe(content);
    root.addEventListener("scroll", () => this.schedule(), { passive: true });
  }

  schedule() {
    this.frame ??= requestAnimationFrame(() => { this.frame = null; this.settle(); });
  }

  watch(element, show, hide, { measure = () => 0, shown = false } = {}) {
    if (this.rows.has(element)) return;
    this.rows.set(element, { show, hide, measure, shown });
    if (shown) this.shown.add(element);
    this.schedule();
  }

  forget(element) {
    const row = this.rows.get(element);
    if (!row) return;
    this.rows.delete(element);
    this.shown.delete(element);
    if (row.shown) row.hide(element.isConnected ? row.measure() : 0);
  }

  clear() {
    this.rows.clear();
    this.shown.clear();
  }

  #range(margin) {
    const top = this.root.getBoundingClientRect().top, height = this.root.clientHeight;
    return { top: top - height * margin, bottom: top + height * (1 + margin) };
  }

  // Index of the first child of a vertical list that ends below `line`.
  #firstBelow(list, line) {
    const children = list.children;
    let low = 0, high = children.length;
    while (low < high) {
      const middle = (low + high) >> 1;
      // Undisplayed children have no position; decide from the next one that has.
      let probe = middle;
      while (probe < high && !children[probe].getClientRects().length) probe++;
      if (probe === high) { high = middle; continue; }
      if (children[probe].getBoundingClientRect().bottom > line) high = middle;
      else low = probe + 1;
    }
    return low;
  }

  // The vertical list inside a row: older rows are bundled in sections, and an
  // open tool group lists its entries.
  #nested(element) {
    if (element.matches(".trace-section")) return element;
    return element.matches("details[open]") ? element.lastElementChild : null;
  }

  // Watched rows near the viewport, in page order.
  #nearby() {
    const range = this.#range(showMargin), found = [];
    const visit = list => {
      for (let child = list.children[this.#firstBelow(list, range.top)]; child; child = child.nextElementSibling) {
        if (!child.getClientRects().length) continue;
        if (child.getBoundingClientRect().top >= range.bottom) break;
        if (this.rows.has(child)) found.push(child);
        const nested = this.#nested(child);
        if (nested) visit(nested);
        else if (this.rows.has(child.firstElementChild)) found.push(child.firstElementChild);
      }
    };
    if (this.content) visit(this.content);
    return found;
  }

  // Show what is near and release what is far away. Showing a row can move its
  // neighbours, so repeat until nothing new comes into range.
  settle() {
    if (!this.root.clientHeight) return;
    for (let pass = 0; pass < 6; pass++) {
      const near = this.#nearby(), show = near.filter(element => !this.rows.get(element).shown);
      const hide = [];
      if (pass === 0) {
        const keep = this.#range(keepMargin), wanted = new Set(near);
        for (const element of this.shown) {
          if (wanted.has(element)) continue;
          if (!element.isConnected) { this.forget(element); continue; }
          const rect = element.getBoundingClientRect();
          if (!rect.height || rect.bottom <= keep.top || rect.top >= keep.bottom) hide.push(element);
        }
      }
      if (!show.length && !hide.length) return;
      this.#apply(show, hide);
    }
  }

  // The topmost visible row, to keep it in place while others change height.
  anchor() {
    const edge = this.root.getBoundingClientRect().top;
    let list = this.content, element = null;
    while (list) {
      let child = list.children[this.#firstBelow(list, edge)];
      while (child?.dataset.scrollAnchor === "none") child = child.nextElementSibling;
      if (!child) break;
      element = child;
      list = this.#nested(child);
    }
    return element && { element, top: element.getBoundingClientRect().top };
  }

  #apply(show, hide) {
    // All reads happen before any write: measuring between writes would lay
    // the conversation out again for every row.
    const follow = this.scroll.nearBottom();
    const anchor = follow ? null : this.anchor();
    const measured = hide.map(element => this.rows.get(element).measure());
    hide.forEach((element, index) => {
      const row = this.rows.get(element);
      if (!row) return; // Released together with the row that contains it.
      row.shown = false;
      this.shown.delete(element);
      row.hide(measured[index]);
    });
    for (const element of show) {
      const row = this.rows.get(element);
      if (!row || row.shown) continue;
      row.shown = true;
      this.shown.add(element);
      row.show();
    }
    if (follow) this.scroll.toBottom();
    else if (anchor?.element.isConnected) {
      const moved = anchor.element.getBoundingClientRect().top - anchor.top;
      if (moved) this.root.scrollTop += moved;
    }
  }
}

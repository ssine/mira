import { Marked } from "./vendor/marked.js";
import markedCjkFriendly from "./vendor/marked-cjk-friendly.js";

// Share the same CJK delimiter rules in conversations and Markdown file previews.
// The tokenizer preserves source text, code spans/fences and normal GFM parsing.
const { emStrong } = markedCjkFriendly().tokenizer;
export const marked = new Marked({
  gfm: true,
  breaks: false,
  tokenizer: {
    emStrong(...args) {
      const token = emStrong.apply(this, args);
      // Keep CommonMark's whitespace rule: "**label **正文" cannot close bold.
      // marked-cjk-friendly 0.1.2 otherwise accepts this before a CJK character.
      return token && /\s$/u.test(token.text) ? false : token;
    },
  },
});

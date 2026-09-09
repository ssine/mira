// Keep the complete renderer local and reproducible; no CDN or runtime imports.
import { build } from "esbuild";
await build({
  entryPoints: ["node_modules/mermaid/dist/mermaid.esm.min.mjs"],
  outfile: "node/internal/webassets/web/vendor/mermaid.js",
  bundle: true, format: "esm", minify: true, legalComments: "eof",
});

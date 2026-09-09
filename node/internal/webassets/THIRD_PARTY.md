# Embedded Web dependencies

The files under `web/vendor/` are copied unchanged from the exact packages
locked as root development dependencies by `package-lock.json`:

- `@xterm/xterm` 6.0.0 (`xterm.js`, `xterm.css`)
- `@xterm/addon-fit` 0.11.0 (`xterm-addon-fit.js`)
- `marked` 18.0.11 (`marked.js`)
- `dompurify` 3.4.14 (`dompurify.js`)

Their upstream license texts are retained under `licenses/`. These checked-in
browser artifacts are embedded into the Mira executable; production serving
does not read `node_modules`.

Mermaid 11.17.2 (`mermaid.js`) is bundled from its published self-contained ESM
distribution using the pinned esbuild version: run `node scripts/build-mermaid.mjs`
after `npm ci`. Unlike the unchanged copies above, this folds its static and lazy
imports into one local browser module. Upstream bundled license notices remain in
the generated file; Mermaid's license is retained under `licenses/mermaid.txt`.

let renderer;
let renderQueue = Promise.resolve();
const diagrams = new WeakMap();

function renderDiagram(figure) {
  const state = diagrams.get(figure);
  const revision = ++state.revision;
  renderQueue = renderQueue.then(async () => {
    if (!figure.isConnected || revision !== state.revision) return;
    try {
      renderer ??= import("/vendor/mermaid.js").catch((error) => {
        renderer = null;
        throw error;
      });
      const { default: mermaid } = await renderer;
      if (!figure.isConnected || revision !== state.revision) return;
      const style = getComputedStyle(document.documentElement);
      const color = (name) => style.getPropertyValue(name).trim();
      mermaid.initialize({
        startOnLoad: false, securityLevel: "strict", suppressErrorRendering: true,
        theme: "base",
        themeVariables: {
          darkMode: document.documentElement.dataset.theme === "dark",
          background: color("--surface-raised"), primaryColor: color("--accent-soft"),
          primaryTextColor: color("--text"), primaryBorderColor: color("--accent"),
          lineColor: color("--muted"), secondaryColor: color("--surface"),
          tertiaryColor: color("--surface-soft"), edgeLabelBackground: color("--surface-raised"),
        },
        htmlLabels: false, fontFamily: "sans-serif",
        secure: ["secure", "securityLevel", "startOnLoad", "suppressErrorRendering", "maxTextSize", "maxEdges", "htmlLabels"],
      });
      const { svg } = await mermaid.render(`mira-diagram-${crypto.randomUUID()}`, state.source);
      const documentSVG = new DOMParser().parseFromString(svg, "image/svg+xml");
      const svgRoot = documentSVG.documentElement;
      const bounds = svgRoot.getAttribute("viewBox")?.trim().split(/[\s,]+/).map(Number);
      if (bounds?.length === 4 && bounds.every(Number.isFinite) && bounds[2] > 0 && bounds[3] > 0) {
        // Percentage SVG sizes have no intrinsic dimensions in an image document.
        svgRoot.setAttribute("width", String(bounds[2]));
        svgRoot.setAttribute("height", String(bounds[3]));
      }
      // SVG is an image document: diagram CSS, links and scripts never enter the app DOM.
      const image = new Image();
      image.alt = "Mermaid 图表，源码可在下方展开";
      image.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(new XMLSerializer().serializeToString(svgRoot))}`;
      await image.decode();
      if (!figure.isConnected || revision !== state.revision) return;
      const after = state.beforeChange?.();
      state.preview.replaceChildren(image);
      state.status.hidden = true;
      after?.();
    } catch {
      if (!figure.isConnected || revision !== state.revision) return;
      const after = state.beforeChange?.();
      state.status.textContent = "图表暂时无法渲染，请查看源码。";
      state.status.hidden = false;
      state.details.open = true;
      after?.();
    }
  });
}

export function decorateTraceDiagrams(root, beforeChange) {
  for (const code of root.querySelectorAll("pre > code.language-mermaid, pre > code.language-flowchart")) {
    if (code.closest(".trace-diagram")) continue;
    const pre = code.parentElement;
    const figure = document.createElement("figure");
    figure.className = "trace-diagram";
    const preview = document.createElement("div");
    preview.className = "trace-diagram-preview";
    preview.tabIndex = 0;
    preview.setAttribute("role", "region");
    preview.setAttribute("aria-label", "图表，可横向滚动");
    const status = document.createElement("p");
    status.className = "trace-diagram-status";
    status.setAttribute("role", "status");
    status.textContent = "正在渲染图表…";
    const details = document.createElement("details");
    const zoom = document.createElement("button");
    zoom.type = "button";
    zoom.className = "trace-diagram-zoom";
    zoom.textContent = "原始大小";
    zoom.setAttribute("aria-pressed", "false");
    zoom.addEventListener("click", () => {
      const expanded = preview.classList.toggle("trace-diagram-expanded");
      zoom.textContent = expanded ? "适应宽度" : "原始大小";
      zoom.setAttribute("aria-pressed", String(expanded));
    });
    const summary = document.createElement("summary");
    summary.textContent = "查看图表源码";
    details.append(summary);
    pre.replaceWith(figure);
    details.append(pre);
    figure.append(preview, status, zoom, details);
    diagrams.set(figure, { source: code.textContent, preview, status, details, beforeChange, revision: 0 });
    renderDiagram(figure);
  }
}

new MutationObserver(() => {
  for (const figure of document.querySelectorAll(".trace-diagram")) {
    if (diagrams.has(figure)) renderDiagram(figure);
  }
}).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

// Disposable read-only API fixture serving the actual Web sources. No model or
// Node is needed: every displayed image must come from canonical history.
import http from "node:http";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";

export const threadId = "00000000-0000-4000-8000-0000000000a1";
export async function startTranscriptFixture({ port = 0 } = {}) {
  let version = 1, detailRequests = 0, imageRequests = 0, nodeReads = 0;
  const png = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==";
  const row = () => ({ threadId, title: "Transcript regression", cwd: "/work", generation: 1, itemCount: version * 10,
    updatedAt: new Date().toISOString(), activity: { state: "running", generation: 1, itemCount: version * 10, turnId: "turn" } });
  const image = (seq, index = 0) => ({ key: `history-${seq}-image-${index}`, kind: "image", body: "", turnId: "turn", sourceItemSeq: seq, imageIndex: index,
    image: { href: `/v1/codex/threads/${threadId}/transcript/image?storeId=personal&generation=1&itemSeq=${seq}&index=${index}` } });
  const server = http.createServer(async (request, response) => {
    const url = new URL(request.url, "http://localhost"), path = url.pathname;
    const json = value => { response.setHeader("Content-Type", "application/json"); response.end(JSON.stringify(value)); };
    let body = ""; for await (const chunk of request) body += chunk;
    if (path === "/__test/state") {
      if (request.method === "POST") version = JSON.parse(body).version;
      return json({ version, detailRequests, imageRequests, nodeReads });
    }
    if (path === "/healthz") return json({ version: "1.0.4", adminConfigured: true });
    if (path === "/v1/admin/session") return json({ csrfToken: "fixture" });
    if (path === "/v1/nodes") return json({ data: [] });
    if (path.endsWith("/invoke")) { nodeReads++; return json({ error: "No Node filesystem in this fixture" }); }
    if (path === "/v1/codex/threads") return json({ data: [row()] });
    if (path.endsWith("/transcript/image")) {
      imageRequests++;
      await new Promise(resolve => setTimeout(resolve, url.searchParams.get("itemSeq") === "2" ? 120 : 10));
      return json({ url: png });
    }
    if (path.endsWith("/transcript")) {
      const snapshot = version, loaded = url.searchParams.get("toolDetails") === "1";
      if (loaded) { detailRequests++; await new Promise(resolve => setTimeout(resolve, 350)); }
      const output = `revision ${snapshot}\n` + "retained tool output\n".repeat(240);
      return json({ generation: 1, itemCount: snapshot * 10, storeVersion: snapshot, nextCursor: null,
        trace: [{ key: "tool", itemId: "tool", kind: "tool", title: "Long tool", turnId: "turn", sourceItemSeq: 1, status: "完成",
          body: loaded ? output : "", toolFragment: loaded ? { output } : { hasOutput: true },
          toolDetail: { pages: [{ cursor: `page-${snapshot}`, limit: 60, loaded }] } },
          image(2), { key: "reply", kind: "assistant", body: "Between pictures", turnId: "turn", sourceItemSeq: 3 }, image(4),
          // A legacy path-only tool notification must never mount an image.
          { key: "path-only", kind: "tool", title: "Path only", body: "/tmp/missing.png", sourceItemSeq: 5, images: [{ path: "/tmp/missing.png" }] }],
      });
    }
    if (path === `/v1/codex/threads/${threadId}`) return json(row());
    if (path.startsWith("/v1/")) return json({ data: [] });
    try {
      const resource = path === "/" ? "index.html" : path.slice(1);
      if (resource.includes("..")) throw Error("Invalid path");
      const file = path === "/__test/scenarios.mjs" ? new URL("./transcript_images_scenarios.mjs", import.meta.url)
        : new URL(resource.startsWith("vendor/") ? `../node/internal/webassets/web/${resource}` : `../server/public/${resource}`, import.meta.url);
      const type = resource.endsWith(".css") ? "text/css" : /\.(?:js|mjs)$/.test(resource) ? "text/javascript" : resource.endsWith(".html") ? "text/html" : "application/octet-stream";
      response.setHeader("Content-Type", type);
      response.setHeader("Cache-Control", "no-store");
      response.setHeader("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self' ws: wss:");
      response.end(await fs.readFile(file));
    } catch { response.statusCode = 404; response.end(); }
  });
  await new Promise(resolve => server.listen(port, "127.0.0.1", resolve));
  return { origin: `http://127.0.0.1:${server.address().port}`, close: () => new Promise(resolve => server.close(resolve)) };
}
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const fixture = await startTranscriptFixture({ port: Number(process.argv[2] ?? 0) });
  console.log(fixture.origin);
}

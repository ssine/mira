// Both native lists retain independent cursors. Merge their read projections only.
export function conversationPageReader(api, engineOf) {
  const directories = new Map();
  return async query => {
    if (query.get("view") === "children") {
      const engine = engineOf(query.get("parentThreadId"));
      return api(`${engine === "claude" ? "/v1/claude/conversations" : "/v1/codex/threads"}?${query}`);
    }
    let cursors = null;
    if (query.has("cursor")) {
      try { cursors = JSON.parse(query.get("cursor")); } catch { throw new Error("会话分页已过期，请刷新"); }
    }
    const fetchPage = async (engine) => {
      if (cursors && cursors[engine] === null) return { data: [], nextCursor: null };
      const q = new URLSearchParams(query); q.delete("cursor");
      if (cursors?.[engine]) q.set("cursor", cursors[engine]);
      try { return await api(`${engine === "claude" ? "/v1/claude/conversations" : "/v1/codex/threads"}?${q}`, { signal: AbortSignal.timeout(15_000) }); }
      catch (error) { if (engine === "claude" && error.status === 404) return { data: [] }; throw error; }
    };
    const [codex, claude] = await Promise.all([fetchPage("codex"), fetchPage("claude")]);
    const scope = query.get("archived") || "0";
    const catalog = directories.get(scope) || {codex:[],claude:[]};
    if (Array.isArray(codex.projects)) catalog.codex = codex.projects;
    if (Array.isArray(claude.projects)) catalog.claude = claude.projects;
    directories.set(scope, catalog);
    const projects = new Map();
    const time = p => Date.parse(p?.updatedAt) || 0;
    for (const p of [...catalog.codex, ...catalog.claude]) {
      const prior = projects.get(p.key);
      projects.set(p.key, { ...p, count: (prior?.count || 0) + p.count, updatedAt: time(prior) > time(p) ? prior.updatedAt : p.updatedAt });
    }
    const next = { codex: codex.nextCursor || null, claude: claude.nextCursor || null };
    return { ...codex, data: [...(codex.data || []), ...(claude.data || [])].sort((a,b) => Date.parse(b.updatedAt)-Date.parse(a.updatedAt)),
      // Order directories by latest activity across both engines; the sort is
      // stable, so Servers without updatedAt keep their own order.
      ...(codex.projects || claude.projects ? { projects: [...projects.values()].sort((a, b) => time(b) - time(a)) } : {}),
      nextCursor: next.codex || next.claude ? JSON.stringify(next) : null,
      // Legacy Codex responses already contain the full family tree. Do not
      // reinterpret those rows as paginated roots merely because Claude pages.
      paged: codex.paged === true, removed: [...(codex.removed || []), ...(claude.removed || [])] };
  };
}

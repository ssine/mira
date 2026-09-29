// Disposable probe backend, not a production Mira storage path. A managed
// runtime will call the Mira API; it must never receive PostgreSQL credentials.
export class ProbeSessionStore {
  constructor(pool, schema) {
    if (!/^claude_probe_[a-f0-9]+$/.test(schema)) throw new Error("Expected a disposable probe schema");
    this.pool = pool;
    this.schema = schema;
  }

  async initialize() {
    await this.pool.query(`CREATE SCHEMA ${this.schema}`);
    await this.pool.query(`CREATE TABLE ${this.schema}.entries (
      sequence BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
      project_key TEXT NOT NULL, session_id TEXT NOT NULL, subpath TEXT NOT NULL,
      entry_uuid TEXT, payload JSON NOT NULL,
      written_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
      UNIQUE(project_key, session_id, subpath, entry_uuid)
    )`);
  }

  async append(key, entries) {
    if (!entries.length) return;
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      // Serialize commits for one transcript, so sequence order also reflects
      // committed batch order. Subagent keys remain independent transcripts.
      await client.query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", [JSON.stringify([this.schema, key.projectKey, key.sessionId, key.subpath ?? ""])]);
      for (const entry of entries) {
        await client.query(`INSERT INTO ${this.schema}.entries(project_key,session_id,subpath,entry_uuid,payload)
          VALUES($1,$2,$3,$4,$5::json) ON CONFLICT(project_key,session_id,subpath,entry_uuid) DO NOTHING`,
        [key.projectKey, key.sessionId, key.subpath ?? "", typeof entry.uuid === "string" ? entry.uuid : null, JSON.stringify(entry)]);
      }
      await client.query("COMMIT");
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  }

  async load(key) {
    const { rows } = await this.pool.query(`SELECT payload FROM ${this.schema}.entries
      WHERE project_key=$1 AND session_id=$2 AND subpath=$3 ORDER BY sequence`, [key.projectKey, key.sessionId, key.subpath ?? ""]);
    return rows.length ? rows.map(row => row.payload) : null;
  }

  async listSessions(projectKey) {
    const { rows } = await this.pool.query(`SELECT session_id, floor(extract(epoch FROM max(written_at))*1000)::float8 AS mtime
      FROM ${this.schema}.entries WHERE project_key=$1 AND subpath='' GROUP BY session_id`, [projectKey]);
    return rows.map(row => ({ sessionId: row.session_id, mtime: row.mtime }));
  }

  async listSubkeys(key) {
    const { rows } = await this.pool.query(`SELECT DISTINCT subpath FROM ${this.schema}.entries
      WHERE project_key=$1 AND session_id=$2 AND subpath<>'' ORDER BY subpath`, [key.projectKey, key.sessionId]);
    return rows.map(row => row.subpath);
  }

  async dispose() {
    await this.pool.query(`DROP SCHEMA ${this.schema} CASCADE`);
  }
}

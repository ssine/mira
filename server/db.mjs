import crypto from "node:crypto";

const migrations = [
  {
    version: 1,
    name: "event-log-and-node-registry",
    sql: `
      CREATE TABLE IF NOT EXISTS codex_thread_store_snapshots (
        store_id TEXT PRIMARY KEY,
        version BIGINT NOT NULL CHECK (version > 0),
        snapshot JSONB NOT NULL,
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );

      CREATE TABLE IF NOT EXISTS codex_store_events (
        store_id TEXT NOT NULL,
        event_seq BIGINT NOT NULL CHECK (event_seq > 0),
        previous_event_seq BIGINT NOT NULL CHECK (previous_event_seq >= 0),
        operation_id UUID NOT NULL,
        event_format_version INTEGER NOT NULL,
        codex_version TEXT,
        state JSONB NOT NULL,
        history_manifest JSONB NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        PRIMARY KEY (store_id, event_seq),
        UNIQUE (store_id, operation_id)
      );

      CREATE TABLE IF NOT EXISTS codex_thread_events (
        store_id TEXT NOT NULL,
        thread_id TEXT NOT NULL,
        generation BIGINT NOT NULL CHECK (generation > 0),
        item_seq BIGINT NOT NULL CHECK (item_seq > 0),
        store_event_seq BIGINT NOT NULL CHECK (store_event_seq > 0),
        event_format_version INTEGER NOT NULL,
        codex_version TEXT,
        payload JSONB NOT NULL,
        payload_sha256 TEXT NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        PRIMARY KEY (store_id, thread_id, generation, item_seq),
        FOREIGN KEY (store_id, store_event_seq)
          REFERENCES codex_store_events(store_id, event_seq)
          ON DELETE RESTRICT
      );

      CREATE INDEX IF NOT EXISTS codex_thread_events_store_event_idx
        ON codex_thread_events(store_id, store_event_seq);

      CREATE TABLE IF NOT EXISTS codex_thread_projections (
        store_id TEXT NOT NULL,
        thread_id TEXT NOT NULL,
        active_generation BIGINT NOT NULL CHECK (active_generation > 0),
        item_count BIGINT NOT NULL CHECK (item_count >= 0),
        parent_thread_id TEXT,
        source_kind TEXT,
        title TEXT,
        cwd TEXT,
        state JSONB NOT NULL,
        through_event_seq BIGINT NOT NULL CHECK (through_event_seq > 0),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        PRIMARY KEY (store_id, thread_id)
      );

      CREATE INDEX IF NOT EXISTS codex_thread_projections_parent_idx
        ON codex_thread_projections(store_id, parent_thread_id);

      CREATE TABLE IF NOT EXISTS codex_nodes (
        node_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        node_key TEXT NOT NULL UNIQUE,
        hostname TEXT NOT NULL,
        platform TEXT NOT NULL,
        architecture TEXT NOT NULL,
        node_mode TEXT NOT NULL,
        agent_version TEXT NOT NULL,
        capabilities JSONB NOT NULL,
        codex_installations JSONB NOT NULL,
        desired_app_server JSONB NOT NULL DEFAULT '{"running":true}'::jsonb,
        reported_app_server JSONB NOT NULL DEFAULT '{"status":"stopped"}'::jsonb,
        registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );

      CREATE INDEX IF NOT EXISTS codex_nodes_last_seen_idx
        ON codex_nodes(last_seen_at DESC);
    `,
  },
  {
    version: 2,
    name: "node-runtime-status",
    sql: `
      ALTER TABLE codex_nodes
        ADD COLUMN IF NOT EXISTS machine_status JSONB NOT NULL DEFAULT '{}'::jsonb,
        ADD COLUMN IF NOT EXISTS channel_status JSONB NOT NULL DEFAULT '{"connected":false}'::jsonb;
    `,
  },
  {
    version: 3,
    name: "per-store-write-locks",
    sql: `
      CREATE TABLE IF NOT EXISTS codex_store_write_locks (
        store_id TEXT PRIMARY KEY,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );
    `,
  },
  {
    version: 4,
    name: "authoritative-store-heads",
    sql: `
      CREATE TABLE IF NOT EXISTS codex_store_heads (
        store_id TEXT PRIMARY KEY,
        version BIGINT NOT NULL CHECK (version >= 0),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );

      INSERT INTO codex_store_heads (store_id, version)
      SELECT store_id, MAX(event_seq) FROM codex_store_events GROUP BY store_id
      ON CONFLICT (store_id) DO UPDATE SET
        version = GREATEST(codex_store_heads.version, EXCLUDED.version),
        updated_at = NOW();
    `,
  },
  {
    version: 5,
    name: "mira-node-terminology",
    sql: `
      DO $$
      BEGIN
        IF EXISTS (
          SELECT 1 FROM information_schema.columns
          WHERE table_schema = 'public'
            AND table_name = 'codex_nodes'
            AND column_name = 'agent_version'
        ) AND NOT EXISTS (
          SELECT 1 FROM information_schema.columns
          WHERE table_schema = 'public'
            AND table_name = 'codex_nodes'
            AND column_name = 'node_version'
        ) THEN
          ALTER TABLE codex_nodes RENAME COLUMN agent_version TO node_version;
        END IF;
      END $$;
    `,
  },
  {
    version: 6,
    name: "admin-node-identity-and-audit",
    sql: `
      CREATE TABLE IF NOT EXISTS mira_admin_users (
        admin_user_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        singleton BOOLEAN NOT NULL DEFAULT TRUE UNIQUE CHECK (singleton),
        username TEXT NOT NULL,
        password_hash TEXT NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );

      CREATE UNIQUE INDEX IF NOT EXISTS mira_admin_users_username_idx
        ON mira_admin_users (LOWER(username));

      CREATE TABLE IF NOT EXISTS mira_admin_sessions (
        session_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        admin_user_id UUID NOT NULL REFERENCES mira_admin_users(admin_user_id) ON DELETE CASCADE,
        token_hash TEXT NOT NULL UNIQUE,
        csrf_token_hash TEXT NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        expires_at TIMESTAMPTZ NOT NULL,
        revoked_at TIMESTAMPTZ
      );

      CREATE INDEX IF NOT EXISTS mira_admin_sessions_expiry_idx
        ON mira_admin_sessions (expires_at) WHERE revoked_at IS NULL;

      CREATE TABLE IF NOT EXISTS mira_node_enrollment_requests (
        enrollment_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        credential_id UUID NOT NULL UNIQUE,
        credential_secret_hash TEXT NOT NULL,
        credential_fingerprint TEXT NOT NULL,
        node_key TEXT NOT NULL,
        verification_code TEXT NOT NULL,
        hostname TEXT NOT NULL,
        platform TEXT NOT NULL,
        architecture TEXT NOT NULL,
        node_mode TEXT NOT NULL,
        node_version TEXT NOT NULL,
        capabilities JSONB NOT NULL,
        codex_installations JSONB NOT NULL,
        default_desired_app_server JSONB NOT NULL,
        machine_status JSONB NOT NULL DEFAULT '{}'::jsonb,
        requested_from TEXT,
        status TEXT NOT NULL DEFAULT 'pending'
          CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
        decision_note TEXT,
        approved_by UUID REFERENCES mira_admin_users(admin_user_id) ON DELETE SET NULL,
        requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        expires_at TIMESTAMPTZ NOT NULL,
        approved_at TIMESTAMPTZ,
        rejected_at TIMESTAMPTZ,
        node_id UUID
      );

      CREATE INDEX IF NOT EXISTS mira_node_enrollments_status_idx
        ON mira_node_enrollment_requests (status, requested_at DESC);
      CREATE INDEX IF NOT EXISTS mira_node_enrollments_key_idx
        ON mira_node_enrollment_requests (node_key, requested_at DESC);

      ALTER TABLE codex_nodes
        ADD COLUMN IF NOT EXISTS enrollment_id UUID
          REFERENCES mira_node_enrollment_requests(enrollment_id) ON DELETE SET NULL,
        ADD COLUMN IF NOT EXISTS approval_status TEXT NOT NULL DEFAULT 'revoked'
          CHECK (approval_status IN ('approved', 'revoked')),
        ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ,
        ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ,
        ADD COLUMN IF NOT EXISTS last_authenticated_at TIMESTAMPTZ;

      UPDATE codex_nodes SET approval_status = 'revoked', revoked_at = COALESCE(revoked_at, NOW())
        WHERE enrollment_id IS NULL;

      ALTER TABLE mira_node_enrollment_requests
        DROP CONSTRAINT IF EXISTS mira_node_enrollment_requests_node_id_fkey;
      ALTER TABLE mira_node_enrollment_requests
        ADD CONSTRAINT mira_node_enrollment_requests_node_id_fkey
        FOREIGN KEY (node_id) REFERENCES codex_nodes(node_id) ON DELETE SET NULL;

      CREATE TABLE IF NOT EXISTS mira_node_credentials (
        credential_id UUID PRIMARY KEY,
        node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE CASCADE,
        enrollment_id UUID REFERENCES mira_node_enrollment_requests(enrollment_id) ON DELETE SET NULL,
        secret_hash TEXT NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        last_used_at TIMESTAMPTZ,
        revoked_at TIMESTAMPTZ,
        UNIQUE (enrollment_id)
      );

      CREATE INDEX IF NOT EXISTS mira_node_credentials_node_idx
        ON mira_node_credentials (node_id) WHERE revoked_at IS NULL;

      CREATE TABLE IF NOT EXISTS mira_audit_events (
        audit_event_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        action TEXT NOT NULL,
        actor_type TEXT,
        actor_admin_id UUID,
        actor_node_id UUID,
        client_type TEXT,
        target_node_id UUID,
        thread_id TEXT,
        request_id TEXT,
        success BOOLEAN NOT NULL DEFAULT TRUE,
        error_code TEXT,
        request_address TEXT,
        metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );

      CREATE INDEX IF NOT EXISTS mira_audit_events_created_idx
        ON mira_audit_events (created_at DESC);

      CREATE OR REPLACE FUNCTION mira_reject_audit_mutation()
      RETURNS TRIGGER
      LANGUAGE plpgsql
      AS $$
      BEGIN
        RAISE EXCEPTION 'mira_audit_events is append-only';
      END;
      $$;

      DROP TRIGGER IF EXISTS mira_audit_events_append_only ON mira_audit_events;
      CREATE TRIGGER mira_audit_events_append_only
        BEFORE UPDATE OR DELETE ON mira_audit_events
        FOR EACH ROW EXECUTE FUNCTION mira_reject_audit_mutation();
    `,
  },
  {
    version: 7,
    name: "remove-secrets-from-central-app-server-overrides",
    sql: `
      UPDATE codex_nodes AS nodes
      SET desired_app_server = jsonb_set(
        nodes.desired_app_server,
        '{configOverrides}',
        COALESCE(
          (
            SELECT jsonb_agg(value)
            FROM jsonb_array_elements_text(nodes.desired_app_server->'configOverrides') AS value
            WHERE value !~* '(bearer_token|access_token|password|secret|api_key)[[:space:]]*='
          ),
          '[]'::jsonb
        )
      )
      WHERE jsonb_typeof(nodes.desired_app_server->'configOverrides') = 'array';

      UPDATE mira_node_enrollment_requests AS requests
      SET default_desired_app_server = jsonb_set(
        requests.default_desired_app_server,
        '{configOverrides}',
        COALESCE(
          (
            SELECT jsonb_agg(value)
            FROM jsonb_array_elements_text(requests.default_desired_app_server->'configOverrides') AS value
            WHERE value !~* '(bearer_token|access_token|password|secret|api_key)[[:space:]]*='
          ),
          '[]'::jsonb
        )
      )
      WHERE jsonb_typeof(requests.default_desired_app_server->'configOverrides') = 'array';
    `,
  },
  {
    version: 8,
    name: "node-build-metadata",
    sql: `
      ALTER TABLE codex_nodes
        ADD COLUMN IF NOT EXISTS node_build JSONB NOT NULL DEFAULT '{}'::jsonb;

      ALTER TABLE mira_node_enrollment_requests
        ADD COLUMN IF NOT EXISTS node_build JSONB NOT NULL DEFAULT '{}'::jsonb;

      UPDATE codex_nodes
        SET node_build = jsonb_build_object('version', node_version, 'protocolVersion', 1)
        WHERE node_build = '{}'::jsonb;

      UPDATE mira_node_enrollment_requests
        SET node_build = jsonb_build_object('version', node_version, 'protocolVersion', 1)
        WHERE node_build = '{}'::jsonb;
    `,
  },
  {
    version: 9,
    name: "node-ssh-public-keys",
    sql: `
      CREATE TABLE mira_node_ssh_keys (
        credential_id UUID PRIMARY KEY REFERENCES mira_node_credentials(credential_id) ON DELETE CASCADE,
        host_key TEXT NOT NULL,
        client_key TEXT NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );
    `,
  },
  {
    version: 10,
    name: "codex-session-import-provenance",
    sql: `
      CREATE TABLE mira_codex_session_imports (
        import_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        store_id TEXT NOT NULL,
        thread_id TEXT NOT NULL,
        source_node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE RESTRICT,
        source_path TEXT NOT NULL,
        source_sha256 TEXT NOT NULL,
        source_size_bytes BIGINT NOT NULL CHECK (source_size_bytes >= 0),
        source_modified_at TIMESTAMPTZ,
        source_codex_version TEXT,
        source_item_count BIGINT NOT NULL CHECK (source_item_count > 0),
        store_event_seq BIGINT,
        status TEXT NOT NULL CHECK (status IN ('staged', 'imported', 'failed')),
        error_code TEXT,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        UNIQUE (source_node_id, source_path, source_sha256)
      );

      CREATE INDEX mira_codex_session_imports_thread_idx
        ON mira_codex_session_imports(store_id, thread_id, created_at DESC);
      CREATE INDEX mira_codex_session_imports_source_idx
        ON mira_codex_session_imports(source_node_id, source_path, created_at DESC);

      CREATE TABLE mira_codex_session_import_records (
        import_id UUID NOT NULL REFERENCES mira_codex_session_imports(import_id) ON DELETE RESTRICT,
        line_seq BIGINT NOT NULL CHECK (line_seq > 0),
        raw_record JSONB NOT NULL,
        raw_sha256 TEXT NOT NULL,
        PRIMARY KEY (import_id, line_seq)
      );

      CREATE OR REPLACE FUNCTION mira_reject_import_record_mutation()
      RETURNS TRIGGER
      LANGUAGE plpgsql
      AS $$
      BEGIN
        RAISE EXCEPTION 'mira_codex_session_import_records is append-only';
      END;
      $$;

      CREATE TRIGGER mira_codex_session_import_records_append_only
        BEFORE UPDATE OR DELETE ON mira_codex_session_import_records
        FOR EACH ROW EXECUTE FUNCTION mira_reject_import_record_mutation();
    `,
  },
  {
    version: 11,
    name: "codex-thread-runtime-bindings",
    sql: `
      CREATE TABLE mira_codex_thread_runtimes (
        store_id TEXT NOT NULL,
        thread_id TEXT NOT NULL,
        node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE CASCADE,
        bound_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        PRIMARY KEY (store_id, thread_id)
      );

      CREATE INDEX mira_codex_thread_runtimes_node_idx
        ON mira_codex_thread_runtimes(node_id, bound_at DESC);
    `,
  },
  {
    version: 12,
    name: "appserver-thread-start-idempotency",
    sql: `
      CREATE TABLE mira_appserver_thread_start_requests (
        store_id TEXT NOT NULL,
        actor_key TEXT NOT NULL,
        client_request_id UUID NOT NULL,
        target_node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE RESTRICT,
        request_sha256 TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('pending', 'completed', 'failed')),
        thread_id TEXT,
        response JSONB,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        PRIMARY KEY (store_id, actor_key, client_request_id),
        CHECK ((status = 'completed') = (thread_id IS NOT NULL AND response IS NOT NULL))
      );

      CREATE INDEX mira_appserver_thread_start_requests_updated_idx
        ON mira_appserver_thread_start_requests(updated_at DESC);
    `,
  },
  {
    version: 13,
    name: "lossless-rollout-json",
    sql: `
      -- Raw tool output may contain escaped NUL/unpaired UTF-16 surrogates.
      -- PostgreSQL JSONB rejects these; JSON preserves their valid JSON text.
      -- Indexed metadata/manifests remain JSONB. No source records are dropped.
      ALTER TABLE codex_thread_events ALTER COLUMN payload TYPE JSON USING payload::json;
      ALTER TABLE mira_codex_session_import_records ALTER COLUMN raw_record TYPE JSON USING raw_record::json;
      ALTER TABLE codex_thread_store_snapshots ALTER COLUMN snapshot TYPE JSON USING snapshot::json;
    `,
  },
  {
    version: 14,
    name: "import-rollout-lineage",
    sql: `
      ALTER TABLE mira_codex_session_imports ADD COLUMN source_boundary JSONB;
      CREATE TABLE mira_codex_session_import_segments (
        import_id UUID NOT NULL REFERENCES mira_codex_session_imports(import_id),
        segment_index INTEGER NOT NULL,
        source_import_id UUID NOT NULL REFERENCES mira_codex_session_imports(import_id),
        first_line_seq BIGINT NOT NULL CHECK (first_line_seq > 0),
        item_count BIGINT NOT NULL CHECK (item_count > 0),
        end_position JSONB,
        PRIMARY KEY (import_id, segment_index)
      );
    `,
  },
  {
    version: 15,
    name: "web-thread-archive-and-permanent-delete",
    sql: `
      CREATE TABLE mira_thread_actions (
        action_seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        store_id TEXT NOT NULL,
        thread_id TEXT NOT NULL,
        action TEXT NOT NULL CHECK (action IN ('archive', 'restore', 'delete')),
        operation_id UUID NOT NULL,
        generation BIGINT NOT NULL CHECK (generation > 0),
        item_count BIGINT CHECK (item_count >= 0),
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        UNIQUE (store_id, operation_id)
      );
      CREATE INDEX mira_thread_actions_latest_idx ON mira_thread_actions(store_id, thread_id, action_seq DESC);
      CREATE UNIQUE INDEX mira_thread_actions_deleted_idx ON mira_thread_actions(store_id, thread_id) WHERE action = 'delete';

      CREATE FUNCTION mira_without_thread_state(value JSONB, target TEXT) RETURNS JSONB
      LANGUAGE SQL IMMUTABLE AS $$
        SELECT COALESCE(jsonb_object_agg(key, CASE
          WHEN key = 'rollout_paths' AND jsonb_typeof(entry) = 'object' THEN
            COALESCE((SELECT jsonb_object_agg(path, id) FROM jsonb_each(entry) AS paths(path, id) WHERE id <> to_jsonb(target)), '{}'::jsonb)
          WHEN jsonb_typeof(entry) = 'object' THEN entry - target
          ELSE entry END), '{}'::jsonb)
        FROM jsonb_each(value) AS fields(key, entry)
      $$;

      -- Keep the erasure fence effective even if an older Server is rolled back.
      CREATE FUNCTION mira_reject_deleted_thread_event() RETURNS TRIGGER
      LANGUAGE plpgsql AS $$
      BEGIN
        IF EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=NEW.store_id AND action='delete'
          AND (NEW.history_manifest ? thread_id OR NEW.state <> mira_without_thread_state(NEW.state,thread_id))) THEN
          RAISE EXCEPTION 'permanently deleted thread cannot be recreated' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
      END;
      $$;
      CREATE TRIGGER codex_store_events_deleted_thread_fence BEFORE INSERT ON codex_store_events
        FOR EACH ROW EXECUTE FUNCTION mira_reject_deleted_thread_event();

      -- Ordinary import provenance stays immutable. Explicit permanent deletion
      -- may remove source records only after no surviving lineage references them.
      CREATE OR REPLACE FUNCTION mira_reject_import_record_mutation() RETURNS TRIGGER
      LANGUAGE plpgsql AS $$
      BEGIN
        IF TG_OP = 'DELETE' AND EXISTS (
          SELECT 1 FROM mira_codex_session_imports imports JOIN mira_thread_actions actions
            ON actions.store_id = imports.store_id AND actions.thread_id = imports.thread_id AND actions.action = 'delete'
          WHERE imports.import_id = OLD.import_id
        ) AND NOT EXISTS (SELECT 1 FROM mira_codex_session_import_segments WHERE source_import_id = OLD.import_id) THEN
          RETURN OLD;
        END IF;
        RAISE EXCEPTION 'mira_codex_session_import_records is append-only';
      END;
      $$;
    `,
  },
];

export async function initializeDatabase(pool) {
  await pool.query(`
    CREATE TABLE IF NOT EXISTS codex_schema_migrations (
      version INTEGER PRIMARY KEY,
      name TEXT NOT NULL,
      checksum TEXT NOT NULL,
      applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
    )
  `);

  for (const migration of migrations) {
    const checksum = crypto.createHash("sha256").update(migration.sql).digest("hex");
    const existing = await pool.query(
      "SELECT name, checksum FROM codex_schema_migrations WHERE version = $1",
      [migration.version],
    );
    if (existing.rowCount > 0) {
      const row = existing.rows[0];
      if (row.name !== migration.name || row.checksum !== checksum) {
        throw new Error(`database migration ${migration.version} checksum mismatch`);
      }
      continue;
    }

    const client = await pool.connect();
    try {
      await client.query("BEGIN");
      await client.query(migration.sql);
      await client.query(
        `INSERT INTO codex_schema_migrations (version, name, checksum)
         VALUES ($1, $2, $3)`,
        [migration.version, migration.name, checksum],
      );
      await client.query("COMMIT");
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  }
}

export function currentSchemaVersion() {
  return migrations.at(-1)?.version ?? 0;
}

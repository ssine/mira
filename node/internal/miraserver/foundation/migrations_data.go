package foundation

// Migration is one immutable database schema transition. Checksum is the SHA-256
// of SQL and must stay compatible with the released JavaScript server.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

var schemaMigrations = []Migration{
	{
		Version: 1,
		Name:    "event-log-and-node-registry",
		SQL: `
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
		Checksum: "99f872cf9ef0e051a4d50995a6abf714f25fa123116a71165548e6d7a5d76569",
	},
	{
		Version: 2,
		Name:    "node-runtime-status",
		SQL: `
      ALTER TABLE codex_nodes
        ADD COLUMN IF NOT EXISTS machine_status JSONB NOT NULL DEFAULT '{}'::jsonb,
        ADD COLUMN IF NOT EXISTS channel_status JSONB NOT NULL DEFAULT '{"connected":false}'::jsonb;
    `,
		Checksum: "a5312ecc2f2265744914cf8440c210a729497cd239ba02b1275b5264ea089ef3",
	},
	{
		Version: 3,
		Name:    "per-store-write-locks",
		SQL: `
      CREATE TABLE IF NOT EXISTS codex_store_write_locks (
        store_id TEXT PRIMARY KEY,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );
    `,
		Checksum: "b1a1ce80ce50eb7d51a2f64b5b182380a860ce91b2e79ab2265ea7acf8b45a89",
	},
	{
		Version: 4,
		Name:    "authoritative-store-heads",
		SQL: `
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
		Checksum: "128321452a22fe7d57761a46829edcf3a01f5f517d80bd970f81005f4e1a0300",
	},
	{
		Version: 5,
		Name:    "mira-node-terminology",
		SQL: `
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
		Checksum: "14d08cc9e4da3022a102c557ccf40ca5eb8f45d4e23a65d29893e0b366c259ba",
	},
	{
		Version: 6,
		Name:    "admin-node-identity-and-audit",
		SQL: `
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
		Checksum: "0eeb066f10e2c1014b2605c408b4f4bde35eb29ce7ecb2711e9fa834f26b77f9",
	},
	{
		Version: 7,
		Name:    "remove-secrets-from-central-app-server-overrides",
		SQL: `
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
		Checksum: "69c6cd67cace3574896ea11b013e3716214b388962de04227352634befec6edd",
	},
	{
		Version: 8,
		Name:    "node-build-metadata",
		SQL: `
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
		Checksum: "967bb84c5524d5ca7cecfac48dab237943c22772f7adf1829c2e192967afeaa5",
	},
	{
		Version: 9,
		Name:    "node-ssh-public-keys",
		SQL: `
      CREATE TABLE mira_node_ssh_keys (
        credential_id UUID PRIMARY KEY REFERENCES mira_node_credentials(credential_id) ON DELETE CASCADE,
        host_key TEXT NOT NULL,
        client_key TEXT NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
      );
    `,
		Checksum: "10eb56a3fc87d102656e0637ddd1a2956a8a54f9a20f9390fa56bb453ef9f53b",
	},
	{
		Version: 10,
		Name:    "codex-session-import-provenance",
		SQL: `
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
		Checksum: "43dbed25d07ba1cd66de39e4df7872597c5ed9bfe6a7684ed688a9a2b803acf3",
	},
	{
		Version: 11,
		Name:    "codex-thread-runtime-bindings",
		SQL: `
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
		Checksum: "e724054d3f7230c8d928384f384bdf4bfb87b766567a466b2d8551746d360d84",
	},
	{
		Version: 12,
		Name:    "appserver-thread-start-idempotency",
		SQL: `
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
		Checksum: "030b06b9f847f686b235b95223f9a0cdd3cbd26d6b5bfeddecdcb3befb9b6ac0",
	},
	{
		Version: 13,
		Name:    "lossless-rollout-json",
		SQL: `
      -- Raw tool output may contain escaped NUL/unpaired UTF-16 surrogates.
      -- PostgreSQL JSONB rejects these; JSON preserves their valid JSON text.
      -- Indexed metadata/manifests remain JSONB. No source records are dropped.
      ALTER TABLE codex_thread_events ALTER COLUMN payload TYPE JSON USING payload::json;
      ALTER TABLE mira_codex_session_import_records ALTER COLUMN raw_record TYPE JSON USING raw_record::json;
      ALTER TABLE codex_thread_store_snapshots ALTER COLUMN snapshot TYPE JSON USING snapshot::json;
    `,
		Checksum: "2aef1dc29099f2b9dc8dcdf472fb215deff11347c2df6773dc0318728e0bdd8b",
	},
	{
		Version: 14,
		Name:    "import-rollout-lineage",
		SQL: `
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
		Checksum: "fea59f493c68d416516a1e1982482a57366a51004bc59e95b9fd5d549566d8aa",
	},
	{
		Version: 15,
		Name:    "web-thread-archive-and-permanent-delete",
		SQL: `
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
		Checksum: "00bd305f00395d30027648ccd2ea9c80a1d19f8199cd8b75c3de370627ddee79",
	},
	{
		Version: 16,
		Name:    "bounded-background-thread-erasure",
		SQL: `
      CREATE TABLE mira_thread_erasures (
        store_id TEXT NOT NULL,
        thread_id TEXT NOT NULL,
        action_seq BIGINT NOT NULL REFERENCES mira_thread_actions(action_seq),
        through_event_seq BIGINT NOT NULL CHECK (through_event_seq >= 0),
        after_event_seq BIGINT NOT NULL DEFAULT 0 CHECK (after_event_seq >= 0),
        phase TEXT NOT NULL DEFAULT 'events' CHECK (phase IN ('events','history','provenance','complete')),
        retry_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        last_error_code TEXT,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        completed_at TIMESTAMPTZ,
        PRIMARY KEY (store_id,thread_id)
      );
      CREATE INDEX mira_thread_erasures_pending_idx ON mira_thread_erasures(updated_at) WHERE phase<>'complete';
    `,
		Checksum: "bd3e21e39b76e3ff2db8b15b1e228c522c2d150781328eb0be1f9a69b895b32b",
	},
	{
		Version: 17,
		Name:    "normalized-thread-records-cutover",
		SQL: `
  DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM codex_thread_store_snapshots s WHERE NOT EXISTS(
      SELECT 1 FROM codex_store_events e WHERE e.store_id=s.store_id))
    THEN RAISE EXCEPTION 'legacy snapshot-only stores require canonical import before cutover'; END IF;
  END $$;
  ALTER TABLE codex_store_heads ADD COLUMN history_floor BIGINT NOT NULL DEFAULT 0;
  ALTER TABLE codex_store_events RENAME TO codex_store_events_legacy;
  CREATE TABLE codex_store_events (
    store_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    event_seq BIGINT,
    previous_event_seq BIGINT,
    result_version BIGINT,
    request_sha256 TEXT,
    event_format_version INTEGER NOT NULL DEFAULT 2,
    codex_version TEXT,
    appended_item_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(store_id,operation_id),
    UNIQUE(store_id,event_seq)
  );
  CREATE TABLE codex_store_state_entries (
    store_id TEXT NOT NULL,
    field TEXT NOT NULL,
    entry_key TEXT NOT NULL DEFAULT '',
    is_root BOOLEAN NOT NULL,
    thread_id TEXT,
    value JSONB NOT NULL,
    PRIMARY KEY(store_id,field,is_root,entry_key)
  );
  CREATE INDEX codex_store_state_entries_thread_idx ON codex_store_state_entries(store_id,thread_id);
  CREATE TABLE codex_store_state_changes (
    store_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    change_seq INTEGER NOT NULL,
    thread_id TEXT,
    path TEXT[] NOT NULL,
    mode TEXT NOT NULL CHECK(mode IN ('set','remove')),
    value JSON,
    PRIMARY KEY(store_id,operation_id,change_seq),
    FOREIGN KEY(store_id,operation_id) REFERENCES codex_store_events(store_id,operation_id)
  );
  CREATE INDEX codex_store_state_changes_thread_idx ON codex_store_state_changes(store_id,thread_id);
  CREATE TABLE codex_thread_revisions (
    store_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    operation_id UUID NOT NULL,
    generation BIGINT NOT NULL,
    item_count BIGINT NOT NULL,
    active BOOLEAN NOT NULL,
    PRIMARY KEY(store_id,thread_id,operation_id),
    FOREIGN KEY(store_id,operation_id) REFERENCES codex_store_events(store_id,operation_id)
  );
  CREATE INDEX codex_thread_revisions_commit_idx ON codex_thread_revisions(store_id,operation_id);
  CREATE TEMP TABLE mira_storage_baseline ON COMMIT DROP AS
    SELECT heads.store_id, heads.version, gen_random_uuid() AS operation_id,
      COALESCE(events.state,'{}'::jsonb) AS state,
      COALESCE(events.history_manifest,'{}'::jsonb) AS manifest
    FROM codex_store_heads heads LEFT JOIN codex_store_events_legacy events
      ON events.store_id=heads.store_id AND events.event_seq=heads.version;
  DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM codex_store_heads h WHERE h.version>0 AND NOT EXISTS(
      SELECT 1 FROM codex_store_events_legacy e WHERE e.store_id=h.store_id AND e.event_seq=h.version))
    THEN RAISE EXCEPTION 'cannot migrate a missing canonical store head'; END IF;
  END $$;
  INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,codex_version)
    SELECT store_id,operation_id,version,0,version,'mira-storage-baseline' FROM mira_storage_baseline WHERE version>0;
  INSERT INTO codex_store_state_entries(store_id,field,is_root,value)
    SELECT b.store_id,f.key,true,CASE WHEN jsonb_typeof(f.value)='object' THEN '{}'::jsonb ELSE f.value END
    FROM mira_storage_baseline b CROSS JOIN LATERAL jsonb_each(b.state) f;
  INSERT INTO codex_store_state_entries(store_id,field,entry_key,is_root,thread_id,value)
    SELECT b.store_id,f.key,e.key,false,
      CASE WHEN f.key='rollout_paths' THEN CASE WHEN jsonb_typeof(e.value)='string' THEN e.value#>>'{}' ELSE NULL END ELSE e.key END,e.value
    FROM mira_storage_baseline b CROSS JOIN LATERAL jsonb_each(b.state) f
    CROSS JOIN LATERAL jsonb_each(CASE WHEN jsonb_typeof(f.value)='object' THEN f.value ELSE '{}'::jsonb END) e;
  INSERT INTO codex_store_state_changes(store_id,operation_id,change_seq,thread_id,path,mode,value)
    SELECT s.store_id,b.operation_id,(row_number() OVER(PARTITION BY s.store_id ORDER BY s.field,s.is_root DESC,s.entry_key))::int,
      s.thread_id,CASE WHEN s.is_root THEN ARRAY[s.field] ELSE ARRAY[s.field,s.entry_key] END,'set',s.value::json
    FROM codex_store_state_entries s JOIN mira_storage_baseline b USING(store_id) WHERE b.version>0;
  INSERT INTO codex_thread_revisions(store_id,thread_id,operation_id,generation,item_count,active)
    SELECT b.store_id,e.key,b.operation_id,(e.value->>'generation')::bigint,(e.value->>'itemCount')::bigint,true
    FROM mira_storage_baseline b CROSS JOIN LATERAL jsonb_each(b.manifest) e;
  ALTER TABLE codex_thread_events ADD COLUMN operation_id UUID;
  UPDATE codex_thread_events items SET operation_id=b.operation_id FROM mira_storage_baseline b WHERE items.store_id=b.store_id;
  ALTER TABLE codex_thread_events ALTER COLUMN operation_id SET NOT NULL;
  ALTER TABLE codex_thread_events DROP COLUMN store_event_seq CASCADE;
  ALTER TABLE codex_thread_events ADD FOREIGN KEY(store_id,operation_id) REFERENCES codex_store_events(store_id,operation_id);
  CREATE INDEX codex_thread_events_commit_idx ON codex_thread_events(store_id,operation_id);
  CREATE VIEW codex_thread_events_versioned AS
    SELECT items.*,commits.event_seq AS store_event_seq FROM codex_thread_events items
      JOIN codex_store_events commits USING(store_id,operation_id);
  UPDATE codex_store_heads SET history_floor=version;
  UPDATE codex_thread_projections p SET through_event_seq=b.version FROM mira_storage_baseline b WHERE p.store_id=b.store_id;
  DROP TABLE codex_store_events_legacy;
  DROP FUNCTION mira_reject_deleted_thread_event();
  DROP TABLE codex_thread_store_snapshots;
  DROP TABLE codex_store_write_locks;
  CREATE FUNCTION mira_reject_deleted_thread_row() RETURNS TRIGGER LANGUAGE plpgsql AS $$
  BEGIN
    IF EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=NEW.store_id AND thread_id=NEW.thread_id AND action='delete')
    THEN RAISE EXCEPTION 'permanently deleted thread cannot be recreated' USING ERRCODE='23514'; END IF;
    RETURN NEW;
  END $$;
  CREATE TRIGGER codex_thread_events_deleted_fence BEFORE INSERT ON codex_thread_events
    FOR EACH ROW EXECUTE FUNCTION mira_reject_deleted_thread_row();
  CREATE TRIGGER codex_state_entries_deleted_fence BEFORE INSERT OR UPDATE ON codex_store_state_entries
    FOR EACH ROW EXECUTE FUNCTION mira_reject_deleted_thread_row();
`,
		Checksum: "59371d803cf7ac559fe9d75159cc1527c694c0c13453394a994258ce7d860fc2",
	},
	{
		Version: 18,
		Name:    "thread-lifecycle-lookup",
		SQL: `CREATE INDEX codex_thread_events_lifecycle_idx
      ON codex_thread_events(store_id, thread_id, generation, item_seq DESC)
      WHERE payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_started|turn_started|task_complete|turn_complete|turn_aborted|error)"';`,
		Checksum: "9e51adaa35356299241d4d3cf9f0515b4497998b189aba8b025f9be43e9ba204",
	},
	{
		Version: 19,
		Name:    "shared-web-thread-read-positions",
		SQL: `
  CREATE TABLE mira_thread_read_positions (
    store_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    item_count BIGINT NOT NULL CHECK (item_count >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (store_id, thread_id)
  );
  -- Existing history predates read tracking. Start with a quiet baseline;
  -- subsequent conversations/updates are unread until actually viewed.
  INSERT INTO mira_thread_read_positions(store_id,thread_id,generation,item_count)
    SELECT store_id,thread_id,active_generation,item_count FROM codex_thread_projections;
  CREATE INDEX codex_thread_events_visible_update_idx
    ON codex_thread_events(store_id,thread_id,generation,item_seq DESC)
    WHERE payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(user_message|agent_message|item_completed|view_image_tool_call|task_complete|turn_complete|turn_aborted|error|message|function_call_output|custom_tool_call_output)"';
`,
		Checksum: "f4c060c16ac7db73d785416115891b68c34ee7da447774206f9756d6da5eb778",
	},
	{
		Version: 20,
		Name:    "account-quota-history",
		SQL: `
  CREATE TABLE mira_account_quota_samples (
    node_id UUID NOT NULL REFERENCES codex_nodes(node_id),
    runtime_key TEXT NOT NULL,
    sampled_at TIMESTAMPTZ NOT NULL,
    sample_slot BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ok', 'error')),
    account_type TEXT,
    email TEXT,
    plan_type TEXT,
    remaining DOUBLE PRECISION CHECK (remaining BETWEEN 0 AND 100),
    resets_at TIMESTAMPTZ,
    reset_count BIGINT CHECK (reset_count >= 0),
    PRIMARY KEY (node_id, runtime_key, sample_slot)
  );
  CREATE INDEX mira_account_quota_samples_time_idx
    ON mira_account_quota_samples(node_id, runtime_key, sampled_at DESC);
`,
		Checksum: "6cae78d664e4874576144799ec9e143c83f9d8207da81bbd490456b0ee4093ee",
	},
	{
		Version: 21,
		Name:    "thread-token-usage-lookup",
		SQL: `CREATE INDEX codex_thread_events_token_usage_idx
  ON codex_thread_events(store_id,thread_id,generation,item_seq DESC)
  WHERE payload::text ~ '"type"[[:space:]]*:[[:space:]]*"token_count"';`,
		Checksum: "3427d67078470ec7729e7d3c4b0a52490764dc335d8d0ba14f7f2fd5593564e8",
	},
	{
		Version: 22,
		Name:    "thread-cost-event-lookup",
		SQL: `CREATE INDEX codex_thread_events_cost_idx
  ON codex_thread_events(store_id,thread_id,generation,item_seq)
  WHERE payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(turn_context|token_count|thread_settings_applied)"';
CREATE INDEX codex_thread_events_model_idx
  ON codex_thread_events(store_id,thread_id,generation,item_seq)
  WHERE payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(turn_context|thread_settings_applied)"';`,
		Checksum: "78cc4d33d9aedb43e2d42e97535c19d2ca47d2efc9a802579ca0422c9f301285",
	},
	{
		Version: 23,
		Name:    "transcript-turn-boundary-lookup",
		SQL: `CREATE INDEX codex_thread_events_turn_context_idx
      ON codex_thread_events(store_id, thread_id, generation, item_seq DESC)
      WHERE payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_started|turn_started|turn_context)"';
    CREATE INDEX codex_thread_events_turn_completion_idx
      ON codex_thread_events(store_id, thread_id, generation, item_seq)
      WHERE payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_complete|turn_complete|turn_aborted)"';`,
		Checksum: "788302c740260e02dcd466ab1227007dea5f0a28e200bd7aa5e098b6f9b8fe08",
	},
	{
		Version: 24,
		Name:    "node-user-metadata",
		SQL: `
      ALTER TABLE codex_nodes
        ADD COLUMN display_name TEXT,
        ADD COLUMN labels JSONB NOT NULL DEFAULT '{}'::jsonb,
        ADD COLUMN metadata_revision BIGINT NOT NULL DEFAULT 0 CHECK (metadata_revision >= 0);

      CREATE TABLE mira_node_aliases (
        alias_key TEXT PRIMARY KEY,
        alias TEXT NOT NULL,
        node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE CASCADE,
        created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        UNIQUE (node_id, alias_key)
      );

      CREATE INDEX mira_node_aliases_node_idx ON mira_node_aliases(node_id, alias_key);
    `,
		Checksum: "de8dd9262bd86168b05a879d21397f806dae66670fa2415c94732908a1033899",
	},
	{
		Version: 25,
		Name:    "node-developer-instructions-file",
		SQL: `
      ALTER TABLE codex_nodes ALTER COLUMN desired_app_server
        SET DEFAULT '{"running":true,"developerInstructionsFile":null}'::jsonb;

      UPDATE codex_nodes
      SET desired_app_server = jsonb_set(desired_app_server, '{developerInstructionsFile}', 'null'::jsonb, true)
      WHERE NOT desired_app_server ? 'developerInstructionsFile';

      UPDATE mira_node_enrollment_requests
      SET default_desired_app_server = jsonb_set(default_desired_app_server, '{developerInstructionsFile}', 'null'::jsonb, true)
      WHERE NOT default_desired_app_server ? 'developerInstructionsFile';
    `,
		Checksum: "2e1e213f560c1156d2d9f79c7f594c948dce6e23f1f807b9dd0a8f65d19fa8e2",
	},
	{
		Version: 26,
		Name:    "codex-session-import-store-isolation-expand",
		SQL: `
      ALTER TABLE mira_codex_session_imports
        ADD CONSTRAINT mira_codex_session_imports_store_source_unique
        UNIQUE (store_id, source_node_id, source_path, source_sha256);
    `,
		Checksum: "b82604a878f8788bd8e7a173efc6a48b9de2215317dd130528dae642869be66f",
	},
	{Version: 27, Name: "chunked-history-uploads", SQL: `
      CREATE TABLE mira_history_uploads (
        store_id TEXT NOT NULL, upload_id UUID NOT NULL, thread_id TEXT NOT NULL,
        item_count BIGINT NOT NULL CHECK(item_count>=0), total_bytes BIGINT NOT NULL CHECK(total_bytes>=0),
        received_bytes BIGINT NOT NULL DEFAULT 0 CHECK(received_bytes>=0 AND received_bytes<=total_bytes),
        status TEXT NOT NULL DEFAULT 'uploading' CHECK(status IN ('uploading','sealed','committed','cancelled')),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(store_id,upload_id)
      );
      CREATE INDEX mira_history_uploads_expiry ON mira_history_uploads(updated_at);
      CREATE TABLE mira_history_upload_chunks (
        store_id TEXT NOT NULL, upload_id UUID NOT NULL, byte_offset BIGINT NOT NULL CHECK(byte_offset>=0),
        data BYTEA NOT NULL CHECK(octet_length(data) BETWEEN 1 AND 4194304),
        PRIMARY KEY(store_id,upload_id,byte_offset),
        FOREIGN KEY(store_id,upload_id) REFERENCES mira_history_uploads ON DELETE CASCADE
      );
      CREATE TABLE mira_history_upload_items (
        store_id TEXT NOT NULL, upload_id UUID NOT NULL, item_seq BIGINT NOT NULL CHECK(item_seq>0),
        payload JSON NOT NULL, payload_sha256 TEXT NOT NULL, PRIMARY KEY(store_id,upload_id,item_seq),
        FOREIGN KEY(store_id,upload_id) REFERENCES mira_history_uploads ON DELETE CASCADE
      );
    `, Checksum: "4019c43a6c95834ef08b2e37ec19272058c3a180a9f2f466a231e1336731b876"},
	{Version: 28, Name: "codex-account-resources", SQL: codexAccountsSQL, Checksum: "32123250e3d764c783014572e68c8b22d959d4f55253525656f3c2401b85c5cc"},
	{Version: 29, Name: "canonical-agent-graph", SQL: agentGraphSQL, Checksum: "3cb1c9b27efc99b54dd1e62a83bf3cd964e93b23ac0f82482104d16c15ee7111"},
}

// Schema 17 is an explicit cutover: preserve canonical items/current metadata,
// retire historical full-store snapshots and establish a new readable baseline.
export const storageRowsMigration = `
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
`;

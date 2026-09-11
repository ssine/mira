package foundation

// Each event contains the complete resulting edge, so the mutable graph is
// rebuildable without consulting node-local SQLite or guessing closed status.
const agentGraphSQL = `
CREATE TABLE mira_agent_graph_events (
  event_seq BIGSERIAL PRIMARY KEY,
  store_id TEXT NOT NULL,
  operation_id UUID NOT NULL,
  request_sha256 TEXT NOT NULL,
  child_thread_id TEXT NOT NULL,
  parent_thread_id TEXT,
  child_generation BIGINT,
  parent_generation BIGINT,
  status TEXT NOT NULL CHECK(status IN ('open','closed')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE(store_id,operation_id)
);
CREATE INDEX mira_agent_graph_events_child_idx ON mira_agent_graph_events(store_id,child_thread_id,event_seq DESC);
CREATE TABLE mira_agent_graph_edges (
  store_id TEXT NOT NULL,
  child_thread_id TEXT NOT NULL,
  parent_thread_id TEXT NOT NULL,
  child_generation BIGINT NOT NULL,
  parent_generation BIGINT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('open','closed')),
  event_seq BIGINT NOT NULL REFERENCES mira_agent_graph_events(event_seq),
  PRIMARY KEY(store_id,child_thread_id)
);
CREATE INDEX mira_agent_graph_edges_parent_idx ON mira_agent_graph_edges(store_id,parent_thread_id);
CREATE FUNCTION mira_rebuild_agent_graph(selected_store TEXT) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('["mira-store",' || to_json(selected_store)::text || ']',0));
  DELETE FROM mira_agent_graph_edges WHERE store_id=selected_store;
  INSERT INTO mira_agent_graph_edges(store_id,child_thread_id,parent_thread_id,child_generation,parent_generation,status,event_seq)
    SELECT DISTINCT ON(child_thread_id) store_id,child_thread_id,parent_thread_id,child_generation,parent_generation,status,event_seq
    FROM mira_agent_graph_events WHERE store_id=selected_store AND parent_thread_id IS NOT NULL
    ORDER BY child_thread_id,event_seq DESC;
END $$;
`

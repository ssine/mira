package foundation

// Port sites are configuration, independent of login sessions and Node control
// epochs. Retired names remain reserved so deleting a URL cannot silently give
// a different application its old browser origin.
const portSitesSQL = `
CREATE TABLE mira_port_sites (
  site_id UUID PRIMARY KEY,
  name TEXT NOT NULL UNIQUE CHECK(name ~ '^[a-z][a-z0-9-]{0,59}$'),
  node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE RESTRICT,
  port INTEGER NOT NULL CHECK(port BETWEEN 1 AND 65535),
  scheme TEXT NOT NULL DEFAULT 'http' CHECK(scheme IN ('http','https')),
  enabled BOOLEAN NOT NULL DEFAULT true,
  revision BIGINT NOT NULL DEFAULT 1 CHECK(revision > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ
);
CREATE INDEX mira_port_sites_live_name_idx ON mira_port_sites(name) WHERE deleted_at IS NULL;
`

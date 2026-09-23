package foundation

// Additive: the preceding Server can continue using its Web Push tables.
const androidNotificationsSQL = `
CREATE TABLE mira_node_notification_subscriptions (
 node_id UUID PRIMARY KEY REFERENCES codex_nodes ON DELETE CASCADE,
 session_id UUID NOT NULL REFERENCES mira_admin_sessions ON DELETE CASCADE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE mira_node_notification_deliveries (
 delivery_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 node_id UUID NOT NULL REFERENCES mira_node_notification_subscriptions ON DELETE CASCADE,
 store_id TEXT NOT NULL, thread_id TEXT NOT NULL, generation BIGINT NOT NULL,
 turn_id TEXT NOT NULL, title TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 expires_at TIMESTAMPTZ NOT NULL DEFAULT now()+interval '1 day',
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 finished BOOLEAN NOT NULL DEFAULT FALSE,
 UNIQUE(node_id,store_id,thread_id,generation,turn_id)
);
CREATE INDEX mira_node_notifications_pending ON mira_node_notification_deliveries(next_attempt_at) WHERE NOT finished;
CREATE INDEX mira_node_notifications_expiry ON mira_node_notification_deliveries(expires_at);
`

package foundation

// Delivery state is independent of canonical history and cannot replay tools.
// No backfill: subscribing must never notify about existing conversations.
const pushNotificationsSQL = `
CREATE TABLE mira_push_keys (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
 public_key TEXT NOT NULL, private_key TEXT NOT NULL
);
CREATE TABLE mira_push_subscriptions (
 subscription_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 endpoint TEXT NOT NULL UNIQUE, p256dh TEXT NOT NULL, auth TEXT NOT NULL,
 session_id UUID NOT NULL REFERENCES mira_admin_sessions ON DELETE CASCADE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mira_push_subscriptions_session ON mira_push_subscriptions(session_id);
CREATE TABLE mira_push_deliveries (
 delivery_id BIGSERIAL PRIMARY KEY,
 subscription_id UUID NOT NULL REFERENCES mira_push_subscriptions ON DELETE CASCADE,
 runtime TEXT NOT NULL CHECK(runtime = 'codex'),
 store_id TEXT NOT NULL, thread_id TEXT NOT NULL, generation BIGINT NOT NULL,
 turn_id TEXT NOT NULL, title TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 expires_at TIMESTAMPTZ NOT NULL DEFAULT now()+interval '1 day',
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 attempts INTEGER NOT NULL DEFAULT 0, finished BOOLEAN NOT NULL DEFAULT FALSE,
 UNIQUE(subscription_id,runtime,store_id,thread_id,generation,turn_id)
);
CREATE INDEX mira_push_deliveries_pending ON mira_push_deliveries(next_attempt_at) WHERE NOT finished;
CREATE INDEX mira_push_deliveries_expiry ON mira_push_deliveries(expires_at);
`

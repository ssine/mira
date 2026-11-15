package foundation

// Additive (expand only): the previous Server keeps reading completion deliveries.
// Alert rows use store_id 'alert'; an older Server finds no matching thread
// projection and finishes them without delivery, which is safe after a rollback.
const nodeAlertsSQL = `
ALTER TABLE mira_node_notification_deliveries
 ADD COLUMN kind TEXT NOT NULL DEFAULT 'completion',
 ADD COLUMN body TEXT NOT NULL DEFAULT '',
 ADD COLUMN level TEXT NOT NULL DEFAULT '',
 ADD COLUMN url TEXT NOT NULL DEFAULT '',
 ADD COLUMN source_node_id UUID;
ALTER TABLE mira_node_notification_deliveries
 ADD CONSTRAINT mira_node_notification_kind CHECK (kind IN ('completion','alert'));
`

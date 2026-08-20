ALTER TABLE alerts
    ADD COLUMN IF NOT EXISTS notifications_muted BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS notifications_muted_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS notification_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_notified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS next_notification_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resolution_notification_pending BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS notification_claimed_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS notification_claim_until TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_alerts_notification_due
    ON alerts(next_notification_at)
    WHERE next_notification_at IS NOT NULL;

-- Existing active alerts must enter the new dispatcher once. Without this,
-- their next_notification_at would stay NULL forever because repeated monitor
-- observations preserve the reminder clock of an already firing alert.
UPDATE alerts
SET next_notification_at = NOW()
WHERE state IN ('firing', 'acked') AND next_notification_at IS NULL;

CREATE TABLE IF NOT EXISTS notification_settings (
    id                       SMALLINT PRIMARY KEY CHECK (id = 1),
    enabled                  BOOLEAN,
    min_severity             TEXT,
    default_repeat_minutes   INTEGER,
    notify_on_resolved       BOOLEAN,
    ack_stops_repeats        BOOLEAN,
    max_repeats              INTEGER,
    updated_by               TEXT NOT NULL DEFAULT '',
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (min_severity IS NULL OR min_severity IN ('info', 'warning', 'critical')),
    CHECK (default_repeat_minutes IS NULL OR default_repeat_minutes BETWEEN 0 AND 525600),
    CHECK (max_repeats IS NULL OR max_repeats BETWEEN 0 AND 10000)
);

INSERT INTO notification_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS notification_policies (
    kind             TEXT PRIMARY KEY,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    repeat_minutes   INTEGER NOT NULL DEFAULT 0 CHECK (repeat_minutes BETWEEN 0 AND 525600),
    notify_resolved  BOOLEAN NOT NULL DEFAULT FALSE,
    stop_on_ack      BOOLEAN NOT NULL DEFAULT TRUE,
    max_repeats      INTEGER NOT NULL DEFAULT 0 CHECK (max_repeats BETWEEN 0 AND 10000),
    channels         JSONB NOT NULL DEFAULT '[]'::jsonb,
    updated_by       TEXT NOT NULL DEFAULT '',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS notification_deliveries (
    id            TEXT PRIMARY KEY,
    alert_id      TEXT NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
    event         TEXT NOT NULL CHECK (event IN ('opened', 'reminder', 'resolved')),
    sequence      INTEGER NOT NULL,
    channel       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sending', 'sent', 'failed')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 5,
    scheduled_at  TIMESTAMPTZ NOT NULL,
    lease_owner   TEXT NOT NULL DEFAULT '',
    lease_until   TIMESTAMPTZ,
    last_error    TEXT NOT NULL DEFAULT '',
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at       TIMESTAMPTZ,
    UNIQUE (alert_id, event, sequence, channel)
);

CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due
    ON notification_deliveries(status, scheduled_at, lease_until);

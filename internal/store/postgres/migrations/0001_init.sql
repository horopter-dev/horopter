CREATE TABLE IF NOT EXISTS event (
    id               TEXT PRIMARY KEY,
    tenant           TEXT        NOT NULL,
    idempotency_key  TEXT,
    schema_version   INTEGER     NOT NULL,
    source           TEXT        NOT NULL,
    type             TEXT        NOT NULL,
    subject          TEXT        NOT NULL,
    actor            TEXT        NOT NULL DEFAULT '',
    title            TEXT        NOT NULL DEFAULT '',
    occurred_at      TIMESTAMPTZ NOT NULL,
    received_at      TIMESTAMPTZ NOT NULL,
    correlation_ref  TEXT        NOT NULL DEFAULT '',
    attributes       JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS ix_event_tenant_occurred ON event (tenant, occurred_at);
CREATE INDEX IF NOT EXISTS ix_event_tenant_corr     ON event (tenant, correlation_ref);
CREATE UNIQUE INDEX IF NOT EXISTS uq_event_idem
    ON event (tenant, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS change (
    tenant         TEXT        NOT NULL,
    ref            TEXT        NOT NULL,
    subject        TEXT        NOT NULL,
    type           TEXT        NOT NULL,
    title          TEXT        NOT NULL DEFAULT '',
    actor          TEXT        NOT NULL DEFAULT '',
    start_at       TIMESTAMPTZ NOT NULL,
    end_at         TIMESTAMPTZ,
    end_effective  TIMESTAMPTZ NOT NULL,
    status         TEXT        NOT NULL,
    attributes     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    last_event_id  TEXT        NOT NULL,
    PRIMARY KEY (tenant, ref)
);

CREATE INDEX IF NOT EXISTS ix_change_overlap ON change (tenant, start_at, end_effective);
CREATE INDEX IF NOT EXISTS ix_change_subject ON change (tenant, subject);
CREATE INDEX IF NOT EXISTS ix_change_status  ON change (tenant, status);

CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER     NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    singleton  BOOLEAN     NOT NULL DEFAULT TRUE,
    CONSTRAINT schema_version_singleton UNIQUE (singleton)
);

INSERT INTO schema_version (version) VALUES (1)
ON CONFLICT (singleton) DO UPDATE SET version = 1, applied_at = now();

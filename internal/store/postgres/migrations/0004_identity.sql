CREATE TABLE tenant (
    id           TEXT        PRIMARY KEY,
    display_name TEXT        NOT NULL,
    customer_id  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE principal (
    id           TEXT        PRIMARY KEY,
    display_name TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE role (
    name         TEXT   PRIMARY KEY,
    capabilities TEXT[] NOT NULL
);

INSERT INTO role (name, capabilities) VALUES
    ('producer',     ARRAY['append']),
    ('reader',       ARRAY['read']),
    ('tenant_admin', ARRAY['read', 'admin']),
    ('superuser',    ARRAY['append', 'read', 'admin', 'issue']);

CREATE TABLE credential (
    id              TEXT        PRIMARY KEY,
    principal_id    TEXT        NOT NULL REFERENCES principal (id),
    type            TEXT        NOT NULL,
    key_prefix      TEXT        NOT NULL UNIQUE,
    key_hash        TEXT        NOT NULL,
    role            TEXT        NOT NULL REFERENCES role (name),
    tenant_id       TEXT        REFERENCES tenant (id),
    source          TEXT        NOT NULL,
    locus           TEXT        NOT NULL,
    subject_pattern TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    CONSTRAINT credential_type_vocabulary CHECK (type IN ('machine_key', 'oidc')),
    CONSTRAINT credential_locus_no_slash  CHECK (locus !~ '/' AND locus <> ''),
    CONSTRAINT credential_admin_has_no_subject_pattern
        CHECK (subject_pattern IS NULL OR role NOT IN ('tenant_admin', 'superuser'))
);

CREATE INDEX ix_credential_prefix ON credential (key_prefix);

ALTER TABLE event  ADD COLUMN origin TEXT    NOT NULL DEFAULT 'process',
                   ADD COLUMN edited BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE event  ADD CONSTRAINT event_origin_vocabulary CHECK (origin IN ('human', 'process'));
ALTER TABLE change ADD COLUMN edited BOOLEAN NOT NULL DEFAULT FALSE;

INSERT INTO tenant (id, display_name)
SELECT 'tnt_' || replace(gen_random_uuid()::text, '-', ''), t
FROM (SELECT DISTINCT tenant AS t FROM event
      UNION SELECT DISTINCT tenant FROM change) AS distinct_tenants;

UPDATE event  e SET tenant = tn.id FROM tenant tn WHERE tn.display_name = e.tenant;
UPDATE change c SET tenant = tn.id, ref = 'legacy/' || c.ref
                FROM tenant tn WHERE tn.display_name = c.tenant;

-- No FK on event.tenant/change.tenant here: nothing yet guarantees a write's tenant was
-- provisioned first. 0005 adds it once Task 5 makes tenant credential-derived.

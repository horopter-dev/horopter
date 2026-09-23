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
    -- Mirrors auth.ValidateLocus clause for clause, so a direct INSERT cannot store a locus
    -- horoctl would have refused. No null-byte clause: TEXT cannot hold one, so the column
    -- type already rejects it. locus <> 'legacy' mirrors ValidateLocus's reservation of that
    -- literal name: this migration prefixes every pre-existing ref with legacy/ below, and a
    -- credential issued with that locus would write into the same backfilled namespace.
    CONSTRAINT credential_locus_shape CHECK (
        locus <> ''
        AND locus <> 'legacy'
        AND locus !~ '/'
        AND octet_length(locus) <= 64
        AND locus !~ '^[[:space:]]|[[:space:]]$'),
    -- A BACKSTOP, not the authority, and narrower than horoctl's own rule: horoctl refuses a
    -- subject pattern on any role whose capabilities reach beyond append, because a read is
    -- just as unnarrowed by subject as a rebuild is. A column constraint cannot read the role
    -- table without a trigger, so only the admin case is enumerated here — the one where a
    -- mismatched credential would otherwise mint cleanly and then never authenticate at all,
    -- since internal/auth's principal() refuses it outright. horoctl reads role.capabilities
    -- instead of naming roles, which is what keeps issue time and request time in agreement
    -- as the vocabulary grows.
    CONSTRAINT credential_admin_has_no_subject_pattern
        CHECK (subject_pattern IS NULL OR role NOT IN ('tenant_admin', 'superuser'))
);

CREATE INDEX ix_credential_prefix ON credential (key_prefix);

ALTER TABLE event  ADD COLUMN origin TEXT    NOT NULL DEFAULT 'process',
                   ADD COLUMN edited BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE event  ADD CONSTRAINT event_origin_vocabulary CHECK (origin IN ('human', 'process'));
-- change.edited is carried unpopulated for Area B's corrections, as tenant.customer_id is
-- carried for Tier 2: nothing writes or reads it in this build, and core.Change has no
-- field for it. It goes in now because it rides a breaking window a later one would have to
-- reopen.
ALTER TABLE change ADD COLUMN edited BOOLEAN NOT NULL DEFAULT FALSE;

INSERT INTO tenant (id, display_name)
SELECT 'tnt_' || replace(gen_random_uuid()::text, '-', ''), t
FROM (SELECT DISTINCT tenant AS t FROM event
      UNION SELECT DISTINCT tenant FROM change) AS distinct_tenants;

UPDATE event  e SET tenant = tn.id FROM tenant tn WHERE tn.display_name = e.tenant;
UPDATE change c SET tenant = tn.id FROM tenant tn WHERE tn.display_name = c.tenant;

-- Both halves of a ref move together, or the projection stops being a function of the log:
-- core.RefFor derives a correlated change's ref from event.correlation_ref, so prefixing
-- change.ref alone would have RebuildProjection re-insert every change under the bare ref
-- and silently undo this. A POINT change is the exception here and must stay bare: its
-- events carry correlation_ref = '' permanently, RefFor yields the event id, so a prefixed
-- row would be orphaned by the first rebuild. Matching change.ref against a correlated
-- event is what tells the two apart, and it has to run BEFORE correlation_ref is rewritten
-- or it matches nothing.
--
-- That exception is superseded by 0006, which adds event.locus and prefixes the point
-- changes left bare here — RefFor was changed to compose a point's ref from locus instead
-- of yielding the bare event id. The statement above holds only as of this migration.
UPDATE change c SET ref = 'legacy/' || c.ref
 WHERE EXISTS (SELECT 1 FROM event e
                WHERE e.tenant = c.tenant AND e.correlation_ref = c.ref);
UPDATE event SET correlation_ref = 'legacy/' || correlation_ref WHERE correlation_ref <> '';

-- No FK on event.tenant/change.tenant here: nothing yet guarantees a write's tenant was
-- provisioned first. 0005 adds it once Task 5 makes tenant credential-derived.

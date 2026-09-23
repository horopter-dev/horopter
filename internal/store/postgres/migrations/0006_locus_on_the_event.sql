-- The locus is a property of the event, not a denormalised copy of the appending
-- credential's field. core.RefFor composes a point change's ref from it, so it has to be
-- readable from the log: prefixing the projection alone would make RebuildProjection
-- recreate every point change unprefixed, which is the failure 0004 shipped with.

-- Added nullable and backfilled before SET NOT NULL, so an incomplete backfill aborts the
-- migration's transaction instead of silently defaulting. No DEFAULT survives: 0004 left
-- origin defaulted to 'process', but a write path that forgets a locus must fail rather
-- than quietly claim 'legacy'.
ALTER TABLE event ADD COLUMN locus TEXT;

-- Recovered exactly, not guessed. A locus cannot contain '/', so the part of a stored ref
-- before the first '/' is the locus that composed it — for both 'legacy/' refs 0004 wrote
-- and '<locus>/' refs ingest has written since.
UPDATE event SET locus = split_part(correlation_ref, '/', 1) WHERE correlation_ref <> '';

-- A point event records no locus anywhere, so there is nothing to recover. 'legacy' is the
-- fact rather than a guess: no released build contains 0004 (every tag stops at 0003), so
-- the real migration path is schema 3 to 6, where every pre-existing row predates loci.
UPDATE event SET locus = 'legacy' WHERE correlation_ref = '';

ALTER TABLE event ALTER COLUMN locus SET NOT NULL;

-- Prefix the point changes 0004 deliberately left bare, identified positively: a point
-- change's ref IS the id of one of its tenant's point events. Written this way rather than
-- as the NOT EXISTS complement of 0004:90-92, which needed that test only because both ref
-- shapes were bare at the time. event.id is the primary key, so this matches at most one
-- row: no fan-out and no double-prefixing. A correlated c.ref carries a '/' and an
-- event.id never does, so the join cannot reach one.
UPDATE change c SET ref = e.locus || '/' || c.ref
  FROM event e
 WHERE e.tenant = c.tenant
   AND e.id = c.ref
   AND e.correlation_ref = '';

-- Backstops, not the authority, and narrower than auth.ValidateLocus, which also reserves
-- 'legacy', caps length and rejects edge whitespace. 'legacy' cannot be reserved here: it
-- is exactly what the backfill above writes. strpos(...) > 1 rather than ~ '/' so a leading
-- slash is rejected too — '/deploy-1' has a slash but no locus, and an empty locus is not
-- a locus.
ALTER TABLE event  ADD CONSTRAINT event_locus_shape
    CHECK (locus <> '' AND locus !~ '/');
ALTER TABLE event  ADD CONSTRAINT event_corr_ref_has_locus
    CHECK (correlation_ref = '' OR strpos(correlation_ref, '/') > 1);
ALTER TABLE change ADD CONSTRAINT change_ref_has_locus
    CHECK (strpos(ref, '/') > 1);

-- Filtering changes by locus, source and attributes. Three filters, three different costs;
-- horopter-internal docs/superpowers/specs/2026-09-24-change-filters-design.md records why.

-- Locus is already in the data: it is everything before a ref's first "/", and
-- event_locus_shape forbids a locus containing one, so split_part recovers it exactly.
-- Generated rather than backfilled, so it cannot drift from the ref it derives from and a
-- rebuild has nothing to reproduce. 0003 sets the precedent for start_at and end_effective.
-- change_ref_has_locus (0006) guarantees strpos(ref,'/') > 1, so this is never empty.
ALTER TABLE change ADD COLUMN locus TEXT NOT NULL
    GENERATED ALWAYS AS (split_part(ref, '/', 1)) STORED;

-- Source is descriptive and follows core.Fold's last-wins rule, as subject and type do, so
-- it is a real column rather than a derived one. The DEFAULT exists because a column added
-- to a live table needs one; every write from here sets it explicitly.
ALTER TABLE change ADD COLUMN source TEXT NOT NULL DEFAULT '';

-- Backfilled from each ref's LAST event by id, which is what the fold rule produces:
-- core.Fold sets out.LastEventID = ev.ID on every event, so last_event_id already names the
-- event whose values won. Joining on correlation_ref instead would populate a
-- reasonable-looking column that only a rebuild disagrees with — 0004's defect exactly, and
-- the reason 0006 was needed.
UPDATE change c SET source = e.source
  FROM event e
 WHERE e.tenant = c.tenant
   AND e.id = c.last_event_id;

CREATE INDEX ix_change_locus  ON change (tenant, locus);
CREATE INDEX ix_change_source ON change (tenant, source);

-- One expression index per allowlisted attribute key. postgres.FilterableAttributeKeys is
-- the Go half of this pair; adding a key means adding both, which is the point.
CREATE INDEX ix_change_attr_environment ON change
    (tenant, (attributes ->> 'deployment.environment.name'));

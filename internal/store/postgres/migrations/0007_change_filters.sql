-- Filtering changes by locus, source and attributes. Three filters, three different costs:
-- locus is derived from data the row already holds, sources is a new column backfilled as a
-- union, and attributes need an index per allowlisted key. Each section below says why.

-- Locus is already in the data: it is everything before a ref's first "/", and
-- event_locus_shape forbids a locus containing one, so split_part recovers it exactly.
-- Generated rather than backfilled, so it cannot drift from the ref it derives from and a
-- rebuild has nothing to reproduce. 0002 set the precedent with end_effective and 0003
-- with start_at.
-- change_ref_has_locus (0006) guarantees strpos(ref,'/') > 1, so this is never empty.
ALTER TABLE change ADD COLUMN locus TEXT NOT NULL
    GENERATED ALWAYS AS (split_part(ref, '/', 1)) STORED;

-- A source is a property of an EVENT, and one ref is designed to span credentials with
-- different sources (a change CI starts and a cluster controller completes), so a change has
-- a SET of them. Union rather than last-wins: a union is commutative, so a rebuild in any
-- order reproduces it exactly, and core.Fold's other aggregate fields (first_seen_at's min,
-- the attribute merge) already work this way. capabilities TEXT[] in 0004 is the array precedent.
ALTER TABLE change ADD COLUMN sources TEXT[] NOT NULL DEFAULT '{}';

-- The ref an event folds onto is core.RefFor's rule: its correlation_ref, or its locus and
-- its own id when it has none. This CASE is that rule in SQL, so the backfill covers BOTH
-- change shapes. Joining on correlation_ref alone would leave every POINT change empty while
-- a rebuild filled it -- 0004's defect exactly, and the reason 0006 was needed.
--
-- DISTINCT and ORDER BY are both load-bearing: core.Fold produces a sorted, deduplicated
-- slice, and TestMigrationsLeaveTheProjectionRebuildable compares whole rows as JSON, so an
-- array in a different order is a different value.
--
-- COLLATE "C" on both, because DISTINCT and ORDER BY must agree on collation and slices.Sort
-- orders by byte value, not by this database's default collation (en_US.utf8, which sorts
-- "GitHub" after "buildkite" and "github"; "C" sorts it first, matching Go's byte order,
-- where uppercase letters precede lowercase ones). Without it, a ref whose sources differ in
-- case or punctuation position backfills in one order and rebuilds in another -- the same
-- set, a different array, which is a different row to
-- TestMigrationsLeaveTheProjectionRebuildable's to_jsonb comparison.
UPDATE change c SET sources = s.sources
  FROM (
    SELECT tenant,
           CASE WHEN correlation_ref <> '' THEN correlation_ref
                ELSE locus || '/' || id END AS ref,
           array_agg(DISTINCT source COLLATE "C" ORDER BY source COLLATE "C") AS sources
      FROM event
     GROUP BY 1, 2
  ) s
 WHERE s.tenant = c.tenant
   AND s.ref = c.ref;

CREATE INDEX ix_change_locus ON change (tenant, locus);

-- GIN, because the query is containment (sources @> ARRAY[$n]) rather than equality. The
-- tenant predicate is served separately; a composite (tenant, sources) would need btree_gin,
-- an extension this schema does not otherwise require.
CREATE INDEX ix_change_sources ON change USING GIN (sources);

-- One expression index per allowlisted attribute key. postgres.FilterableAttributeKeys is
-- the Go half of this pair; adding a key means adding both, which is the point.
CREATE INDEX ix_change_attr_environment ON change
    (tenant, (attributes ->> 'deployment.environment.name'));

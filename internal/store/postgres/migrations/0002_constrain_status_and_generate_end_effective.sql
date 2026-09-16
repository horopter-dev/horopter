-- Repair, then constrain. Order matters: the CHECK below would otherwise reject rows
-- the shipped code had already written, and this migration would fail on any existing
-- database.
--
-- A correlated event that was neither a start nor a terminal type left status empty:
-- internal/core/fold.go now defaults it to in_progress. Every such row has end_at NULL,
-- so in_progress is exactly what the corrected fold produces for them. Only the empty
-- status is repaired. Any other value outside the vocabulary is data this migration
-- must not guess at, and the CHECK will refuse it loudly rather than rewrite it.
UPDATE change SET status = 'in_progress' WHERE status = '';

-- The vocabulary is internal/core's Status constants (internal/core/change.go). An empty
-- status made a change both unqueryable —
-- QueryChanges reads an empty Status as "no filter" — and, with end_at NULL, an open
-- interval matching every overlap query until the year 9999.
ALTER TABLE change ADD CONSTRAINT change_status_vocabulary
    CHECK (status IN ('occurred', 'in_progress', 'completed', 'failed',
                      'rolled_back', 'cancelled'));

-- end_effective is the overlap index's key and was computed in Go, so two writers owned
-- one derived fact and could disagree. Postgres owns it now, which makes drift between
-- end_at and the index key impossible. The literal is core.OpenSentinel; its explicit Z
-- offset is what keeps the expression immutable enough for a generated column and
-- independent of the session TimeZone.
--
-- Converting an existing column to generated requires dropping and re-adding it. That
-- also drops ix_change_overlap, which depends on the column, so it is recreated below.
-- The re-add rewrites the table under an ACCESS EXCLUSIVE lock for its duration.
ALTER TABLE change DROP COLUMN end_effective;
ALTER TABLE change ADD COLUMN end_effective TIMESTAMPTZ NOT NULL GENERATED ALWAYS AS
    (COALESCE(end_at, TIMESTAMPTZ '9999-12-31 23:59:59Z')) STORED;
CREATE INDEX ix_change_overlap ON change (tenant, start_at, end_effective);

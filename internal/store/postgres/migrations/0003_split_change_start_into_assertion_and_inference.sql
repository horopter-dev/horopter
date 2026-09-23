-- start_at held two different kinds of claim in one column: a producer's ASSERTION that
-- the change began at T (a start-type event's occurred_at), and an INFERENCE that the
-- change must have existed by T because an event referencing it exists. Any precedence
-- rule written over one column has to destroy one of them, and which one it destroys
-- changes with fold order. Recording both facts removes the dilemma instead of resolving
-- it.
ALTER TABLE change ADD COLUMN started_at    TIMESTAMPTZ;
ALTER TABLE change ADD COLUMN first_seen_at TIMESTAMPTZ;

-- Existing rows cannot be backfilled truthfully from the projection alone: whether a
-- stored start_at came from an assertion or an inference is exactly the information the
-- single-column model destroyed. first_seen_at = start_at with started_at left NULL
-- encodes "we do not know" rather than guessing, which is the only honest answer a system
-- of record can give here. Correct values for existing rows need a replay of the event
-- log: horoctl-migrate --rebuild-tenant=<id>.
UPDATE change SET first_seen_at = start_at;

ALTER TABLE change ALTER COLUMN first_seen_at SET NOT NULL;

-- start_at becomes derived, the same shape end_effective took in 0002: Postgres owns the
-- derived value, the domain owns the facts, and two writers of one fact cannot drift
-- apart because there is only one writer. The COALESCE is what makes the assertion beat
-- the inference structurally, rather than through a branch ordering a later edit can get
-- wrong.
--
-- Converting an existing column to generated requires dropping and re-adding it, which
-- drops everything depending on the column. ix_change_overlap (tenant, start_at,
-- end_effective) is the only such dependency — checked against pg_indexes, pg_constraint
-- and pg_views on a migrated database: no view, no CHECK and no foreign key mentions
-- start_at, and the implicit NOT NULL is reinstated by the new column — so it is the only
-- object recreated below. The re-add rewrites the table under an ACCESS EXCLUSIVE lock for
-- its duration: instant on a small table, an outage window on a large one. v0.1 has no
-- zero-downtime migration story.
ALTER TABLE change DROP COLUMN start_at;
ALTER TABLE change ADD COLUMN start_at TIMESTAMPTZ NOT NULL GENERATED ALWAYS AS
    (COALESCE(started_at, first_seen_at)) STORED;
CREATE INDEX ix_change_overlap ON change (tenant, start_at, end_effective);

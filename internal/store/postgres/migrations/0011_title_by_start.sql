-- A change is titled by its earliest titled start event (core.Fold's foldTitle).
--
-- title_started_at is that start's occurred_at, NULL when no start event had a title and the
-- title came from the fallback rule. The fold needs it to compare an arriving start against
-- the one it holds, which is what keeps the title independent of arrival order.
--
-- The backfill applies the same rule to every existing change, so a rebuild straight after the
-- migration changes nothing:
--   * a start is core.classify's: lifecycle 'started', or no lifecycle and a type ending in
--     .started or .begun (lifecycle 'none' asserts nothing, whatever the type says);
--   * earliest by occurred_at, a tie going to the smaller title in byte order (COLLATE "C"),
--     which is Go's string comparison;
--   * the ref is core.RefFor's, as in 0007 and 0010.
ALTER TABLE change ADD COLUMN title_started_at TIMESTAMPTZ;

UPDATE change c SET title = s.title, title_started_at = s.at
  FROM (
    SELECT DISTINCT ON (tenant, ref) tenant, ref, title, at
      FROM (
        SELECT tenant,
               CASE WHEN correlation_ref <> '' THEN correlation_ref
                    ELSE locus || '/' || id END AS ref,
               title, occurred_at AS at
          FROM event
         WHERE title <> ''
           AND (lifecycle = 'started'
                OR (lifecycle = '' AND (type LIKE '%.started' OR type LIKE '%.begun')))
      ) starts
     ORDER BY tenant, ref, at, title COLLATE "C"
  ) s
 WHERE s.tenant = c.tenant
   AND s.ref = c.ref;

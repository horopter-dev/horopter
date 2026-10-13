-- Each change's subjects, with when each first appeared, for subject-pattern filtering.
--
-- A subject is in a change's scope when it first appeared no later than the change's end, or
-- at any time while the change is open. That bound is applied when reading, not folded into
-- a stored set: an out-of-order older outcome moves the end earlier, and a set accumulated
-- against the old end would keep subjects the new one excludes. Storing each subject's
-- earliest time lets the query decide against whatever the end is now.
--
-- A map from subject to the minimum occurred_at of that subject's events, in Unix
-- microseconds: the precision occurred_at is stored at, so core.Fold's UnixMicro and this
-- backfill agree exactly. The ref rule is core.RefFor's, as in 0007, so point changes are
-- backfilled too. jsonb normalises key order, so no ORDER BY is needed for a rebuild to
-- reproduce the row.
ALTER TABLE change ADD COLUMN subject_first_seen JSONB NOT NULL DEFAULT '{}';

UPDATE change c SET subject_first_seen = s.m
  FROM (
    SELECT tenant, ref, jsonb_object_agg(subject, first_us) AS m
      FROM (
        SELECT tenant,
               CASE WHEN correlation_ref <> '' THEN correlation_ref
                    ELSE locus || '/' || id END AS ref,
               subject,
               min((extract(epoch FROM occurred_at) * 1000000)::bigint) AS first_us
          FROM event
         WHERE subject <> ''
         GROUP BY 1, 2, 3
      ) per_subject
     GROUP BY 1, 2
  ) s
 WHERE s.tenant = c.tenant
   AND s.ref = c.ref;

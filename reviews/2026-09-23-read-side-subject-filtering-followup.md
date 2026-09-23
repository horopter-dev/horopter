# Follow-up: read-side subject filtering

Raised by the pre-push review that produced commits on
`worktree-identity-and-authorization` on 2026-09-23 (Finding 1, CRITICAL). Recorded here
per that finding's ruling, so the option is not lost.

## What this branch did instead

`subject_pattern` restricted a credential's writes but was read by nothing in
`internal/egress` — every read route (`GET /v1/changes`, `GET /v1/events`,
`GET /adapters/grafana/annotations`, `GET /adapters/otlp/spans`) answered with every
subject in the tenant regardless of the pattern on the credential making the request. The
fix applied here was issue-time only: `checkRoleAllowsSubjectPattern` in
`cmd/horoctl/keys.go` now refuses a subject pattern on any role whose capabilities reach
beyond append, so a `read`-capable credential can no longer be issued with a pattern in
the first place. No read route was touched.

## What is still missing

Nothing narrows a read by subject today. A `reader` credential — correctly issued without
a pattern, since one is refused now — still sees every subject in its tenant. If a
narrower read (e.g. "this reader may only see `team-a/*`") becomes a real requirement,
that is a feature with its own design surface:

- **Query**: `WHERE subject LIKE ...` or similar per read handler, applied only when the
  requesting credential carries a pattern.
- **Index**: an unindexed subject filter on `change`/`event` would force a sequential
  scan under tenant-wide read traffic; the pattern syntax (glob? prefix? something
  stricter) determines whether a btree prefix index, a trigram index, or something else
  is the right shape.
- **API**: whether the filter is silent (the credential's pattern narrows results with no
  visible parameter) or explicit (a query parameter the caller can also narrow further,
  validated against the credential's own pattern as a ceiling) is a real design choice
  with different caller-visible behavior.
- **Adapters**: `adapters/grafana/annotations` and `adapters/otlp/spans` would need the
  same treatment as the native routes, not just `/v1/changes` and `/v1/events`.

None of this is designed yet. Do not implement it speculatively — it is not needed until
a real credential requires a narrowed read, per this codebase's "no speculative
features" rule — but when it is needed, this document is the starting point: the
capability-based issue-time check stays exactly as it is (append-only roles may still
carry a pattern), and this becomes the second enforcement point, symmetric with how
writes are already checked in `internal/ingress`.

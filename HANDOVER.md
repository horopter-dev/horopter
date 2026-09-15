# HANDOVER — Horopter v0.1, fix wave in progress

**Temporary file. Delete it when the fix wave is done and the branch is merged.**

Written 2026-09-15. Branch `feat/v0.1`, HEAD `d84336c`.

**Two files are UNTRACKED and not in any commit — do not lose them:** this file, and
`decisions/2026-09-15-change-start-time-provenance.md`. Robin was not asked to commit them.
Offer to commit them early in the next session. Never run `git clean -fdx` here: it would
destroy both, plus the gitignored `.superpowers/` workspace holding the ledger, every task
brief and report, and the review packages.

---

## Read these first, in this order

1. This file.
2. `.superpowers/sdd/2026-09-14-horopter-v0.1/progress.md` — the SDD ledger. It is the
   authoritative record and it is long; the commits it names exist in git even where this
   file is terser. **Read its last ~150 lines**, from the line
   `## FINAL REVIEW RESULT (opus)` onward.
3. `.superpowers/sdd/2026-09-14-horopter-v0.1/final-review-findings.md` — every finding from
   the final whole-branch review, with file:line and fix guidance. This is the work list.
4. `decisions/2026-09-15-projection-write-concurrency.md` and
   `decisions/2026-09-15-change-start-time-provenance.md` — two decisions Robin made.
   **Both are settled. Implement them; do not reopen them.**

The governing skill is `superpowers:subagent-driven-development`. Invoke it before doing
anything else.

---

## Where the project got to

All 14 planned tasks are complete and reviewed. The final whole-branch review then ran on
Opus over 36 commits and returned **NOT READY TO MERGE — 2 Critical, 10 Important, ~18
Minor**. Robin chose to fix **everything the review raised**, with one exception (egress,
below).

The fix wave is split into **four sequential batches**. This is a deliberate deviation from
the skill's "one fix subagent" rule, recorded in the ledger: 30 findings across ~15 files
including two schema migrations is not one edit, and one agent would produce a partial diff.
Four context builds, not thirty. Sequential because the batches share files.

- **Batch A — store correctness: DONE** (commit `d84336c`), reviewed, all A1–A8 addressed.
- **Batch B — HTTP contract: NOT STARTED. This is your next job.**
- **Batch C — adapters: not started.**
- **Batch D — infra/hygiene: not started.**

---

## Environment — check this before anything else

    docker compose ps          # db must be Up (healthy)
    docker compose up -d db    # if not

Run the store tests with the DSN exported, or they **skip silently and still print `ok`**:

    export HOROPTER_TEST_DSN='postgres://horopter:horopter@localhost:5432/horopter'
    go test -race -count=1 ./...

`ok` alone does **not** mean the tests ran. This project has been fooled by that before. Use
`-v` and count PASS lines when it matters.

**Never run `docker compose down` or `task down`** — they destroy the volume holding the
migrated schema.

Baseline at HEAD `d84336c`, all verified by me:

- `go test -race -count=1 ./...` → 113 PASS, 0 FAIL, 0 SKIP
- `golangci-lint run ./...` → exit 0, "0 issues" (golangci-lint 2.13.2, installed via brew)
- `gofmt -l .` → empty
- `schema_version` = 2; `applied_migration` holds `0001_init.sql` and
  `0002_constrain_status_and_generate_end_effective.sql`

---

## Robin's decisions — settled, do not re-litigate

1. **C1 concurrency fix = advisory lock on the ref.** Implemented in batch A. See
   `decisions/2026-09-15-projection-write-concurrency.md`.
2. **Fix scope = everything the final review raised.**
3. **Egress stays exactly as-is.** Unauthenticated read side, raw store errors still
   relayed. Document the exposure in the README; change no behaviour. This specifically
   overrides "everything" for finding I4 — Robin was asked about egress directly and chose
   leave-as-is.
4. **Change start time = two facts, assertion + inference, `start_at` derived.** See
   `decisions/2026-09-15-change-start-time-provenance.md`. Robin asked for "the most correct
   thing, not the easiest" and this is it. It supersedes part of batch A's A6.

---

## BATCH B — your next job

Write `.superpowers/sdd/2026-09-14-horopter-v0.1/fix-b-brief.md`, dispatch one implementer
(sonnet is probably enough; opus if you judge otherwise), then a scoped review. Record
`BASE` (`git rev-parse HEAD`) before dispatching.

Contents of batch B, in suggested order:

### B1 (Important, REGRESSION from batch A) — `recordSchemaVersion` can silently downgrade

`internal/store/postgres/postgres.go:137` and `:221-233`. It derives the version from
`names[len(names)-1]` — the highest migration **the binary embeds** — and writes it with an
unconditional `DO UPDATE SET version = $1`. An older `horopter-migrate` run against a
newer database rewrites `schema_version` **downward** while the ledger still holds every
applied file, so the recorded version contradicts the database's actual structure.

**I proved this empirically.** Hiding `0002` from the embedded FS and running migrate took
`schema_version` from 2 to 1 while `applied_migration` still held 2 rows. Restored
afterwards; tree clean.

Fix: derive from the ledger the database actually holds —
`SELECT max(filename) FROM applied_migration` — after which an old binary's own
`ExpectedSchemaVersion` check fails loudly, which is the wanted behaviour.

### B2 — the start-time provenance model

Implement `decisions/2026-09-15-change-start-time-provenance.md`: `started_at` (nullable
assertion), `first_seen_at` (inference, running min), `start_at` becomes
`GENERATED ALWAYS AS (COALESCE(started_at, first_seen_at)) STORED` in a `0003` migration.
Correct the `fold.go` comment that claims an order-insensitivity the code does not have
(see B6). Note the truthful backfill for existing rows is
`first_seen_at = start_at, started_at = NULL`; correct values need a rebuild (B4).

### B3 (C2, CRITICAL) — every store failure returns `400 Bad Request`

`internal/ingress/server.go:126-131`. Validation errors, connection failures, commit
failures and constraint violations all answer `400` with the raw error string. Producers
treat 4xx as "do not retry", so a transient database blip permanently loses every event
posted in that window. Also leaks internal error text to callers.

Fix: `var ErrInvalidEvent = errors.New(...)` in `internal/core`, wrapped by each validation
failure; `errors.Is(err, core.ErrInvalidEvent)` → 400, everything else → 503 with a generic
message and the detail in a server-side log line.

Came from the plan, not from an implementer.

### B4 (I6) — `RebuildProjection` has no operator surface

Add `horopter-migrate --rebuild-tenant=<id>`. It is the escape hatch for every fold bug, and
B2's truthful backfill needs it.

**Before shipping this, read the batch A review's operational warning** (in the ledger):
the tenant-wide exclusive lock means every append blocks for the whole duration of a
rebuild, each blocked appender pinning a pool connection, and the pool is shared across
tenants — so a long rebuild can stall ingress for *unrelated* tenants. There is no timeout,
backoff or batching. Do not ship the CLI without thinking this through.

### B5 (I2, I3) — smaller contract fixes

- **I2**: `internal/ingress/cloudevents.go:85` uses the CloudEvents `id` alone as the
  idempotency key. CloudEvents only guarantees `source` + `id` unique, so two sources
  sharing an id means the second, genuinely distinct event is silently swallowed and
  answered `200 {"created": false}`. Fix: `ce.Source() + "/" + ce.ID()`.
- **I3**: `internal/egress/server.go:115-139` clamps `limit` to 1000 and returns it as if
  honoured; ordering is `start_at ASC`, so truncation drops the **newest** changes — during
  an incident, exactly the ones that matter. Fix: signal truncation (header or envelope
  flag); consider `DESC` for the truncated case. Keep the cap itself.

### B6 — comments that assert what the code does not do

The batch A review found three more. This codebase has a persistent pattern of confidently
wrong comments, and one of them (`ON CONFLICT` "resolves the insert race") is very likely
why the C1 bug went untested for the whole run. Treat these as real:

- `internal/core/fold.go:82-84` claims the earliest-start rule is order-insensitive. The
  `min` branch is; the resulting `Start` is not, because `isStartType` still assigns
  unconditionally. B2 changes this code — make the comment true afterwards.
- `internal/store/postgres/postgres.go:674-675` lists the fields a rebuild can diverge on
  (Title, Type, Actor, attributes, LastEventID) and omits `Start`.
- `internal/egress/server.go:209-210` says `listEvents` is "optionally scoped to one
  correlation ref"; after batch A it scopes to a change's ref — a correlation ref *or* a
  point's own event id.
- `internal/store/postgres/append_test.go:24-26` names two package-level hooks as the reason
  tests cannot be parallel; batch A added a third, `afterDeleteHook`.

### B7 — smaller items

- `AppendEvent` cyclomatic complexity is ~13 against Robin's stated hard limit of ≤8.
  Restructure it. (A complexity linter is batch D, so the limit is enforced not remembered.)
- `internal/ingress/server.go:55-64`: with `s.token == ""`,
  `subtle.ConstantTimeCompare([]byte(""), []byte(""))` returns 1, so a request with **no**
  `Authorization` header authenticates. `main.go` prevents this in production but the guard
  belongs in `ingress.New` so the safety is structural.
- `internal/ingress/server.go:98`, `cloudevents.go:21`: no `http.MaxBytesReader`;
  `io.ReadAll` on an unbounded body. No `ReadTimeout`/`WriteTimeout`/`IdleTimeout` on either
  server (only `ReadHeaderTimeout`).
- `core.Change.EndEffective()` has had no production caller since batch A made
  `end_effective` a generated column. Per "replace, don't deprecate": delete it, or state in
  its doc comment that the store no longer uses it.
- Two test-quality gaps the batch A review identified, both of which can silently stop
  testing what they name (details in the ledger):
  `TestConcurrentDuplicateIdempotencyKeyReturnsTheOriginal` would still pass if the
  `afterIdemLookupHook` call site were deleted — add an `atomic.Int64` counter and assert it
  was called twice; and `TestListEventsFindsAPointChangesEvents` does not pin the
  `correlation_ref = ''` qualifier, so loosening the predicate breaks no test.

---

## BATCH C — adapters (after B)

- **I8**: `internal/adapter/otlp` does not emit OTLP. It returns
  `{"TraceID":…,"StartUnixNano":…}` — neither OTLP/protobuf nor OTLP/JSON (which nests
  `resourceSpans[].scopeSpans[].spans[]`, spells fields `traceId`, `startTimeUnixNano`, and
  encodes nanos as decimal *strings*). No collector can consume it. **Hand-roll the JSON
  shape — do not add `go.opentelemetry.io/otel`**; Robin's standards require justifying each
  dependency and this is just nested JSON. Also: `otlp.Span` has no `json` struct tags at
  all, so its public field names are whatever the Go identifiers happen to be.
- **I10**: Grafana's SimpleJson and JSON API datasource plugins request annotations via
  `POST /annotations` with a JSON body; the endpoint is a `GET` with query params. The
  renderer is unchanged either way — add the POST route. Note nothing has ever been pointed
  at a real Grafana; that verification is still outstanding and Robin should know it.
- `otlp.go:59`: `SpanID` derives from `Ref` alone while `TraceID` includes the tenant, so
  two tenants sharing a ref get identical span ids. Include the tenant in both.
- `grafana.go:73-78`: `textFor` reads an attribute key `description` that appears nowhere in
  the spec's §5.4 vocabulary. Document it or drop it.

## BATCH D — infra/hygiene (last)

- **I9, do this first — the quality gate has been theatre all run.** The pre-commit
  `go-test` hook runs without `HOROPTER_TEST_DSN` and without `-race`, so the store package,
  all of ingress and most of egress **skip silently**. What actually runs is `internal/core`,
  the two adapters and three no-database tests. Fix: have the hook call `task test` (it
  already sets the DSN and `-race`). Then add a CI workflow with a Postgres service
  container — there is no CI anywhere in the repo.
- Add a cyclomatic-complexity linter to `.golangci.yml` (Robin's hard limit is ≤8).
- `go mod tidy` — `cloudevents/sdk-go/v2` is marked `// indirect` though directly imported;
  `google/uuid`, `zap`, `multierr` are unused. Confirmed with `go mod tidy -diff`.
- `.dockerignore` missing `dist/` (the repointed `task build` target populates it).
- `.pre-commit-config.yaml:24` is 106 chars, the only line over 100 in the repo.
- `internal/core/event.go:34-61`: "IDs are ULIDs, 26-char TEXT" is a Global Constraint
  enforced nowhere — `Validate` ignores `ID` and the column is bare `TEXT`.
- `version_test.go:26` flips the global `schema_version` to 99; a killed run leaves the
  database at 99 and every daemon then refuses to start. Use the existing
  `isolatedSchemaDSN` helper.
- README: document the open, unauthenticated egress read side (decision 3 above).
- Rename `TestRebuildReproducesProjectionExactly` — batch A retracted the claim its name
  makes.

---

## After all four batches

1. One more whole-branch review on the most capable model, over `d98470e..HEAD`
   (`scripts/review-package` in the skill directory; **there is no `main` branch**, so
   `git merge-base main HEAD` fails — use `d98470e`).
2. Then `superpowers:finishing-a-development-branch`.
3. **Branch topology is Robin's call and has not been made.** There is no `main` and no
   remote — only `docs/design-spec` (d98470e) and `feat/v0.1`. Ask; do not guess.
4. The plan document itself is wrong in five places (listed at the end of
   `final-review-findings.md`) and needs a correction pass before it is used as the record
   of what was built.
5. Delete this file and the plan's `.superpowers/sdd/2026-09-14-horopter-v0.1/` workspace
   once merged — with `trash`, never `rm -rf` (Robin's global standard). Sibling
   directories under `.superpowers/sdd/` belong to other plans; leave them alone.

---

## Things this project keeps getting wrong — read before dispatching anyone

- **`go test` printing `ok` is not proof the tests ran.** Without `HOROPTER_TEST_DSN` the
  store package skips and still prints `ok`. Check PASS counts with `-v`.
- **Guards that assert nothing.** The ledger records repeated instances. Every new guard
  must be mutation-proven: break the thing it protects, watch it fail, restore. Confirm four
  things each time — unique anchor, the changed line via diff, that the mutated code still
  *compiles* (else the failure is a build error, not the assertion), and that the test
  actually RAN.
- **Confidently wrong comments.** Three found in batch A alone, and one of them concealed
  the Critical bug for the whole run. Verify comments against the code they describe.
- **Implementer reports have contained fabricated citations.** Three consecutive early ones.
  Implementers are now forbidden from quoting documents — cite file:line only — and every
  claim must be verified against the diff. Verify report claims yourself; several times this
  session a report was accurate and several times it was not.
- **Do not pre-judge findings for a reviewer.** If you have spotted something, leave it out
  of the prompt and let the reviewer find it independently. That is the confirmation with
  value.

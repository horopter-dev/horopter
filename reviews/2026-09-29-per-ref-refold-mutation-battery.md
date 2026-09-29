# Mutation battery — per-ref re-fold, Task 7

Date: 2026-09-29
Branch: `feat/per-ref-refold`
Commit the battery was run at: `1b5f151476cd3dfbb497d62ecc339da7d1193e3b`
("feat(store): count re-folds and warn when one moves an outcome")

## Why this exists

`reviews/2026-09-23-mutation-battery.md` established the form for this repository: every
mutation is given as an exact before/after edit, applied singly, run against the real
Postgres-backed suite, and reverted before the next one, so the battery can be re-applied
by hand from this document alone rather than from a session transcript.

## Method

1. Working tree confirmed clean (`git status --porcelain`) before the first mutation.
2. Each mutation applied singly, with `Edit`, to the smallest change that reproduces the
   described defect and still compiles.
3. `task test -- ./internal/store/postgres/` (which sets `HOROPTER_TEST_DSN` and runs
   `go test -race -count=1`) run after every mutation — never a bare `go test`, which
   skips the store's tests entirely and would make a "survives" verdict worthless.
4. Reverted with `git checkout -- <file>` before the next mutation. `git status --porcelain`
   confirmed clean before every mutation and after every revert.
5. Mutations 6 and 9, which survived the suite as it stood, got a new test each,
   appended to `internal/store/postgres/refold_test.go`; both mutations were then
   re-applied and re-reverted to confirm the new test is what catches them.
6. At the end: `task check` (`gofmt -l .`, `golangci-lint run ./...`,
   `go test -race -count=1 ./...`) run once more against the untouched tree.

## Correction 1(b) — deriving the correct `End` assertion

The brief's Step 2 code for the mutation-9 test asserted `got.End.Equal(at)` — the
**earlier** of the two timestamps — as the id-ordered answer. The task's correction said
that is backwards and asked for an independent derivation. Working from
`internal/core/fold.go`'s transition table and its own comments:

- `stateClosed`/`assertsTerminal` is `{setAsserted, setsEnd: true, minStart: false,
  commutative: false}` — a terminal event folded onto an **already-closed** change
  overwrites `Status` and `End` again. `refold.go`'s `sameInstant` doc comment says this
  directly: "core.Fold's terminal cell overwrites End every time it fires, so whichever
  terminal event folds LAST sets it."
- Fixture: `early` (lower id, `at`) and `late` (higher id, `at+20m`), both
  `deployment.completed`. Id order folds `early` first — the change is open, so `early`'s
  terminal cell closes it and sets `End = at`. Id order then folds `late` second — the
  change is now closed, `late` is *also* terminal, so `stateClosed`/`assertsTerminal`
  fires again and **overwrites** `End` with `late`'s timestamp, `at+20m`.
- The re-folded (id-ordered) `End` is therefore `at+20m` — the **later** timestamp, not
  the earlier one. `at` alone (the brief's assertion) is what an incremental,
  arrival-order fold would leave if `late` were appended first and `early` folded onto it
  last — i.e., the answer the re-fold exists to *reject*, not the one it produces.

This was verified empirically, not just reasoned: with the corrected assertion
(`got.End.Equal(at.Add(20*time.Minute))`), the test passes against the unmutated store and
fails against mutation 9 with the expected message. The brief's `at` assertion would have
passed under the *mutated* code as well as the correct code (both leave `End` non-nil), so
it would not have caught anything — see the mutation 9 row below.

## Mutation table

| # | file | edit | predicted catcher | actual outcome |
|---|---|---|---|---|
| 1 | `refold.go`, `foldRows` | key the accumulator by `ev.CorrelationRef` instead of `core.RefFor(ev)` | `TestRefoldRefFoldsAPointRefFromItsOwnEvent` | **caught, as predicted** |
| 2 | `refold.go`, `foldEventQuery` | drop `ORDER BY id ASC` | `TestAConvergedRefEqualsAWholeTenantRebuild` | **caught, but not by the predicted test** — see note |
| 3 | `refold.go`, `foldRef` | drop the `(correlation_ref = '' AND id = $3)` arm | `TestRefoldRefFoldsAPointRefFromItsOwnEvent` | **caught, as predicted, plus 14 collateral failures** — see note |
| 4 | `append.go`, `applyToProjection` | make `if !out.outOfOrder` unconditional (`if true`) | `TestAnOutOfOrderAppendConvergesTheRefToIDOrder` | **caught, as predicted** |
| 5 | `append.go`, `refoldOutOfOrderAppend` | `out.foldStatus = folded.Status` instead of `refolded.Status` | none in the brief; `TestAppendRecordsTheReFoldedStatusNotTheIncrementalOne` per correction 1(a) | **caught, as corrected** |
| 6 | `refold.go`, `refoldRef` | return before the `DELETE` when `folded.events == 0` | none — predicted to survive | **survived; new test `TestRefoldRefDeletesAChangeWhoseEventsAreGone` added and confirmed to catch it** |
| 7 | `refold.go`, `foldRef` | pass `ref.String()` where `ref.ProducerRef()` is expected | `TestRefoldRefFoldsAPointRefFromItsOwnEvent` | **caught, as predicted** |
| 8 | `append.go`, `AppendEvent`'s deferred metrics | `len(out.refoldMoved) > 0` instead of `movedSomethingRecorded(out.refoldMoved)` | `TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered` | **caught, as predicted** |
| 9 | `refold.go`, `alteredFields` | compare `end` by `(before.End == nil) != (after.End == nil)` instead of `!sameInstant(...)` | none — predicted to survive | **survived; new test `TestARefoldThatMovesOnlyTheEndTimeIsCountedAsAltered` added (corrected per 1(b)) and confirmed to catch it** |
| 10 | `outcome.go`, `metricAttributes` | add `attribute.StringSlice("horopter.refold.moved", o.refoldMoved)` | none — predicted to survive | **caught immediately by six existing tests** — see note; no new test needed |

All ten mutations are caught in the final state of the tree (six by pre-existing tests
unchanged, one by a pre-existing test per correction 1(a), two by tests added this task,
one by pre-existing tests contrary to its "predicted to survive" label).

## Exact edits, catchers and failure messages

### Mutation 1 — key the accumulator by `ev.CorrelationRef`

`internal/store/postgres/refold.go`, `foldRows`:

```diff
-		ref := core.RefFor(ev)
+		ref := ev.CorrelationRef
```

Caught by `TestRefoldRefFoldsAPointRefFromItsOwnEvent`:

```
Error:      	"[]" should have 1 item(s), but has 0
Test:       	TestRefoldRefFoldsAPointRefFromItsOwnEvent
Messages:   	expected exactly one change at ref "ci/01M3PP2GS7DR34MER1F5YPQXW3"
```

A point event's `CorrelationRef` is the zero ref, so every point keys under the same zero
entry and is folded together instead of under its own `<locus>/<id>` — the change this
test looks for is never written, and `oneChange`'s `require.Len(got, 1)` fails.

### Mutation 2 — drop `ORDER BY id ASC`

`internal/store/postgres/refold.go`, `foldEventQuery`:

```diff
-	return `SELECT ` + foldColumns + ` FROM event WHERE ` + where + ` ORDER BY id ASC`
+	return `SELECT ` + foldColumns + ` FROM event WHERE ` + where
```

**Not** caught by the brief's predicted `TestAConvergedRefEqualsAWholeTenantRebuild` — that
test stayed green. `RebuildProjection`'s whole-tenant replay reads through the same
`foldEventQuery` seam, so both sides of that test's comparison lost ordering together and
it could not tell the difference; separately, a small, single-transaction result set from
this test's fixture size can come back in primary-key order from Postgres regardless of an
explicit `ORDER BY`, which is exactly the kind of accidental agreement an `ORDER BY` clause
exists to not depend on.

Caught instead by three tests that assert a specific id-ordered *answer*, most directly
`TestAppendRecordsTheReFoldedStatusNotTheIncrementalOne`:

```
Error:      	Not equal:
            	expected: []string{"failed", "failed"}
            	actual  : []string{"failed", "completed"}
Test:       	TestAppendRecordsTheReFoldedStatusNotTheIncrementalOne
Messages:   	the second append's incremental fold would have said completed; the span must carry the re-folded answer, failed
```

Also caught by `TestAnOutOfOrderAppendConvergesTheRefToIDOrder` (`expected: "failed"`,
`actual: "completed"`, plus a wrong `last_event_id`) and
`TestARefoldThatMovesAnOutcomeIsCountedAsAltered` (`expected: 1`, `actual: 0` on the
`refoldCounts` "true" bucket).

### Mutation 3 — drop the point-ref arm

`internal/store/postgres/refold.go`, `foldRef`:

```diff
 	rows, err := tx.Query(ctx, foldEventQuery(
-		`tenant = $1 AND (correlation_ref = $2 OR (correlation_ref = '' AND id = $3))`),
+		`tenant = $1 AND correlation_ref = $2`),
 		tenantID, ref.String(), ref.ProducerRef())
```

This leaves the query with two bound placeholders and three arguments, so it fails at
the `pgx` layer on every call, not only the point-ref path — `refoldRef` runs inside every
out-of-order append and every explicit `RefoldRef` call in the suite. 15 tests failed,
including the predicted `TestRefoldRefFoldsAPointRefFromItsOwnEvent`:

```
Error:      	Received unexpected error:
            	read events for ref "ci/01M3PP5M9TZ3XWG7RD1ZNVSBS9" in tenant "test-TestRefoldRefFoldsAPointRefFromItsOwnEvent-dlruaqu7oc6o-1": expected 2 arguments, got 3
Test:       	TestRefoldRefFoldsAPointRefFromItsOwnEvent
```

The predicted test is caught, as the brief said, but the mutation's actual blast radius —
every test that exercises `refoldRef` at all — is far wider than "a point ref now folds to
nothing," because the edit also breaks the query's own arity rather than only its
semantics.

### Mutation 4 — make the in-order branch unconditional

`internal/store/postgres/append.go`, `applyToProjection`:

```diff
 	out.outOfOrder = found && ev.ID < cur.LastEventID
-	if !out.outOfOrder {
+	if true {
 		out.foldStatus = folded.Status
 		out.foldClosed = folded.End != nil
 		return upsertChange(ctx, tx, folded)
```

Caught by `TestAnOutOfOrderAppendConvergesTheRefToIDOrder`, as predicted:

```
Error:      	Not equal:
            	expected: "failed"
            	actual  : "completed"
Test:       	TestAnOutOfOrderAppendConvergesTheRefToIDOrder
Messages:   	id order closes on failed; without the re-fold this is completed
```

with a second assertion in the same test failing on `last_event_id`. Also caught by
`TestAppendRecordsTheReFoldedStatusNotTheIncrementalOne`,
`TestAConvergedRefEqualsAWholeTenantRebuild` and
`TestARefoldThatMovesAnOutcomeIsCountedAsAltered` — `refoldRef` never runs, so a live
append and a whole-tenant rebuild now disagree on this fixture.

### Mutation 5 — report the incremental status, not the re-folded one

`internal/store/postgres/append.go`, `refoldOutOfOrderAppend`:

```diff
-	out.foldStatus = refolded.Status
+	out.foldStatus = folded.Status
 	out.foldClosed = refolded.End != nil
 	out.refoldMoved = alteredFields(folded, refolded)
```

**Correction 1(a) applied**: the brief's own Step 2 would have added a new test
(`TestAConvergingAppendReportsTheRefoldedStatus`) for this row, but Task 5's fix round
already shipped a test doing exactly that job —
`TestAppendRecordsTheReFoldedStatusNotTheIncrementalOne`
(`internal/store/postgres/append_test.go:635`). No new test was written; the existing one
was re-run and confirmed to catch this exact mutation:

```
Error:      	Not equal:
            	expected: []string{"failed", "failed"}
            	actual  : []string{"failed", "completed"}
Test:       	TestAppendRecordsTheReFoldedStatusNotTheIncrementalOne
Messages:   	the second append's incremental fold would have said completed; the span must carry the re-folded answer, failed
```

### Mutation 6 — skip the delete of an events-gone change

`internal/store/postgres/refold.go`, `refoldRef`:

```diff
 	if folded.events == 0 {
-		if _, err := tx.Exec(ctx,
-			`DELETE FROM change WHERE tenant = $1 AND ref = $2`,
-			tenantID, ref.String()); err != nil {
-			return 0, fmt.Errorf(
-				"clear change %q in tenant %q: %w", ref, tenantID, err)
-		}
 		return 0, nil
 	}
```

**Survived** the suite as it stood before this task (`task test -- ./internal/store/postgres/`
ran green with this mutation applied) — as predicted. The delete branch is unreachable
through the store's own API (nothing deletes an event), so nothing in the existing suite
can put a ref into the "had events, now has none" state that would exercise it.

**New test added**, `TestRefoldRefDeletesAChangeWhoseEventsAreGone` in
`internal/store/postgres/refold_test.go`, which reaches that state by deleting the events
directly through the pool rather than through the store's API. Re-applying the mutation
against the new test:

```
Error:      	Should be empty, but was [{test-TestRefoldRefDeletesAChangeWhoseEventsAreGone-dlrufbo1f33c-1 ci/d1 acme/prod deployment.started deployment.started ci-bot 2026-09-29 13:00:00 +0100 BST 2026-09-29 13:00:00 +0100 BST 2026-09-29 13:00:00 +0100 BST <nil> in_progress map[service.version:2.5.0] 01M3PPGJPKER3J0P8ZS9H56AHK [ci]}]
Test:       	TestRefoldRefDeletesAChangeWhoseEventsAreGone
Messages:   	a ref with no events must leave no change behind
```

### Mutation 7 — pass the wrong argument to the point-ref arm

`internal/store/postgres/refold.go`, `foldRef`:

```diff
-		tenantID, ref.String(), ref.ProducerRef())
+		tenantID, ref.String(), ref.String())
```

Caught by `TestRefoldRefFoldsAPointRefFromItsOwnEvent`, as predicted:

```
Error:      	"[]" should have 1 item(s), but has 0
Test:       	TestRefoldRefFoldsAPointRefFromItsOwnEvent
Messages:   	expected exactly one change at ref "ci/01M3PP9ZPP0GQ1Q5Y5D2MX5T14"
```

`ref.String()` (`ci/<id>`) never equals a stored event's own `id`, so the point arm's
`correlation_ref = '' AND id = $3` never matches and the point folds to nothing.

### Mutation 8 — count with a presence check instead of `movedSomethingRecorded`

`internal/store/postgres/append.go`, `AppendEvent`'s deferred metrics block:

```diff
 			s.refolds.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(
 				attribute.Bool("horopter.refold.altered",
-					movedSomethingRecorded(out.refoldMoved)))))
+					len(out.refoldMoved) > 0))))
```

Caught by `TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered`, as predicted:

```
Error:      	Not equal:
            	expected: 1
            	actual  : 0
Test:       	TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered
Messages:   	a re-fold that moved only last_event_id must not be counted as altering
```

```
Error:      	Should be zero, but was 1
Test:       	TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered
```

`refoldMoved` always contains `last_event_id` on an out-of-order re-fold (it moves by
construction — see `movedSomethingRecorded`'s doc comment), so `len(...) > 0` is true on
every single re-fold, making `horopter.refold.altered` true unconditionally — exactly the
cardinality/signal failure `movedSomethingRecorded` exists to prevent.

### Mutation 9 — compare `End` by presence instead of by value

`internal/store/postgres/refold.go`, `alteredFields`:

```diff
-		{"end", !sameInstant(before.End, after.End)},
+		{"end", (before.End == nil) != (after.End == nil)},
```

**Survived** the suite as it stood before this task — as predicted, and confirmed
empirically (`task test -- ./internal/store/postgres/` green with this mutation applied).

**New test added**, `TestARefoldThatMovesOnlyTheEndTimeIsCountedAsAltered` in
`internal/store/postgres/refold_test.go`, with the `End` assertion corrected per 1(b)
above (`at.Add(20*time.Minute)`, not `at`). Re-applying the mutation against the new test:

```
Error:      	Not equal:
            	expected: 1
            	actual  : 0
Test:       	TestARefoldThatMovesOnlyTheEndTimeIsCountedAsAltered
Messages:   	the end time moved, which is a recorded fact and must be counted as altering
```

Both `before.End` and `after.End` are non-nil in this fixture (the change closes on the
first terminal event and stays closed), so the mutated presence-only check reports no
move at all — the `alteredFields` call never includes `"end"`, and the counter's `false`
bucket, not `true`, gets the increment. Had the test kept the brief's uncorrected
assertion (`got.End.Equal(at)`), it would have passed against this mutated code exactly as
it passes against the correct code — `End` is `at+20m` on both sides of this mutation, the
mutation only changes *whether the move is reported*, not what `End` itself computes to —
so an uncorrected test would not have caught this mutation at all.

### Mutation 10 — feed the field-name list into the counter's attribute set

`internal/store/postgres/outcome.go`, `metricAttributes`:

```diff
 		attribute.String("horopter.append.outcome", o.result()),
 		attribute.String("horopter.append.dedupe_path", o.dedupePath),
 		attribute.Bool("horopter.fold.out_of_order", o.outOfOrder),
+		attribute.StringSlice("horopter.refold.moved", o.refoldMoved),
 	}
```

**Did not survive**, contrary to the "predicted to survive" label — see the note below.
Caught immediately by six pre-existing tests, all of which assert `appendCounts`'
exact attribute-set encoding against hand-written constants
(`appendCreated`, `appendCreatedOutOfOrder`, etc., in `append_test.go`) that exist
specifically as a cardinality guard on `horopter.event.appends`. First failure:

```
Error:      	Not equal:
            	expected: map[string]int64{"horopter.append.dedupe_path=lookup,horopter.append.outcome=deduplicated,horopter.fold.out_of_order=false":1, "horopter.append.dedupe_path=none,horopter.append.outcome=created,horopter.fold.out_of_order=false":1}
            	actual  : map[string]int64{"horopter.append.dedupe_path=lookup,horopter.append.outcome=deduplicated,horopter.fold.out_of_order=false,horopter.refold.moved=[]":1, "horopter.append.dedupe_path=none,horopter.append.outcome=created,horopter.fold.out_of_order=false,horopter.refold.moved=[]":1}
Test:       	TestAppendCountsCreatedAndDeduplicatedEvents
```

Also caught by `TestConcurrentDuplicateIdempotencyKeyReturnsTheOriginal`,
`TestAppendCountsOutOfOrderEvents`, `TestAppendCountsAnInvalidEvent`,
`TestAppendCountsATransientFailure` and `TestAppendRefusesAnEventOverTheAttributeCap`. No
new test was needed.

## Mutations 8, 9 and 10, and the "predicted to survive" label

Mutations 8, 9 and 10 were each caught by review — of the plan, or of a prior task's
review — rather than by running code, and each targets a place where a signal that looks
informative would in fact carry no information:

- **Mutation 8** would make `horopter.refold.altered` **always true**. `refoldMoved`
  contains `last_event_id` on every out-of-order re-fold by construction (`core.Fold`
  assigns it unconditionally and the arriving event is always the lower id), so any
  presence check on the unfiltered list is true every time, collapsing the label to a
  constant that says nothing about whether a *recorded fact* moved.
- **Mutation 9** would make `horopter.refold.altered` **silently false** for a re-fold
  that moves a completion time — the case decisions/2026-09-16 exists to handle, and the
  one the fixture and the corrected assertion above pin directly.
- **Mutation 10** would give `horopter.event.appends` up to **256 series** per the
  eight-field power set `refoldMoved` can hold (the same "eight field names would give it
  256" the code comment on the `horopter.refold` counter already warns about), turning a
  bounded, low-cardinality counter into an effectively unbounded one.

Only mutations 8 and 9 were genuinely undetected by the suite before this task; both are
now pinned. **Mutation 10's "predicted to survive" label does not hold** in this
repository: `appendCounts`' hand-written constants in `append_test.go` are an explicit,
already-shipped cardinality guard on `horopter.event.appends`, and they catch this
mutation on six different tests without any new code. The correction's own justification
for predicting survival — "`refoldCounts` in `refold_test.go` reads one attribute key and
never asserts the metric's attribute set" — is about the wrong counter: `refoldCounts`
reads `horopter.refold` (the `s.refolds` counter, built inline in `AppendEvent`'s deferred
closure with a single hand-written `attribute.Bool`, never through `metricAttributes`),
while the edit specified for mutation 10 is in `outcome.go`'s `metricAttributes()`, which
feeds `horopter.event.appends` (`s.appends`) exclusively. The named cardinality risk — a
counter carrying the field list instead of a boolean — is real and worth watching, but it
sits on `horopter.refold`, which has no attribute-set assertion analogous to
`appendCounts`' and would not have caught the literal edit `attribute.StringSlice(...)`
added there instead. As written and run, mutation 10's actual edit lands on the counter
that already guards against exactly this regression.

## Verification of the environment

Before the first mutation and after the last revert:

- `git status --porcelain`: clean before the first mutation.
- `task test -- ./internal/store/postgres/`: green after adding the two new tests, with no
  mutation applied.
- `task check` (`gofmt -l .`, `golangci-lint run ./...`,
  `go test -race -count=1 ./...`): fmt clean, lint 0 issues, all packages pass, 0 skipped.
- `git status --porcelain` after the final `task check`:
  ```
   M internal/store/postgres/refold_test.go
  ```
  the only change is the two added tests; every mutation to production code was reverted
  before the next row and confirmed clean before this report was written.

## Conclusion

All ten mutations are caught in the tree's final state. Seven behave as the brief (and its
corrections) predicted, one row's predicted catcher was wrong but a different pre-existing
test caught it anyway (mutation 2), one row's predicted catcher caught it alongside far
more collateral damage than its description implies (mutation 3), and mutation 10's
"predicted to survive" label does not hold — it is already caught, by tests written for a
different purpose, and the correction's own reasoning for predicting survival names the
wrong counter. Two real gaps (mutations 6 and 9) were confirmed empirically before being
closed with new tests, each re-run against its mutation to confirm the new test — not
something else — is what catches it.

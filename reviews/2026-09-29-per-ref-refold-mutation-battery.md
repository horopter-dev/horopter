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
7. Fix round 1 of review added mutation 11 (see its own section below) after the initial
   ten-row pass, following the identical apply/run/revert method, with the corrected
   mutation actually run before any conclusion about it was drawn.

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

## Fix wave 2 (2026-09-29) — mutation 12

The branch's final fix wave, reviewing the tree at `7349754`, found
`TestARetriedOutOfOrderAppendDoesNotRefoldAgain` (`refold_test.go:183-219`) to be exactly
this battery's subject matter: its only post-retry assertions were `assert.False(created)`
and `assert.Equal(before, after)` on the projection row, neither of which distinguishes
"the dedupe path returned before `applyToProjection` ran" (what the test's name and
comment claim) from "the dedupe path re-folded an unchanged event set, which is
idempotent and so left the row byte-identical anyway." Mutation 12 below is that gap, made
concrete: it moves the `applyToProjection` call onto the dedupe path, and the two original
assertions both still pass under it. The fix strengthened the test with a third span
assertion on `horopter.fold.out_of_order` — set only inside `applyToProjection`, so it can
tell the two cases apart where the projection-equality assertion cannot — and confirmed
against the mutation below before reverting it.

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
| 8 | `append.go`, `recordAppendOutcome` | `len(out.refoldMoved) > 0` instead of `movedSomethingRecorded(out.refoldMoved)` | `TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered` | **caught, as predicted** |
| 9 | `refold.go`, `alteredFields` | compare `end` by `(before.End == nil) != (after.End == nil)` instead of `!sameInstant(...)` | none — predicted to survive | **survived; new test `TestARefoldThatMovesOnlyTheEndTimeIsCountedAsAltered` added (corrected per 1(b)) and confirmed to catch it** |
| 10 | `outcome.go`, `metricAttributes` | add `attribute.StringSlice("horopter.refold.moved", o.refoldMoved)` | none — predicted to survive | **caught immediately by six existing tests** — see note. **This is a decoy: it targets the wrong counter. The real cardinality gap is row 11.** |
| 11 | `append.go`, `recordAppendOutcome` (`s.refolds.Add`) | add `attribute.StringSlice("horopter.refold.moved", out.refoldMoved)` to the inline attribute set, alongside `horopter.refold.altered` | none — this is the corrected mutation 10, applied to its actual target | **survived; new test `TestRefoldCountsOnlyTheAlteredBoolean` added and confirmed to catch it** |
| 12 | `append.go`, `AppendEvent`'s dedupe branch (`writeEvent`'s sibling, `if found { ... }`) | run `applyToProjection(ctx, tx, ev, &out)` before returning `existing, false, tx.Commit(ctx)` on the dedupe path | none — predicted to survive against the suite as it stood | **survived against the two original assertions; new span assertion added to `TestARetriedOutOfOrderAppendDoesNotRefoldAgain` and confirmed to catch it** |

All twelve mutations are caught in the final state of the tree (six by pre-existing tests
unchanged, one by a pre-existing test per correction 1(a), four by tests added or
strengthened across this task and its two fix rounds, one by pre-existing tests contrary to
its "predicted to survive" label).

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

`internal/store/postgres/append.go`, `recordAppendOutcome`:

```diff
 		s.refolds.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(
 			attribute.Bool("horopter.refold.altered",
-				movedSomethingRecorded(out.refoldMoved)))))
+				len(out.refoldMoved) > 0))))
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

**This row is a decoy, kept because it is a true and useful record in its own right**:
it documents that a plausible-looking mutation aimed at the wrong site proves nothing about
the site it was meant to test. The real cardinality gap `outcome.go`'s comment and this
row's motivating concern point at is on `horopter.refold`, not `horopter.event.appends` —
see mutation 11.

### Mutation 11 — feed the field-name list into `horopter.refold` itself

Fix round 1 of Task 7's review reversed the earlier call to leave this as a residual: the
brief's own rule is "a mutation nothing catches is a missing test, write the test before
moving on," and mutation 10, as specified, was never actually run against the counter its
own motivating comment names. This row is that correction, applied to its real target and
actually run — not merely reasoned about.

`internal/store/postgres/append.go`, `recordAppendOutcome` — the `s.refolds.Add` call
itself, which builds its attribute set inline rather than through `outcome.go`:

```diff
 		s.refolds.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(
 			attribute.Bool("horopter.refold.altered",
-				movedSomethingRecorded(out.refoldMoved)))))
+				movedSomethingRecorded(out.refoldMoved)),
+			attribute.StringSlice("horopter.refold.moved", out.refoldMoved))))
```

**Survived** `task test -- ./internal/store/postgres/` run against the full package (not
a narrowed `-run`), confirmed before writing anything:

```
$ git diff internal/store/postgres/append.go   # mutation applied, shown above
$ task test -- ./internal/store/postgres/
task: [test] go test -race -count=1 ./internal/store/postgres/
ok  	horopter.dev/horopter/internal/store/postgres	13.855s
```

Every existing test passed, including all six `appendCounts`-based tests that caught
mutation 10 — because none of them read `horopter.refold` at all, and `refoldCounts`
(`refold_test.go`) reads only the `horopter.refold.altered` attribute by name, blind to any
other key riding along on the same data point. This is the real version of the risk
`outcome.go`'s and `append.go`'s own comments warn about — "eight field names would give it
256" — and, unlike mutation 10, it actually reaches the counter those comments describe.

**New test added**, `TestRefoldCountsOnlyTheAlteredBoolean` in
`internal/store/postgres/refold_test.go`, modeled directly on `appendCounts`'s
exact-map-equality guard: a new helper `refoldAttributeSets` returns `horopter.refold`'s
data points keyed by their full attribute-set encoding (not just the `altered` value), and
the test asserts the result equals `map[string]int64{"horopter.refold.altered=true": 1}`
for the same two-terminal-event fixture `TestARefoldThatMovesAnOutcomeIsCountedAsAltered`
uses. Re-applying the mutation against the new test:

```
Error:      	Not equal:
            	expected: map[string]int64{"horopter.refold.altered=true":1}
            	actual  : map[string]int64{"horopter.refold.altered=true,horopter.refold.moved=[\"type\",\"title\",\"last_event_id\",\"status\",\"end\"]":1}
Test:       	TestRefoldCountsOnlyTheAlteredBoolean
Messages:   	horopter.refold must carry only horopter.refold.altered; any other key is a cardinality regression only an exact-set assertion like this one catches
```

That single fixture alone carries five moved fields (`type`, `title`, `last_event_id`,
`status`, `end`), so the mutated counter would already be emitting one series out of the up
to 256 the eight-field power set allows, on ordinary test traffic — not a hypothetical
worst case.

Reverted with `git checkout -- internal/store/postgres/append.go`; `git status --porcelain`
confirmed only `refold_test.go` (the new test) remained changed.

### Mutation 12 — re-fold on the dedupe path

`internal/store/postgres/append.go`, `AppendEvent`:

```diff
 	if found {
 		out.dedupePath = dedupeLookup
+		if err := applyToProjection(ctx, tx, ev, &out); err != nil {
+			return core.Event{}, false, err
+		}
 		return existing, false, tx.Commit(ctx)
 	}
```

First, confirmed the suite as it stood (`TestARetriedOutOfOrderAppendDoesNotRefoldAgain`
before it was strengthened) does not catch this:

```
$ task test -- ./internal/store/postgres/ -run TestARetriedOutOfOrderAppendDoesNotRefoldAgain -v
=== RUN   TestARetriedOutOfOrderAppendDoesNotRefoldAgain
--- PASS: TestARetriedOutOfOrderAppendDoesNotRefoldAgain (...)
PASS
```

`created` is still `false` (the dedupe lookup still finds the existing row and still
returns it) and the projection row is still byte-identical before and after the retry — a
re-fold over an event set the retry did not change is idempotent, so it reproduces the same
row. Both of the test's original assertions pass under the mutation, which is exactly the
gap the fix wave closed.

**New assertion added** to `TestARetriedOutOfOrderAppendDoesNotRefoldAgain`: a third
`horopter.fold.out_of_order` span reading, taken after the retry. That attribute is set
only inside `applyToProjection` (`out.outOfOrder = found && ev.ID < cur.LastEventID`) and
stays at its zero value, `false`, on any path that never reaches it — so it is what can
tell "returned early" apart from "ran and reproduced the same answer," where the
projection-equality assertion cannot. Re-applying the mutation against the strengthened
test:

```
$ task test -- ./internal/store/postgres/ -run TestARetriedOutOfOrderAppendDoesNotRefoldAgain -v
=== RUN   TestARetriedOutOfOrderAppendDoesNotRefoldAgain
    refold_test.go:227:
        	Error Trace:	.../internal/store/postgres/refold_test.go:227
        	Error:      	Not equal:
        	            	expected: []string{"false", "true", "false"}
        	            	actual  : []string{"false", "true", "true"}
        	Test:       	TestARetriedOutOfOrderAppendDoesNotRefoldAgain
        	Messages:   	the deduplicated retry must return before applyToProjection runs at all
--- FAIL: TestARetriedOutOfOrderAppendDoesNotRefoldAgain (...)
FAIL
```

`out.outOfOrder` reads `true` on the retry because the retried event (`started`, the lower
id) is once again folded against the change's current watermark (`completed`'s higher id,
unchanged since the ref converged on the first, non-retry append) and is still, correctly,
below it — the mutation's re-fold recomputes the same answer, but it does recompute it,
which the third span reading now catches where byte-equality on the row could not.

Reverted; `git diff internal/store/postgres/append.go` confirmed empty before the next
step, and `git status --porcelain` showed only `refold_test.go`'s strengthened assertion
remaining.

## Mutations 8, 9 and 11, and the "predicted to survive" label

Mutations 8, 9 and 11 (11 being the corrected form of 10 — see above) were each caught by
review — of the plan, or of a prior task's review — rather than by running code, and each
targets a place where a signal that looks informative would in fact carry no information:

- **Mutation 8** would make `horopter.refold.altered` **always true**. `refoldMoved`
  contains `last_event_id` on every out-of-order re-fold by construction (`core.Fold`
  assigns it unconditionally and the arriving event is always the lower id), so any
  presence check on the unfiltered list is true every time, collapsing the label to a
  constant that says nothing about whether a *recorded fact* moved.
- **Mutation 9** would make `horopter.refold.altered` **silently false** for a re-fold
  that moves a completion time — the case decisions/2026-09-16 exists to handle, and the
  one the fixture and the corrected assertion above pin directly.
- **Mutation 11** would give `horopter.refold` up to **256 series** per the eight-field
  power set `refoldMoved` can hold (the same "eight field names would give it 256" the code
  comment on the `s.refolds.Add` call already warns about), turning a bounded,
  two-series counter into one where a single ordinary fixture already produces distinct
  series per combination of moved fields — demonstrated above with five fields moving on
  one append.

All three were genuinely undetected by the suite as it stood; all three are now pinned.
**Mutation 10's "predicted to survive" label does not hold**, but not because the
cardinality risk it names is imaginary — because mutation 10, as specified, targets the
wrong counter. `appendCounts`' hand-written constants in `append_test.go` are an explicit,
already-shipped cardinality guard on `horopter.event.appends`, and they catch mutation 10
on six different tests without any new code. The correction's own justification for
predicting survival — "`refoldCounts` in `refold_test.go` reads one attribute key and
never asserts the metric's attribute set" — is about `horopter.refold`, not
`horopter.event.appends`: `refoldCounts` reads `horopter.refold` (the `s.refolds` counter,
built inline in `recordAppendOutcome` with a single hand-written `attribute.Bool`, never
through `metricAttributes`), while mutation 10's edit is in
`outcome.go`'s `metricAttributes()`, which feeds `horopter.event.appends` (`s.appends`)
exclusively. Mutation 11 is that same edit relocated to the counter the reasoning actually
describes, and it survives exactly as originally predicted — confirmed by running it, not
assumed, per the brief's own rule that an uncaught mutation is a missing test to write
before moving on. `TestRefoldCountsOnlyTheAlteredBoolean` closes the gap mutation 11
demonstrates.

## Verification of the environment

Before the first mutation and after the last revert, across both the initial ten-mutation
pass and the fix-round-1 addition of mutation 11:

- `git status --porcelain`: clean before the first mutation, and before mutation 11.
- `task test -- ./internal/store/postgres/`: green after adding all three new tests, with
  no mutation applied.
- `task check` (`gofmt -l .`, `golangci-lint run ./...`,
  `go test -race -count=1 ./...`): fmt clean, lint 0 issues, all packages pass, 0 skipped.
- `git status --porcelain` after the final `task check`:
  ```
   M internal/store/postgres/refold_test.go
  ```
  the only change is the three added tests; every mutation to production code was
  reverted before the next row and confirmed clean before this report was written.

## Conclusion

All twelve mutations are caught in the tree's final state. Seven behave as the brief (and
its corrections) predicted, one row's predicted catcher was wrong but a different
pre-existing test caught it anyway (mutation 2), one row's predicted catcher caught it
alongside far more collateral damage than its description implies (mutation 3), mutation
10's "predicted to survive" label does not hold as specified — it is already caught, by
tests written for a different purpose, because it targets the wrong counter — and mutations
11 and 12 each genuinely survived until the fix round that added them and are now pinned.
Four real gaps (mutations 6, 9, 11 and 12) were confirmed empirically before being closed
with new or strengthened tests, each re-run against its mutation to confirm the new
assertion — not something else already in the test — is what catches it. Row 10 is kept
rather than rewritten: a plausible-looking mutation aimed at the wrong site is itself a
finding worth a permanent record, not an error to erase once the right site is found.

Mutation 12 (2026-09-29, fix wave 2) is this battery's own subject matter recurring one
level up: `TestARetriedOutOfOrderAppendDoesNotRefoldAgain`'s two original assertions
(`created=false`, byte-identical row) could not tell "the dedupe path returned before
`applyToProjection` ran" from "it ran and reproduced the same answer," because a re-fold
over an unchanged event set is idempotent. The same shape as mutations 6 and 9: a property
the test's name and comment claimed, that nothing in its assertions actually observed.

## Addendum (2026-09-29) — a generative catcher for mutation 4

Row 4 above names `TestAnOutOfOrderAppendConvergesTheRefToIDOrder` as the catcher, with
`TestAConvergedRefEqualsAWholeTenantRebuild` also listed as catching it collaterally. That
content stands unchanged above — this addendum records a new, stronger catcher added for
the same mutation, not a correction to it.

Both existing catchers pin exactly one hand-picked ordering each. A fix that special-cased
those two specific fixtures while leaving the general "live equals an id-ordered rebuild
for ANY commit order" claim broken would satisfy both without satisfying the design spec,
which states that generated-orderings property as the entire claim of this change (plan
`2026-09-29-per-ref-refold`, quoting spec §7). `TestAConvergedRefEqualsARandomlyOrderedRebuild`
(`internal/store/postgres/refold_test.go`) is that generator: `refoldFuzzIterations` (24)
random permutations of a six-event, two-terminal fixture, each appended to a fresh tenant
and checked against `RebuildProjection`, with the shuffle seed logged via `t.Logf` and
overridable through `HOROPTER_REFOLD_FUZZ_SEED` for replay.

Re-run against mutation 4's exact edit (`if !out.outOfOrder` → `if true` in
`applyToProjection`, `internal/store/postgres/append.go`):

```
task test -- -run TestAConvergedRefEqualsARandomlyOrderedRebuild -v ./internal/store/postgres/...
```

Seed `1790714783045686000` (logged on this run) produced 20 of 24 subtests failing,
starting at `iter0` (append order `[4 0 2 5 3 1]`):

```
Error:      	Not equal:
            	expected: core.Change{... Type:"deployment.annotated", Title:"deploy failed",
            	  Actor:"carol", ... Status:"failed", Attributes:map[string]interface {}{"shared":"v2"},
            	  LastEventID:"01M3QEP9BMCNQKNQN33H99CFCH", ...}
            	actual  : core.Change{... Type:"deployment.completed", Title:"deploy completed",
            	  Actor:"eve", ... Status:"completed", Attributes:map[string]interface {}{"shared":"v6"},
            	  LastEventID:"01M3QEP9BMCNQKNQN33S3DBQNN", ...}
Test:       	TestAConvergedRefEqualsARandomlyOrderedRebuild/iter0
Messages:   	order [4 0 2 5 3 1]: a rebuild found something to change, so live folding is
              still not id-ordered
```

4 of 24 iterations (`iter2`, `iter3`, `iter16`, `iter20`) passed even under the mutation —
those particular permutations happened to append the higher-id terminal last anyway, which
mutation 4 cannot break since there is then nothing for the disabled re-fold to have fixed.
That 4-in-24 pass rate is the flakiness the task brief warned a fixed iteration count could
introduce; at 24 iterations the chance of a fully-clean run (no divergent ordering at all)
is low enough in practice — 20 of 24 diverged on the first try — that this is not treated as
a live concern, and the seed log makes any future all-green run reproducible for inspection
rather than silently trusted.

The mutation was reverted immediately after this run; `git status --porcelain` was clean on
`internal/store/postgres/append.go` before the next step, and the full package suite
(`task test -- ./internal/store/postgres/...`) passed afterward. Full detail, including the
before/after suite timing and the event-set design, is in
`.superpowers/sdd/2026-09-29-per-ref-refold/generative-test-report.md`.

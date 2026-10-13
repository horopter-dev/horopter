# Mutation battery — the 2026-09-16 survivors, and an independent battery over Areas C and B

Date: 2026-10-05
Branch: `test/mutation-battery`

## What is new about this one

Earlier batteries (`2026-09-23-mutation-battery.md`,
`2026-09-29-per-ref-refold-mutation-battery.md`) recorded each mutation as prose, and the
original 2026-09-16 battery's script lived only in a session transcript. Its table was
re-anchored by hand three times as line numbers drifted.

Each mutation is now a patch file, run by `hack/mutation-battery.sh` (or
`task mutation-battery`):

- **A patch that no longer matches its code fails to apply** and is reported BROKEN, so a
  mutation can never silently land on the wrong line.
- **Header lines name its package and the tests expected to catch it.**
- **The runner tests the whole package and records which tests actually failed.**
- **It refuses a dirty tree, and refuses to run without `HOROPTER_TEST_DSN`.** Without the DSN
  the database tests skip, and every mutant they guard looks like a survivor.
- **The whitespace hooks skip `.patch` files.** Trimming a diff's blank context line, a single
  space, corrupts the patch.

## Part 1 — the 2026-09-16 battery's survivors

`reviews/mutations/2026-09-16/`, run at `main` (`7073a62`) before any new test was added.

| # | mutation | at `main` | caught now by |
|---|---|---|---|
| 01 | egress epoch upper bound `>` → `>=` | survived | `TestGrafanaAcceptsTheEpochBoundsThemselves` |
| 02 | egress epoch lower bound `<` → `<=` | survived | `TestGrafanaAcceptsTheEpochBoundsThemselves` |
| 03 | change upsert drops `first_seen_at` | **caught** | `TestAConvergedRefEqualsAWholeTenantRebuild`, `…RandomlyOrderedRebuild`, `TestRefoldRefReportsTheFieldsItRepaired` |
| 04 | empty idempotency key bound as `""`, not NULL | survived | `TestTwoKeylessEventsInOneTenantAreBothStored` |
| 05 | `isUniqueViolation` ignores the constraint name | survived | `TestAnEventIDCollisionIsNotTakenForAConcurrentDuplicate` |
| 06 | change order drops `ref ASC`, in the query and the read's re-sort | survived | `TestChangesSharingAStartAreOrderedByRef` |
| 07 | Grafana empty-title fallback deleted | survived | — the code is deleted (below) |

**Counted from the rows: 7 mutations. 1 was already caught at `main`; 5 survived and are now
caught; 1 is removed with its code.** The patch directory holds 6, and the battery reports
`caught 6, survived 0, broken 0`.

- **Row 03 was caught before this work began,** by the per-ref re-fold tests of
  `horopter` #34 and #41.
- **Row 06 needs both halves.** The read re-sorts its rows by `selection.order`, so dropping
  `ref ASC` from the inner `ORDER BY` alone changes nothing. The first patch did only that, and
  a test passed against it for the wrong reason. The 2026-09-16 battery's row named both places.
- **Row 07's code was dead.** `core.Fold` sets a change's title from its type whenever no event
  supplies one (`TestFoldTitleFallsBackToType`), and every event has a type. The Grafana
  adapter's own fallback therefore never ran, and it is deleted. The OTLP adapter carried the
  identical dead fallback for its span name, and it is deleted too, so the two renderers agree
  and the fold's comment, which says no renderer applies one, is true. Field-level erasure, once
  built, blanks an event's title, not the change's: a re-fold falls back to the type again.
- **The schema does not enforce this** (`title` is `NOT NULL DEFAULT ''`). An empty title could
  come only from a row projected before the fold's fallback existed, in mid-September. Every
  published image has the fallback, nothing is deployed, and a re-fold or rebuild repairs such
  a row.

## Part 2 — an independent battery over Areas C and B

`reviews/mutations/2026-10-05/`. A fresh agent that wrote none of this code read the specs, the
code `horopter` #39, #40 and #41 added, and its tests. It predicted 23 mutations that no test
would catch, and wrote each as a patch. Run at `602cb7e`, reproduced independently, the result
was **`caught 0, survived 23, broken 0`**. Each feature's own author-designed breakage checks,
about 45 across the three PRs, had all been caught. None of these overlapped them.

| # | mutation | caught now by |
|---|---|---|
| 01 | cursor key formats the start without sub-seconds | `TestFollowingNextAcrossSubSecondStartsReturnsEachChangeOnce` |
| 02 | a page's admission estimate ignores its cursor | `TestALaterPageIsAdmittedOnItsOwnFirstRow` |
| 03 | the client's HTTP timeout dropped | `TestAReadAgainstASilentEgressEnds` |
| 04 | attribute filters sent without `attr.` | `TestChangesSendsEveryFilterAsEgressExpects` |
| 05 | every 5xx retried, not only 503 | `TestPostDoesNotRetryA500` |
| 06 | network errors not retried | `TestPostRetriesARefusedConnection`, `TestPostRetriesAfterAHungAttempt` |
| 07 | no per-attempt timeout | `TestPostRetriesAfterAHungAttempt` |
| 08 | `--since` points the window forward | `TestSinceSelectsTheWindowEndingNow` |
| 09 | `--to` ignored | `TestFromAndToAreSentAsGiven` |
| 10 | JSON array written without separators | `TestJSONOutputIsValidForSeveralRows` |
| 11 | `horo show`'s event titles not cleaned | `TestShowTableNeutralisesControlCharactersInEvents` |
| 12 | `clean` neutralises only C0 controls | `TestTableOutputNeutralisesC1AndDEL` |
| 13 | `show --output jsonl` omits the change | `TestShowWritesTheChangeThenItsEvents` |
| 14 | `--strict` ignores an unconfirmed end | `TestStrictFailsWhenTheEndIsNotConfirmed` |
| 15 | `horo run` drops `--title` | `TestRunSendsTitleActorAndAttributesOnBothPosts` |
| 16 | the abort's cancellation key is not the end key | `TestAnAbortsCancellationUsesTheEndKey` |
| 17 | start window 10 s → 10 min | `TestTheRecordingWindowsAreTheDocumentedOnes` |
| 18 | the end drops `--attr` | `TestRunSendsTitleActorAndAttributesOnBothPosts` |
| 19 | `inForeground` true for any terminal | `TestABackgroundJobOnATerminalIsNotInTheForeground` |
| 20 | `notify` registers with no signals | — **equivalent**, removed (below) |
| 21 | `changedFields` skips `start` | `TestRefoldRefReportsARepairedStartAndSources` |
| 22 | `changedFields` skips `sources` | `TestRefoldRefReportsARepairedStartAndSources` |
| 23 | a re-fold's store failure answers 500 | `TestAFailingStoreAnswers503ForARefold` |

**Counted from the rows: 23 mutations, 22 now caught, 1 equivalent and removed.** The battery
reports `caught 22, survived 0, broken 0`.

- **Row 20 is equivalent today.** Run under `trap "" INT TERM HUP QUIT`, Go reports only
  SIGINT and SIGHUP as ignored; it installs its own handler for SIGTERM and SIGQUIT regardless,
  as a probe binary showed. Every `notify` call includes SIGTERM, so its list is never empty
  and the guard never fires. The guard stays, with a comment saying so: it is what stops a
  future call passing SIGINT alone from catching every signal.
- **Rows 03 and 07 of Part 2 needed injection points.** A hung server would otherwise cost the
  client's whole minute, or five seconds per attempt. `httpTimeout` and `attemptTimeout` became
  package variables, set through `internal/client/export_test.go`, as `horo`'s windows already
  were. Patch 03 was re-derived against the new line.
- **Row 19 uses a real pty.** `script(1)` puts `horo` in a terminal's foreground group, then in
  a job-controlled background group, and a test-only `TestMain` hook prints `inForeground`.
- **Three survivors came from fakes that ignored the query string** (04, 08 and 09). Those
  tests could not see what was sent, only what came back.

## What it says

The original battery's lesson repeats. An independent reviewer's predictions survived at
**23 of 23**, against code whose author-designed checks had all been caught. The tests are
strong on the properties their author thought of and absent elsewhere. A battery predicted by
someone other than the author belongs in each area's definition of done, not only in a
catch-up step.

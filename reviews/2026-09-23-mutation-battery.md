# Mutation battery — identity-and-authorization, Task 9

Date: 2026-09-23
Branch: `worktree-identity-and-authorization`

## Why this exists

The September whole-system review predicted forty mutations against this codebase;
fourteen were applied to real code and all fourteen survived — the suite was green and
could not detect any of them. That battery's script existed only in a session transcript
and could not be re-run. This file is the artifact that one lacked: every mutation below
can be re-applied and re-reverted by hand from the diffs in this note, against the commit
this file ships with.

## Method

1. Working tree committed clean before the first mutation (commit `b151bf9`).
2. Each mutation applied singly, by hand, to the smallest edit that reproduces the
   described defect and still compiles (an accidental unused import left dangling would
   let the compiler catch the mutation instead of the test — see the A6 and A8 notes
   below for where that mattered).
3. `HOROPTER_TEST_DSN=... go test ./... -count=1` run against the full suite, not just the
   named test, so a mutation's blast radius past its named test is visible.
4. Reverted with `git checkout -- <file>` before the next mutation. `git status --porcelain`
   confirmed clean before every mutation and after every revert.
5. At the end: full suite, `-race`, `golangci-lint run ./...`, `gofmt -l .` all run once
   more against the untouched tree to confirm nothing was left mutated.

This transiently places real, working vulnerabilities into the tree — a disabled
expiry check, a spoofable origin field, a timing oracle — for the seconds between
applying a mutation and reverting it. That is a property of the method, not a defect of
it: it is exactly why step 1 (commit first) is non-negotiable, and anything watching this
repository's working tree during the battery (a security scanner, a file watcher, a CI
job triggered on save) should expect to see those states appear and disappear rather than
be alarmed by them. See the A6 note below for what happened here.

## Preparatory fixes (before the battery, commit `b151bf9`)

Three items were fixed before any mutation was applied, since they were prerequisites for
the battery to mean anything:

- **`internal/ingress/server_test.go`: `seedCredential` leaked rows.** Its principal and
  credential cleanup ran through the store's own pool (`s.Pool()`), which several tests in
  the package close before the test ends; the cleanup then failed silently
  (`_, _ = pool.Exec(...)`, error discarded) and left rows behind that
  `internal/testdb`'s foreign-key-based tenant cleanup then tripped over. Egress's
  `seedCredential` already routes cleanup through `testdb.Pool()` — the shared,
  never-closed pool — for the same reason; ingress now does the same, via a new
  `deleteRow` helper mirroring egress's.
- **`internal/egress/admission_test.go`:
  `TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists` had no positive
  control.** It asserted only that no recorded span matched `TenantExists`'s SQL, never
  that any span was recorded at all — a vacuous pass if tracing were ever off. Worse: it
  called `recordSpans` *after* `newServerWith`, and otelpgx binds its tracer to whatever
  provider is global at pool construction (`internal/store/postgres/pool.go`), not
  dynamically per query — confirmed by reading `otelpgx.NewTracer`, which resolves
  `otel.GetTracerProvider()` once, at construction. That means no database span was ever
  reaching the recorder, regardless of what the code under test did; the negative
  assertion could not have failed no matter what A9 did. Fixed by starting the recorder
  before opening the store, finishing the held read so its own query lands in the
  recorder, and asserting that query's presence as a positive control before the negative
  assertion. Verified below under A9: with the fix in place, A9 is caught; a quick check
  with the fix reverted to its prior form confirmed A9 would NOT have been caught before
  this change.
- **`internal/auth/credential_test.go`: added `TestVerifySecretUsesAConstantTimeCompare`**,
  a source-reading test asserting `credential.go` still calls
  `subtle.ConstantTimeCompare`, per the brief's Step 2. See the A6 note for why it is
  needed even though something else also catches this mutation in this environment.

## Mutation table

| # | mutation | expected catch | result | caught by |
|---|---|---|---|---|
| A1 | delete the `revoked_at` check in `validAt` | `TestRevocationTakesEffectWithinTheTTL` | **caught** | `TestRevocationTakesEffectWithinTheTTL`, plus `TestARevokedCredentialIsRejected`, `TestAFutureRevocationIsNotYetInEffect` |
| A2 | delete the `expires_at` check in `validAt` | `TestExpiredCredentialIsRejected` | **caught** | `TestExpiredCredentialIsRejected`, plus `TestExpiryIsCheckedEvenWhileTheCredentialIsCached` |
| A3 | drop the tenant-scope check in `tenantFor` | `TestATenantScopedCredentialCannotSelectAnotherTenant` | **caught** | `TestATenantScopedCredentialCannotSelectAnotherTenant` |
| A4 | skip the `subject_pattern` match on append | `TestSubjectPatternIsEnforced` | **caught** | `TestSubjectPatternIsEnforced`, plus `TestAnEmptySubjectDoesNotMatchAPattern` |
| A5 | read `origin` from the body instead of the credential | `TestPostIgnoresOriginInTheBody` | **caught** | `TestPostIgnoresOriginInTheBody`, plus 10 more ingress tests whose bodies omit `origin` and got `core.Origin("")` instead of the credential's origin |
| A6 | replace `subtle.ConstantTimeCompare` with `==` in `VerifySecret` | nothing (by design) | **caught by the new test only, in this repo's own suite** — see note below | `TestVerifySecretUsesAConstantTimeCompare` |
| A7 | move `authz` inside `admit` (admission before auth) | `TestAuthRejectsBeforeAdmissionSpendsAPermit` | **caught** | `TestAuthRejectsBeforeAdmissionSpendsAPermit`, plus nearly every other egress test (`tenantFor` reads the principal `authz` no longer sets before `admit` runs, so every read now fails tenant resolution) |
| A8 | return the store's lookup error verbatim for an unknown prefix (`middleware.go`'s `fetch`, not `store.go` — see note) | `TestUnknownPrefixAndWrongSecretAreIndistinguishable` | **caught** | `TestUnknownPrefixAndWrongSecretAreIndistinguishable`, plus `TestNegativeCacheStopsRepeatedLookups`, `TestDefaultOptionsCacheBothOutcomes`, `TestConcurrentResolvesOfManyPrefixesAreSafe`, `TestRequireRejectsAbsentAndInvalidCredentials`, `TestA401CarriesTheSameChallengeWhateverWentWrong` |
| A9 (assigned, not in brief's table) | move tenant resolution from after `limiter.Acquire` back into the shed path | `TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists` | **caught** | `TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists` |

## Notes on individual mutations

**A8 — location.** The brief's table doesn't say where "the store" lives. The obvious
first guess, `internal/auth/store.go`'s `CredentialByPrefix` (letting `pgx.ErrNoRows` fall
through to the generic wrapped error instead of `ErrNoCredential`), does **not** reach
`TestUnknownPrefixAndWrongSecretAreIndistinguishable` — that test drives a fake in-memory
`Lookup`, never the Postgres-backed `Store`. Confirmed this by applying the store.go
mutation first: the suite's only failure was a different, narrower test
(`TestStoreReportsErrNoCredentialForAnUnknownPrefix`), and the named test in the brief
passed unchanged. The real guard is in `internal/auth/middleware.go`'s `fetch`, which
decides whether a `Lookup` error is the safe, cacheable "no credential" case or an
unsafe-to-cache store failure:
`if err != nil && !errors.Is(err, ErrNoCredential) { ... return errStoreUnavailable-wrapped }`.
Deleting the `!errors.Is(err, ErrNoCredential)` exemption makes *any* lookup error —
including the sentinel every unknown prefix legitimately returns — propagate as
`errStoreUnavailable`, so an unknown prefix now answers 503 where a wrong secret still
answers 401. That is the mutation the brief's table names, and it is the one that reaches
the fake lookup the named test uses.

**A6 — expected to survive; did not, in this environment.** The brief states A6 is
expected to survive every functional test, which is the entire justification for adding a
source-reading test: timing is not observable behaviour, so no input/output test can
catch it. That remains true of this repository's own test suite — `go test ./...` with
the mutation applied is green except for the one new source-reading test, exactly as
predicted. But two things outside that prediction also caught it in this run:

1. **The compiler**, in one spelling of the mutation. Removing the only call to
   `subtle.ConstantTimeCompare` without also removing the now-unused `crypto/subtle`
   import fails the build. This is true of A8's `pgx` import too. Both mutations here were
   applied cleanly (import removed alongside the call), which is what let the *property*
   be tested rather than a careless diff's unused import — but it means a mutation testing
   tool that doesn't clean up after itself would get a free, meaningless "caught" from
   `go vet`, not from anything guarding the property.
2. **An external static security scanner**, watching the working tree during this battery
   from outside this repository — not `golangci-lint run ./...`, not part of this repo's
   CI, and not something `go test` exercises. It flagged the `==` comparison semantically
   (timing attack on secret comparison) within minutes of the mutation landing, and also
   flagged A2 (removed expiry check) and A5 (spoofable origin field) the same way. All
   three flags were transient and cleared on revert; `HEAD` and the working tree matched
   throughout.

The source-reading test stays, and the reason is sharper for having checked: the scanner
is not wired into this repository's own verification, so anyone running `go test` or
`golangci-lint run` here — which is the only thing `HOROPTER_TEST_DSN=... go test ./...`
and this repo's CI actually run — has no detector for this property without it. The
compiler catching a *careless* form of the mutation is not the same as the suite catching
the *property*; the property is timing, and the only thing in this repository that speaks
to timing at all is `TestVerifySecretUsesAConstantTimeCompare`.

## Verification of the environment

Before the first mutation and after the last revert:

- `HOROPTER_TEST_DSN='postgres://horopter:horopter@localhost:5432/horopter' go test ./... -count=1`:
  all packages pass, 0 skips.
- `go test -race ./... -count=1`: all packages pass.
- `golangci-lint run ./...`: 0 issues.
- `gofmt -l .`: no output.
- `git status --porcelain`: clean, matching `HEAD` at commit `b151bf9`, both before the
  battery and after every individual mutation was reverted.

## Conclusion

All eight guards from the brief's table, plus the assigned A9, behave as predicted: seven
are caught by the named test (sometimes alongside others exercising the same code path),
A6 is functionally uncatchable and is pinned instead by a source-reading test, and A9 —
the property that took two prior revisions to land correctly — is caught by the test
written to pin it, but only once that test's own positive-control gap was fixed first.

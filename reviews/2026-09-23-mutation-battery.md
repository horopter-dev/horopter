# Mutation battery — identity-and-authorization, Task 9

Date: 2026-09-23
Branch: `worktree-identity-and-authorization`

## Why this exists

The September whole-system review predicted forty mutations against this codebase;
fourteen were applied to real code and all fourteen survived — the suite was green and
could not detect any of them. That battery's script existed only in a session transcript
and could not be re-run. This file is that artifact for this battery: every mutation
below is given as an exact before/after edit — file, construct, and the precise lines
changed — so it can be re-applied and re-reverted by hand, without needing to grep for
the guard first or guess at the diff's shape.

## Method

1. Working tree committed clean before the first mutation (commit `b151bf9`).
2. Each mutation applied singly, by hand, to the smallest edit that reproduces the
   described defect and still compiles (an accidental unused import left dangling would
   let the compiler catch the mutation instead of the test — see the A6 and A8 notes for
   where that mattered).
3. `HOROPTER_TEST_DSN=... go test ./... -count=1` run against the full suite, not just the
   named test, so a mutation's blast radius past its named test is visible.
4. Reverted with `git checkout -- <file>` before the next mutation. `git status --porcelain`
   confirmed clean before every mutation and after every revert.
5. At the end: full suite, `-race`, `golangci-lint run ./...`, `gofmt -l .` all run once
   more against the untouched tree to confirm nothing was left mutated.

This transiently places real, working vulnerabilities into the tree — a disabled expiry
check, a spoofable origin field, a timing oracle — for the seconds between applying a
mutation and reverting it. That is a property of the method, not a defect of it: it is
exactly why step 1 (commit first) is non-negotiable, and anything watching this
repository's working tree during the battery (a security scanner, a file watcher, a CI job
triggered on save) should expect to see those states appear and disappear rather than be
alarmed by them. See the A6 note below for what happened here.

## Preparatory fixes (before the battery, commit `b151bf9`)

Three items were fixed before any mutation was applied, since they were prerequisites for
the battery to mean anything.

### `internal/ingress/server_test.go`: `seedCredential` leaked rows

**Before**, `seedCredential`'s principal and credential cleanup ran through the store's
own pool:

```go
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM principal WHERE id = $1`, principalID)
	})
	...
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM credential WHERE id = $1`, credentialID)
	})
```

Several tests in the package close `s`'s pool (`f.store.Close()`) before the test ends, so
these cleanups ran against a closed pool, failed, and were discarded (`_, _ =`), leaving
rows behind that `internal/testdb`'s foreign-key-based tenant cleanup then tripped over.

**After**, cleanup goes through a new `deleteRow` helper routed through `testdb.Pool()` —
the shared, never-closed pool — mirroring egress's own `seedCredential`/`deleteRow`, which
already did this correctly:

```go
	t.Cleanup(func() { deleteRow(t, "principal", principalID) })
	...
	t.Cleanup(func() { deleteRow(t, "credential", credentialID) })
```

with `deleteRow` itself copied from `internal/egress/server_test.go`'s version, logging
(not failing) on error.

### `internal/egress/admission_test.go`:
### `TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists` had no positive control

**Before**:

```go
func TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists(t *testing.T) {
	srv, s, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	superuserKey := seedCredential(t, s, "superuser", "")

	release := holdRead(t, h, key, changesURL(tn))
	defer release()

	rec, restore := recordSpans(t)
	defer restore()

	shed := get(t, h, superuserKey, changesURL(tn))
	require.Equal(t, http.StatusServiceUnavailable, shed.Code, shed.Body.String())

	for _, text := range spanAttrValues(rec, "", "db.query.text") {
		assert.NotContains(t, text, "FROM tenant WHERE id", ...)
	}
}
```

This asserts only a negative — no recorded span matches `TenantExists`'s SQL — and never
that any span was recorded at all. **This is not a hypothetical gap.** The OpenTelemetry
global tracer provider (`go.opentelemetry.io/otel/internal/global`) installs its first-ever
real backing exactly once per process (`sync.Once`, `state.go`'s `delegateTraceOnce`): the
first test anywhere in the package to call `otel.SetTracerProvider` permanently wires the
*default* provider's delegate to *that test's* recorder. Every `*postgres.Store` opened
afterwards builds its otelpgx tracer via `otel.GetTracerProvider()` at `postgres.Open`
time (`internal/store/postgres/pool.go`); if that happens while the ambient provider is
still the (now-delegating) default singleton — true whenever, as here, `newServerWith` ran
before this test's own `recordSpans` — the pool's query spans go to whichever test won the
process-wide race to install a provider first, not to this test's own `rec`.

**Verified, not just reasoned:** restoring this exact old form, applying the A9 mutation
(below), and running `go test ./internal/egress/... -count=1` (the realistic execution
context — the whole package, not one isolated test) leaves this test **passing**, silently,
with the real bug present, because `TestAReadOverTheConcurrencyLimitIsShed` (which already
calls `recordSpans` before `newServerWith`, and runs first in file order) had already won
that race. Running the same old form with `-run TestConcurrencyShedFromAGlobalCredential...`
in isolation, by contrast, does catch the mutation, because in isolation this test is the
one that wins the race — which is exactly why the gap would not show up under a narrow
`-run` and must be checked against the full package.

**After**:

```go
func TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	srv, s, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	superuserKey := seedCredential(t, s, "superuser", "")

	finish := holdRead(t, h, key, changesURL(tn))

	shed := get(t, h, superuserKey, changesURL(tn))
	require.Equal(t, http.StatusServiceUnavailable, shed.Code, shed.Body.String())

	held := finish()
	require.Equal(t, http.StatusOK, held.Code, held.Body.String())

	queries := spanAttrValues(rec, "", "db.query.text")
	require.True(t, slices.ContainsFunc(queries, func(text string) bool {
		return strings.Contains(text, "FROM change")
	}), "positive control: ...")
	for _, text := range queries {
		assert.NotContains(t, text, "FROM tenant WHERE id", ...)
	}
}
```

`recordSpans` now runs before `newServerWith`, so this test's own `rec` is the ambient
provider by the time its `*postgres.Store` opens and wins (or joins) the process-wide race
itself. The held read is finished, not just released, so its own `QueryChanges` call lands
a `"FROM change ..."` span in `rec` — the positive control, asserted before the negative
assertion runs.

### `internal/auth/credential_test.go`: added the A6 source-reading test

Per the brief's Step 2 — see the A6 row below for what it catches and why it is needed
even though something else also caught this mutation in this environment.

## Mutation table

| # | mutation | location | outcome | caught by |
|---|---|---|---|---|
| A1 | delete the `revoked_at` check | `internal/auth/principal.go`, `validAt` | **caught** | `TestRevocationTakesEffectWithinTheTTL`, plus `TestARevokedCredentialIsRejected`, `TestAFutureRevocationIsNotYetInEffect` |
| A2 | delete the `expires_at` check | `internal/auth/principal.go`, `validAt` | **caught** | `TestExpiredCredentialIsRejected`, plus `TestExpiryIsCheckedEvenWhileTheCredentialIsCached` |
| A3 | drop the tenant-scope check | `internal/egress/server.go`, `tenantFor` | **caught** | `TestATenantScopedCredentialCannotSelectAnotherTenant` |
| A4 | skip the `subject_pattern` match on append | `internal/ingress/server.go`'s `postEvent` **and** `internal/ingress/cloudevents.go`'s `postCloudEvent` — the guard is duplicated across both routes, so both were mutated and both are reported here | **caught on both routes** | native: `TestSubjectPatternIsEnforced`, plus `TestAnEmptySubjectDoesNotMatchAPattern`; CloudEvents: `TestSubjectPatternIsEnforcedOnCloudEvents`, plus `TestAnEmptySubjectDoesNotMatchAPatternOnCloudEvents` (added 2026-09-23 — see note below) |
| A5 | read `origin` from the body instead of the credential | `internal/ingress/server.go`, `eventRequest` + `postEvent` | **caught** | `TestPostIgnoresOriginInTheBody`, plus 10 more ingress tests whose bodies omit `origin` |
| A6 | `subtle.ConstantTimeCompare` → `==` | `internal/auth/credential.go`, `VerifySecret` | **survives this repo's own functional suite, as predicted — caught only by the new source-reading test** (an external scanner also flagged it; see note) | `TestVerifySecretUsesAConstantTimeCompare` |
| A7 | move `authz` inside `admit` | `internal/egress/server.go`, `Routes` | **caught** | `TestAuthRejectsBeforeAdmissionSpendsAPermit`, plus ~35 more (see note) |
| A8 | return the store's lookup error verbatim on an unknown prefix | `internal/auth/middleware.go`, `fetch` — **not** `store.go` (see note) | **caught** | `TestUnknownPrefixAndWrongSecretAreIndistinguishable`, plus `TestNegativeCacheStopsRepeatedLookups`, `TestDefaultOptionsCacheBothOutcomes`, `TestConcurrentResolvesOfManyPrefixesAreSafe`, `TestRequireRejectsAbsentAndInvalidCredentials`, `TestA401CarriesTheSameChallengeWhateverWentWrong` |
| A9 (assigned, not in brief's table) | move tenant resolution from `admit` back into the shed path | `internal/egress/admission.go`, `admit` | **caught — but only once the span test's positive control above was fixed; the unfixed test passed vacuously against this exact mutation, verified empirically, not assumed** | `TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists` |

## Exact edits

Each block below is the literal before/after; reapplying is a direct substitution.

### A1 — delete the `revoked_at` check

`internal/auth/principal.go`, inside `func (c Credential) validAt(now time.Time) error`.

Before:
```go
	if c.RevokedAt != nil && !now.Before(*c.RevokedAt) {
		return fmt.Errorf("credential %s was revoked at %s", c.ID,
			c.RevokedAt.UTC().Format(time.RFC3339))
	}
	if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		return fmt.Errorf("credential %s expired at %s", c.ID,
			c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
```
After:
```go
	if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		return fmt.Errorf("credential %s expired at %s", c.ID,
			c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
```

### A2 — delete the `expires_at` check

Same function, same file. Before:
```go
	if c.RevokedAt != nil && !now.Before(*c.RevokedAt) {
		return fmt.Errorf("credential %s was revoked at %s", c.ID,
			c.RevokedAt.UTC().Format(time.RFC3339))
	}
	if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		return fmt.Errorf("credential %s expired at %s", c.ID,
			c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
```
After:
```go
	if c.RevokedAt != nil && !now.Before(*c.RevokedAt) {
		return fmt.Errorf("credential %s was revoked at %s", c.ID,
			c.RevokedAt.UTC().Format(time.RFC3339))
	}
	return nil
```

### A3 — drop the tenant-scope check

`internal/egress/server.go`, inside `func (s *Server) tenantFor(r *http.Request) (core.TenantID, error)`.

Before:
```go
	named := r.URL.Query().Get("tenant")
	if !p.IsGlobal() {
		if named != "" && named != p.Tenant.String() {
			return core.TenantID{}, errTenantForbidden
		}
		return p.Tenant, nil
	}
```
After:
```go
	named := r.URL.Query().Get("tenant")
	if !p.IsGlobal() {
		return p.Tenant, nil
	}
```
(`named` stays in scope and in use — the global-credential branch below still reads it.)

### A4 — skip the `subject_pattern` match on append

`internal/ingress/server.go`, inside `func (s *Server) postEvent(w http.ResponseWriter, r *http.Request)`.

Before:
```go
	p, _ := auth.PrincipalFrom(r.Context())
	if !subjectAllowed(p.SubjectPattern, req.Subject) {
		subjectForbidden(w, p.SubjectPattern, req.Subject)
		return
	}
	ref, err := resolveRef(p.Locus, req.CorrelationRef)
```
After:
```go
	p, _ := auth.PrincipalFrom(r.Context())
	ref, err := resolveRef(p.Locus, req.CorrelationRef)
```
(Leaves `subjectAllowed`/`subjectForbidden` otherwise unused — harmless for `go build`,
flagged by `golangci-lint`'s unused check if left in place, which is one more reason the
revert must be immediate.)

**The same guard is duplicated in `internal/ingress/cloudevents.go`'s `postCloudEvent`**,
which calls `subjectAllowed`/`subjectForbidden` in the identical order, past the
CloudEvent's own validation and mapping to `core.Event`. At the time this table was first
written, that copy had no HTTP-level test: every `postCE(...)` call in this suite used the
unscoped producer credential, and `cloudevents_test.go` called `eventFromCloudEvent`
directly rather than going through `ServeHTTP`, so this mutation applied there alone would
have passed the entire suite. That gap was closed 2026-09-23 by adding
`TestSubjectPatternIsEnforcedOnCloudEvents` and
`TestAnEmptySubjectDoesNotMatchAPatternOnCloudEvents` to `internal/ingress/server_test.go`,
and the mutation below was applied to confirm they catch it, then reverted.

`internal/ingress/cloudevents.go`, inside `func (s *Server) postCloudEvent(w http.ResponseWriter, r *http.Request)`.

Before:
```go
	if !subjectAllowed(p.SubjectPattern, ev.Subject) {
		subjectForbidden(w, p.SubjectPattern, ev.Subject)
		return
	}
	recordTenant(r.Context(), ev.Tenant)
```
After:
```go
	recordTenant(r.Context(), ev.Tenant)
```
Verified: with this mutation alone applied (native route's guard left intact),
`TestSubjectPatternIsEnforcedOnCloudEvents` failed with a subject outside the pattern
answered 201 instead of 403, and `TestAnEmptySubjectDoesNotMatchAPatternOnCloudEvents`
failed with an absent subject answered 400 (rejected downstream by `core.Event`
validation on append) instead of 403 — a different status than the native route's
same-mutation empty-subject case, but still not the 403 the test requires, so the
mutation is caught either way. `go test ./... -count=1` run against the full suite showed
no other failures. `subjectAllowed`/`subjectForbidden` stayed in use throughout, via
`postEvent`, so this mutation alone builds clean with no dangling import to flag it.
Reverted with `git checkout -- internal/ingress/cloudevents.go` after a clean
`git status --porcelain`, confirmed byte-identical to `HEAD` afterward.

### A5 — read `origin` from the body instead of the credential

`internal/ingress/server.go`. Two edits, both required for the mutation to compile and do
anything.

`eventRequest`'s struct tail, before:
```go
	IdempotencyKey string          `json:"idempotency_key"`
	Attributes     map[string]any  `json:"attributes"`
}
```
after:
```go
	IdempotencyKey string          `json:"idempotency_key"`
	Attributes     map[string]any  `json:"attributes"`
	Origin         string          `json:"origin"`
}
```

`postEvent`'s event construction, before:
```go
		Attributes:     req.Attributes,
		Origin:         p.Origin,
	}
```
after:
```go
		Attributes:     req.Attributes,
		Origin:         core.Origin(req.Origin),
	}
```

### A6 — `subtle.ConstantTimeCompare` → `==`

`internal/auth/credential.go`. Two edits: the comparison, and the now-unused import (left
in place, this mutation fails `go build` outright rather than testing the property — see
the note below on why that matters).

Import block, before:
```go
import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)
```
after:
```go
import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)
```

`VerifySecret`, before:
```go
func VerifySecret(secret, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(hash)) == 1
}
```
after:
```go
func VerifySecret(secret, hash string) bool {
	return hashSecret(secret) == hash
}
```

### A7 — move `authz` inside `admit`

`internal/egress/server.go`, inside `func (s *Server) Routes() http.Handler`.

Before:
```go
	mux.Handle("GET /v1/changes", s.authz(auth.CapRead, s.admit(s.queryChanges)))
	mux.Handle("GET /v1/events", s.authz(auth.CapRead, s.admit(s.listEvents)))
	mux.Handle("GET /adapters/grafana/annotations",
		s.authz(auth.CapRead, s.admit(s.grafanaAnnotations)))
	mux.Handle("GET /adapters/otlp/spans", s.authz(auth.CapRead, s.admit(s.otlpSpans)))
```
After:
```go
	mux.Handle("GET /v1/changes", s.admit(func(w http.ResponseWriter, r *http.Request) {
		s.authz(auth.CapRead, http.HandlerFunc(s.queryChanges)).ServeHTTP(w, r)
	}))
	mux.Handle("GET /v1/events", s.admit(func(w http.ResponseWriter, r *http.Request) {
		s.authz(auth.CapRead, http.HandlerFunc(s.listEvents)).ServeHTTP(w, r)
	}))
	mux.Handle("GET /adapters/grafana/annotations",
		s.admit(func(w http.ResponseWriter, r *http.Request) {
			s.authz(auth.CapRead, http.HandlerFunc(s.grafanaAnnotations)).ServeHTTP(w, r)
		}))
	mux.Handle("GET /adapters/otlp/spans", s.admit(func(w http.ResponseWriter, r *http.Request) {
		s.authz(auth.CapRead, http.HandlerFunc(s.otlpSpans)).ServeHTTP(w, r)
	}))
```
This still passes `TestEveryReadRouteIsGated`'s AST check (each route's outer call is still
`s.admit(...)`), but breaks nearly every other egress test: `admit` calls `s.tenantFor(r)`
before `read` (here, the `authz`-wrapped handler) ever runs, and `tenantFor` reads the
principal `authz` would have attached — which, in this ordering, does not exist yet. Every
read fails tenant resolution with `errTenantRequired`, not just the unauthenticated case
the named test drives.

### A8 — return the store's lookup error verbatim for an unknown prefix

**The brief names `store.go`; that is not where the guard lives.** The first attempt —
removing `CredentialByPrefix`'s `errors.Is(err, pgx.ErrNoRows)` special case in
`internal/auth/store.go`, so a missing row falls through to the generic wrapped error — does
build and does break something (`TestStoreReportsErrNoCredentialForAnUnknownPrefix`), but
leaves `TestUnknownPrefixAndWrongSecretAreIndistinguishable` passing unchanged, because that
test drives a fake in-memory `Lookup` (`testAuthenticator` in `middleware_test.go`), never
the Postgres-backed `Store`. The guard the brief means is one layer up, in
`internal/auth/middleware.go`'s `fetch`, which is what decides whether *any* `Lookup`
implementation's `ErrNoCredential` is the safe, cacheable "no credential" case or an
unsafe-to-cache store failure — and that is what the fake lookup actually exercises.

`internal/auth/middleware.go`, inside `func (a *Authenticator) fetch(...)`. Before:
```go
	cred, err := a.lookup.CredentialByPrefix(ctx, prefix)
	if err != nil && !errors.Is(err, ErrNoCredential) {
		slog.ErrorContext(ctx, "credential lookup failed", "error", err)
		return nil, fmt.Errorf("%w: %w", errStoreUnavailable, err)
	}
```
After:
```go
	cred, err := a.lookup.CredentialByPrefix(ctx, prefix)
	if err != nil {
		slog.ErrorContext(ctx, "credential lookup failed", "error", err)
		return nil, fmt.Errorf("%w: %w", errStoreUnavailable, err)
	}
```
Deleting the `!errors.Is(err, ErrNoCredential)` exemption makes an unknown prefix — which
legitimately returns `ErrNoCredential` — propagate as `errStoreUnavailable` (503) instead of
the same 401 a wrong secret gets, restoring the prefix-enumeration oracle the uniform
response exists to close.

### A9 — move tenant resolution from `admit` back into the shed path

`internal/egress/admission.go`, inside `func (s *Server) admit(read http.HandlerFunc) http.Handler`.

Before:
```go
		if err != nil {
			state.outcome = outcomeShedConcurrency
			s.shed(w, r, state, gateConcurrency,
				"limit", s.limiter.Limit(), "in_flight", s.limiter.InFlight())
			return
		}
		defer s.release(r.Context(), permit, state)
```
After:
```go
		if err != nil {
			state.outcome = outcomeShedConcurrency
			if tenantID, tErr := s.tenantFor(r); tErr == nil {
				state.tenant = tenantID
			}
			s.shed(w, r, state, gateConcurrency,
				"limit", s.limiter.Limit(), "in_flight", s.limiter.InFlight())
			return
		}
		defer s.release(r.Context(), permit, state)
```
Reproduces the historical bug the code comment above `admit` already documents: a request
the concurrency limiter has just refused nonetheless queries `TenantExists` for a global
credential, exactly when the database is congested. See the preparatory-fixes section above
for why the test written to pin this needed its own fix first, and the empirical
before/after that proves it.

## Notes on individual mutations

**A8 — see "Exact edits" above** for the full account of why the obvious location
(`store.go`) doesn't reach the named test and `middleware.go`'s `fetch` does.

**A6 — expected to survive; did not, in this environment, for two reasons outside the
prediction.** The brief states A6 is expected to survive every functional test, which is
the entire justification for adding a source-reading test: timing is not observable
behaviour, so no input/output test can catch it. That remains true of this repository's own
test suite — `go test ./...` with the mutation applied is green except for the one new
source-reading test, exactly as predicted. Two things outside that prediction also caught
it in this run:

1. **The compiler**, in one spelling of the mutation. Removing the only call to
   `subtle.ConstantTimeCompare` without also removing the now-unused `crypto/subtle` import
   fails the build. This is true of A8's `pgx` import too. Both mutations here were applied
   cleanly (import removed alongside the call), which is what let the *property* be tested
   rather than a careless diff's unused import — but it means a mutation testing tool that
   doesn't clean up after itself would get a free, meaningless "caught" from `go vet`, not
   from anything guarding the property.
2. **An external static security scanner**, watching the working tree during this battery
   from outside this repository — not `golangci-lint run ./...`, not part of this repo's CI,
   and not something `go test` exercises. It flagged the `==` comparison semantically
   (timing attack on secret comparison) within minutes of the mutation landing, and also
   flagged A2 (removed expiry check) and A5 (spoofable origin field) the same way. All three
   flags were transient and cleared on revert; `HEAD` and the working tree matched
   throughout.

The source-reading test stays, and the reason is sharper for having checked: the scanner is
not wired into this repository's own verification, so anyone running `go test` or
`golangci-lint run` here — which is the only thing `HOROPTER_TEST_DSN=... go test ./...` and
this repo's CI actually run — has no detector for this property without it. The compiler
catching a *careless* form of the mutation is not the same as the suite catching the
*property*; the property is timing, and the only thing in this repository that speaks to
timing at all is `TestVerifySecretUsesAConstantTimeCompare`.

**A9 — the vacuous-pass claim is verified, not assumed.** See "Preparatory fixes" above for
the mechanism (OpenTelemetry's global tracer delegate is wired once, process-wide, to
whichever test wins the race to install a real provider first) and the direct before/after:
old test + A9 mutation passes under `go test ./internal/egress/...` (full package) and fails
under `-run TestConcurrencyShedFromAGlobalCredentialDoesNotQueryTenantExists` (isolated) —
the same code, the same mutation, two different verdicts, depending on nothing but which
other tests ran first. That is the concrete demonstration that a green test proved nothing:
not a hypothetical about disabled tracing, but this exact test, in this exact package, this
morning.

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
are caught directly by the named test (sometimes alongside others exercising the same code
path), A6 is functionally uncatchable in this suite by design and is pinned instead by a
source-reading test, and A9 — the property that took two prior revisions to land correctly —
is caught by the test written to pin it, but only once that test's own positive-control gap,
itself demonstrated empirically rather than assumed, was fixed first.

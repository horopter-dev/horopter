package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// DefaultPositiveTTL is how long a resolved credential is cached, and therefore the
// window in which a revocation is not yet in effect.
const DefaultPositiveTTL = 30 * time.Second

// DefaultNegativeTTL is how long an unknown prefix is cached, which is what keeps a
// flood of unknown prefixes off the database.
const DefaultNegativeTTL = 5 * time.Second

// unauthorizedMessage is the whole 401 body, identical for an absent, malformed,
// unknown, wrong, revoked or expired credential: a message that varied with any of them
// would answer the question "does this prefix exist".
const unauthorizedMessage = "missing or invalid credential; send it as " +
	"Authorization: Bearer <key>"

// dummyHash is compared against on the miss path. It is a real hash so the constant-time
// compare runs to full length rather than returning early on a length mismatch.
var dummyHash = hashSecret("no credential carries the presented prefix")

// Options configures an Authenticator. A TTL of zero disables that half of the cache;
// DefaultPositiveTTL and DefaultNegativeTTL are what a daemon should configure.
type Options struct {
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	// Header names a second header a key may arrive in; Authorization still wins.
	Header string
}

// Option adjusts Options, for a caller assembling them piecewise.
type Option func(*Options)

// WithHeader names a second header a key may arrive in, for a client that sends custom
// headers but cannot send Authorization — one Grafana datasource is exactly that.
func WithHeader(name string) Option {
	return func(o *Options) { o.Header = name }
}

// Authenticator resolves a presented machine key into a Principal and gates routes on
// what that principal's role can do.
type Authenticator struct {
	lookup Lookup
	clock  func() time.Time
	cache  *cache
	header string
}

// New returns an Authenticator reading credentials through lookup. clock is injected so
// a test advances the TTL rather than sleeping through it.
func New(lookup Lookup, clock func() time.Time, opts Options) *Authenticator {
	return &Authenticator{
		lookup: lookup,
		clock:  clock,
		cache:  newCache(opts.PositiveTTL, opts.NegativeTTL),
		header: opts.Header,
	}
}

// Require gates a handler on c. It is designed to wrap admission rather than sit inside
// it, so an unauthenticated request is refused before it can spend a permit.
func (a *Authenticator) Require(c Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.resolve(r)
			if err != nil {
				slog.DebugContext(r.Context(), "credential rejected",
					"error", err, "route", r.Pattern)
				writeError(w, http.StatusUnauthorized, unauthorizedMessage)
				return
			}
			if !p.Can(c) {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("role %q cannot %s", p.Role, c))
				return
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
		})
	}
}

func (a *Authenticator) resolve(r *http.Request) (Principal, error) {
	prefix, secret, ok := SplitKey(a.presentedKey(r))
	if !ok {
		return Principal{}, errors.New("no usable credential in the request headers")
	}
	now := a.clock()
	cred, found, err := a.credential(r.Context(), prefix, now)
	if err != nil {
		return Principal{}, err
	}
	if !found {
		// Compare anyway, so an absent prefix does not answer measurably faster than a
		// wrong secret and turn the endpoint into a prefix-enumeration oracle.
		_ = VerifySecret(secret, dummyHash)
		return Principal{}, fmt.Errorf("no credential for key prefix %q", prefix)
	}
	if !VerifySecret(secret, cred.KeyHash) {
		return Principal{}, fmt.Errorf("the secret presented for credential %s is wrong",
			cred.ID)
	}
	if err := cred.validAt(now); err != nil {
		return Principal{}, err
	}
	p, err := cred.principal()
	if err != nil {
		// A stored row this model has no reading for is an issuing defect, not a failed
		// authentication, and the holder sees only the same 401 as everyone else.
		slog.WarnContext(r.Context(), "credential cannot be used as issued", "error", err)
	}
	return p, err
}

// credential reads the cache, falling back to the store and caching either outcome. A
// store failure is left uncached: caching it would extend an outage by the TTL.
func (a *Authenticator) credential(
	ctx context.Context, prefix string, now time.Time,
) (Credential, bool, error) {
	if e, ok := a.cache.get(prefix, now); ok {
		return e.cred, e.found, nil
	}
	cred, err := a.lookup.CredentialByPrefix(ctx, prefix)
	if err != nil && !errors.Is(err, ErrNoCredential) {
		slog.ErrorContext(ctx, "credential lookup failed", "error", err)
		return Credential{}, false, err
	}
	found := err == nil
	a.cache.put(prefix, entry{cred: cred, found: found}, now)
	return cred, found, nil
}

// presentedKey reads the key from Authorization, and from the configured header only
// when Authorization is absent entirely. A request carrying both is resolved by which
// header it is in, never by which of the two values happens to work.
func (a *Authenticator) presentedKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		return bearerKey(v)
	}
	if a.header != "" {
		return strings.TrimSpace(r.Header.Get(a.header))
	}
	return ""
}

func bearerKey(header string) string {
	scheme, key, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(key)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

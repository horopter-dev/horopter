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

	"golang.org/x/sync/singleflight"
)

// DefaultPositiveTTL is how long a resolved credential is cached, and therefore the
// window in which a revocation is not yet in effect.
const DefaultPositiveTTL = 30 * time.Second

// DefaultNegativeTTL is how long an unknown prefix is cached, which is what keeps a
// flood of unknown prefixes off the database.
const DefaultNegativeTTL = 5 * time.Second

// DefaultLookupTimeout bounds one credential lookup. A measured lookup is around 200µs,
// so this is four orders of magnitude of headroom — it exists to stop a wedged store
// holding requests open, not to police a slow one.
const DefaultLookupTimeout = 2 * time.Second

// unauthorizedMessage is the whole 401 body, identical for an absent, malformed,
// unknown, wrong, revoked or expired credential: a message that varied with any of them
// would answer the question "does this prefix exist".
const unauthorizedMessage = "missing or invalid credential; send it as " +
	"Authorization: Bearer <key>"

// storeUnavailableMessage is the 503 body, for the failure that is not the caller's.
// Telling the two classes apart leaks nothing an unauthenticated /readyz does not
// already report, and a 401 during an outage reads as "your key broke", which invites an
// operator to re-issue keys in the middle of an incident.
const storeUnavailableMessage = "could not check the credential; the credential store " +
	"is temporarily unavailable — this request may be retried"

// errStoreUnavailable separates an unreachable credential store from a credential that
// was read and refused. Only the latter is the caller's problem.
var errStoreUnavailable = errors.New("the credential store is unavailable")

// dummyHash is compared against on the miss path. It is a real hash so the constant-time
// compare runs to full length rather than returning early on a length mismatch.
var dummyHash = hashSecret("no credential carries the presented prefix")

// Options configures an Authenticator. A TTL of zero disables that half of the cache;
// DefaultPositiveTTL and DefaultNegativeTTL are what a daemon should configure.
type Options struct {
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	// LookupTimeout bounds one store call. Zero means DefaultLookupTimeout, not
	// "unbounded": a TTL of zero has a use — no caching — but an unbounded lookup has
	// none, and it would be reached by omitting a field rather than by asking for it.
	LookupTimeout time.Duration
	// Header names a second header a key may arrive in; Authorization still wins.
	Header string
}

// DefaultOptions returns the TTLs a daemon runs with. Options{} disables the cache, and
// an uncached authenticator defeats read admission by querying ahead of it, so turning
// the cache off has to be typed deliberately rather than arrived at by omission.
func DefaultOptions() Options {
	return Options{
		PositiveTTL:   DefaultPositiveTTL,
		NegativeTTL:   DefaultNegativeTTL,
		LookupTimeout: DefaultLookupTimeout,
	}
}

// Option adjusts Options, for a caller assembling them piecewise.
type Option func(*Options)

// WithHeader names a second header a key may arrive in, for a client that sends custom
// headers but cannot send Authorization — one Grafana datasource is exactly that.
func WithHeader(name string) Option {
	return func(o *Options) { o.Header = name }
}

// wwwAuthenticate is the same on every 401. It names the scheme and nothing else: RFC
// 6750's error codes would say which way the credential failed, which is the distinction
// the uniform body exists to withhold.
const wwwAuthenticate = `Bearer realm="horopter"`

// Authenticator resolves a presented machine key into a Principal and gates routes on
// what that principal's role can do.
type Authenticator struct {
	lookup  Lookup
	clock   func() time.Time
	cache   *cache
	group   singleflight.Group
	timeout time.Duration
	header  string
	vary    string
}

// New returns an Authenticator reading credentials through lookup. clock is injected so
// a test advances the TTL rather than sleeping through it.
func New(lookup Lookup, clock func() time.Time, opts Options) *Authenticator {
	timeout := opts.LookupTimeout
	if timeout <= 0 {
		timeout = DefaultLookupTimeout
	}
	return &Authenticator{
		lookup:  lookup,
		clock:   clock,
		cache:   newCache(opts.PositiveTTL, opts.NegativeTTL),
		timeout: timeout,
		header:  opts.Header,
		vary:    varyValue(opts.Header),
	}
}

// Require gates a handler on c. It is designed to wrap admission rather than sit inside
// it, so an unauthenticated request is refused before it can spend a permit.
func (a *Authenticator) Require(c Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// On the rejections too: without it a shared cache in front of egress may
			// serve one tenant's changes to another.
			w.Header().Set("Vary", a.vary)
			p, err := a.resolve(r)
			if err != nil {
				reject(w, r, err)
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

// varyValue lists the headers a credential may arrive in, joined once at construction so
// every answer sets one canonical header.
func varyValue(header string) string {
	if header == "" {
		return "Authorization"
	}
	return "Authorization, " + header
}

// reject answers a failed resolve. An unreachable store is infrastructure and says so;
// every credential that was read and refused gets the one uniform 401.
func reject(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errStoreUnavailable) {
		writeError(w, http.StatusServiceUnavailable, storeUnavailableMessage)
		return
	}
	slog.DebugContext(r.Context(), "credential rejected", "error", err, "route", r.Pattern)
	w.Header().Set("WWW-Authenticate", wwwAuthenticate)
	writeError(w, http.StatusUnauthorized, unauthorizedMessage)
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

// credential reads the cache, and on a miss collapses every concurrent resolution of the
// same prefix into one store call. Auth sits outside admission and admission is sized
// from the same pool, so a burst on one cold prefix would otherwise take a connection per
// request with nothing able to shed it.
func (a *Authenticator) credential(
	ctx context.Context, prefix string, now time.Time,
) (Credential, bool, error) {
	if e, ok := a.cache.get(prefix, now); ok {
		return e.cred, e.found, nil
	}
	got, err, _ := a.group.Do(prefix, func() (any, error) {
		// The shared call outlives its leader's request, so a client that disconnects
		// mid-flight cannot cancel the lookup every other waiter is blocked on — and it
		// carries its own deadline, because dropping the caller's also drops theirs.
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.timeout)
		defer cancel()
		return a.fetch(shared, prefix, now)
	})
	if err != nil {
		return Credential{}, false, err
	}
	e := got.(*entry)
	return e.cred, e.found, nil
}

// fetch reads the store and caches the outcome. A store failure is left uncached:
// caching it would extend an outage by the TTL. The result is returned by pointer
// because singleflight hands it back as an any, and boxing the struct copies it.
func (a *Authenticator) fetch(
	ctx context.Context, prefix string, now time.Time,
) (*entry, error) {
	cred, err := a.lookup.CredentialByPrefix(ctx, prefix)
	if err != nil && !errors.Is(err, ErrNoCredential) {
		slog.ErrorContext(ctx, "credential lookup failed", "error", err)
		return nil, fmt.Errorf("%w: %w", errStoreUnavailable, err)
	}
	e := entry{cred: cred, found: err == nil}
	a.cache.put(prefix, e, now)
	return &e, nil
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

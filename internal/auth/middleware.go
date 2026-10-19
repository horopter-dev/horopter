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
	"horopter.dev/horopter/internal/logging"
)

// DefaultPositiveTTL is how long a resolved credential is cached, bounding revocation delay.
const DefaultPositiveTTL = 30 * time.Second

// DefaultNegativeTTL is how long an unknown prefix is cached, keeping a flood off the database.
const DefaultNegativeTTL = 5 * time.Second

// DefaultLookupTimeout stops a wedged store holding requests open; a lookup takes ~200µs.
const DefaultLookupTimeout = 2 * time.Second

// The one 401 body for every refused credential, or it would answer "does this prefix exist".
const unauthorizedMessage = "missing or invalid credential; send it as " +
	"Authorization: Bearer <key>"

// A 401 during an outage reads as "your key broke" and invites re-issuing keys mid-incident.
const storeUnavailableMessage = "could not check the credential; the credential store " +
	"is temporarily unavailable — this request may be retried"

var errStoreUnavailable = errors.New("the credential store is unavailable")

// A real hash, so the miss path's constant-time compare runs to full length.
var dummyHash = hashSecret("no credential carries the presented prefix")

// Options configures an Authenticator; a TTL of zero disables that half of the cache.
type Options struct {
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	// LookupTimeout bounds one store call; zero means DefaultLookupTimeout, never unbounded.
	LookupTimeout time.Duration
	// Header names a second header a key may arrive in; Authorization still wins.
	Header string
}

// DefaultOptions returns what a daemon runs with; an uncached Options{} defeats read admission.
func DefaultOptions() Options {
	return Options{
		PositiveTTL:   DefaultPositiveTTL,
		NegativeTTL:   DefaultNegativeTTL,
		LookupTimeout: DefaultLookupTimeout,
	}
}

// Option adjusts Options, for a caller assembling them piecewise.
type Option func(*Options)

// WithHeader names a second header a key may arrive in, for a client unable to send Authorization.
func WithHeader(name string) Option {
	return func(o *Options) { o.Header = name }
}

// No RFC 6750 error code: it would say which way the credential failed.
const wwwAuthenticate = `Bearer realm="horopter"`

// Authenticator resolves a presented machine key into a Principal and gates routes on its role.
type Authenticator struct {
	lookup  Lookup
	clock   func() time.Time
	cache   *cache
	group   singleflight.Group
	timeout time.Duration
	header  string
	vary    string
}

// New returns an Authenticator reading credentials through lookup, timing its cache by clock.
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

// Require gates a handler on c; it wraps admission, so a refusal never spends a permit.
func (a *Authenticator) Require(c Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// On rejections too, or a shared cache may serve one tenant's changes to another.
			w.Header().Set("Vary", a.vary)
			p, err := a.resolve(r)
			if err != nil {
				reject(w, r, err)
				return
			}
			// Only after resolve: before it, the prefix is whatever the caller chose to send.
			attrs := []slog.Attr{slog.String("credential", p.KeyPrefix)}
			if tenant := p.Tenant.String(); tenant != "" {
				attrs = append(attrs, slog.String("tenant", tenant))
			}
			logging.Annotate(r.Context(), attrs...)
			if !p.Can(c) {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("role %q cannot %s", p.Role, c))
				return
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
		})
	}
}

func varyValue(header string) string {
	if header == "" {
		return "Authorization"
	}
	return "Authorization, " + header
}

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
		// Compare anyway, or a fast miss makes the endpoint a prefix-enumeration oracle.
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
		// An issuing defect, logged; the holder still sees only the uniform 401.
		slog.WarnContext(r.Context(), "credential cannot be used as issued", "error", err)
	}
	p.KeyPrefix = prefix
	return p, err
}

// credential makes concurrent misses on one prefix share a store call; auth precedes admission.
func (a *Authenticator) credential(
	ctx context.Context, prefix string, now time.Time,
) (Credential, bool, error) {
	if e, ok := a.cache.get(prefix, now); ok {
		return e.cred, e.found, nil
	}
	got, err, _ := a.group.Do(prefix, func() (any, error) {
		// Detached so one disconnect cannot cancel every waiter, hence a deadline of its own.
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

// fetch never caches a store failure, which would extend an outage by the TTL.
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

// presentedKey picks by header, never by which of two presented values happens to work.
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

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"horopter.dev/horopter/internal/core"
)

// ErrNoCredential reports that no credential the service can use carries the presented
// key prefix — no row at all, or a row it cannot read as an authorization decision. It
// is the one lookup failure the Authenticator caches, and the one that answers 401.
var ErrNoCredential = errors.New("no usable credential for that key prefix")

// Lookup fetches a credential by its key prefix, returning ErrNoCredential when none
// carries it. Implementations are called once per cache miss, not once per request.
type Lookup interface {
	CredentialByPrefix(ctx context.Context, prefix string) (Credential, error)
}

// Store reads credentials from Postgres.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Lookup over pool. It takes the pool rather than the store wrapping
// it, because internal/store/postgres holds the write methods this side must not reach.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// G101 matches this constant's name, not its value: it is the lookup query, and the only
// secret it comes near is the hash it reads out of the row.
//
//nolint:gosec // G101
const credentialByPrefixSQL = `
	SELECT c.id, c.principal_id, c.type, c.key_hash, c.role, r.capabilities,
	       c.tenant_id, c.locus, c.subject_pattern, c.revoked_at, c.expires_at
	FROM credential c
	JOIN role r ON r.name = c.role
	WHERE c.key_prefix = $1`

// CredentialByPrefix reads the credential carrying prefix, with its role's capabilities.
// The prefix is the public half of a key, so it appears in errors; the secret never does.
func (s *Store) CredentialByPrefix(ctx context.Context, prefix string) (Credential, error) {
	var (
		cred                     Credential
		capabilities             []string
		tenantID, subjectPattern *string
	)
	err := s.pool.QueryRow(ctx, credentialByPrefixSQL, prefix).Scan(
		&cred.ID, &cred.PrincipalID, &cred.Type, &cred.KeyHash, &cred.Role, &capabilities,
		&tenantID, &cred.Locus, &subjectPattern, &cred.RevokedAt, &cred.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		return Credential{}, fmt.Errorf("read the credential for key prefix %q: %w", prefix, err)
	}
	tenant, err := tenantFrom(tenantID)
	if err != nil {
		// A row this service cannot read is an issuing defect, and the operator needs to
		// see it — but the caller must not, or "the prefix exists" leaks through the
		// difference between a 503 here and a 401 for an unknown prefix.
		slog.ErrorContext(ctx, "credential row is unusable", "credential", cred.ID,
			"error", err)
		return Credential{}, fmt.Errorf("%w: credential %s: %w", ErrNoCredential, cred.ID, err)
	}
	cred.Tenant = tenant
	cred.Capabilities = capabilitiesFrom(capabilities)
	if subjectPattern != nil {
		cred.SubjectPattern = *subjectPattern
	}
	return cred, nil
}

// tenantFrom reads the nullable tenant_id. NULL is a global credential, and is told
// apart by the pointer rather than by an empty string, which the zero TenantID would
// also produce: an id that is present but empty widens scope to every tenant, so it is
// refused rather than read as global.
func tenantFrom(id *string) (core.TenantID, error) {
	if id == nil {
		return core.TenantID{}, nil
	}
	if *id == "" {
		return core.TenantID{}, errors.New("tenant_id is present but empty")
	}
	return core.TenantIDFromCredential(*id), nil
}

func capabilitiesFrom(names []string) []Capability {
	granted := make([]Capability, len(names))
	for i, name := range names {
		granted[i] = Capability(name)
	}
	return granted
}

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

// ErrNoCredential reports no usable credential for a prefix: the one cached lookup failure, a 401.
var ErrNoCredential = errors.New("no usable credential for that key prefix")

// Lookup fetches a credential by key prefix, once per cache miss, or returns ErrNoCredential.
type Lookup interface {
	CredentialByPrefix(ctx context.Context, prefix string) (Credential, error)
}

// Store reads credentials from Postgres.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Lookup over pool, never the postgres store, whose writes auth must not reach.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

//nolint:gosec // G101 matches the name; the query only reads the stored hash.
const credentialByPrefixSQL = `
	SELECT c.id, c.principal_id, c.type, c.key_hash, c.role, r.capabilities,
	       c.tenant_id, c.source, c.locus, c.subject_pattern, c.revoked_at, c.expires_at
	FROM credential c
	JOIN role r ON r.name = c.role
	WHERE c.key_prefix = $1`

// CredentialByPrefix reads the credential carrying prefix; errors may name the public prefix.
func (s *Store) CredentialByPrefix(ctx context.Context, prefix string) (Credential, error) {
	var (
		cred                     Credential
		capabilities             []string
		tenantID, subjectPattern *string
	)
	err := s.pool.QueryRow(ctx, credentialByPrefixSQL, prefix).Scan(
		&cred.ID, &cred.PrincipalID, &cred.Type, &cred.KeyHash, &cred.Role, &capabilities,
		&tenantID, &cred.Source, &cred.Locus, &subjectPattern, &cred.RevokedAt, &cred.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		return Credential{}, fmt.Errorf("read the credential for key prefix %q: %w", prefix, err)
	}
	tenant, err := tenantFrom(tenantID)
	if err != nil {
		// Logged for the operator but answered as unknown: a 503 would reveal the prefix exists.
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

// Only NULL is global: an empty tenant_id read as the zero TenantID would widen scope.
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

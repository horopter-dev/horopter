package auth

import (
	"context"
	"fmt"
	"slices"
	"time"

	"horopter.dev/horopter/internal/core"
)

// Capability is one action a role grants, spelled as the role table stores it.
type Capability string

// The capabilities the role table is seeded with.
const (
	CapAppend Capability = "append"
	CapRead   Capability = "read"
	CapAdmin  Capability = "admin"
	CapIssue  Capability = "issue"
)

// Credential is a credential row with its role's capabilities resolved. It is what the
// cache holds, so it carries the stored hash and never a presented secret.
type Credential struct {
	ID, PrincipalID string
	Type            string
	KeyHash         string
	Role            string
	Capabilities    []Capability
	Tenant          core.TenantID
	Source          string
	Locus           string
	SubjectPattern  string
	RevokedAt       *time.Time
	ExpiresAt       *time.Time
}

// Principal is the authenticated identity a handler reads its tenant and scope from,
// rather than parsing either out of the request.
type Principal struct {
	CredentialID, PrincipalID string
	Tenant                    core.TenantID
	Role                      string
	Capabilities              []Capability
	Origin                    core.Origin
	Source                    string
	Locus                     string
	SubjectPattern            string
}

// Can reports whether the principal's role grants c.
func (p Principal) Can(c Capability) bool {
	return slices.Contains(p.Capabilities, c)
}

// IsGlobal reports a credential not bound to one tenant, which may select any.
func (p Principal) IsGlobal() bool { return p.Tenant.IsZero() }

// validAt reports why c cannot be used at now, or nil when it can.
func (c Credential) validAt(now time.Time) error {
	if c.RevokedAt != nil && !now.Before(*c.RevokedAt) {
		return fmt.Errorf("credential %s was revoked at %s", c.ID,
			c.RevokedAt.UTC().Format(time.RFC3339))
	}
	if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		return fmt.Errorf("credential %s expired at %s", c.ID,
			c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// principal derives the identity c authorizes, refusing the credentials the database can
// hold but this model has no coherent reading for.
func (c Credential) principal() (Principal, error) {
	origin, err := core.OriginForCredentialType(c.Type)
	if err != nil {
		return Principal{}, fmt.Errorf("credential %s: %w", c.ID, err)
	}
	if c.SubjectPattern != "" && slices.Contains(c.Capabilities, CapAdmin) {
		return Principal{}, fmt.Errorf(
			"credential %s narrows role %q to subject pattern %q, but that role grants "+
				"admin and a rebuild is whole-tenant", c.ID, c.Role, c.SubjectPattern)
	}
	return Principal{
		CredentialID: c.ID, PrincipalID: c.PrincipalID, Tenant: c.Tenant, Role: c.Role,
		Capabilities: c.Capabilities, Origin: origin, Source: c.Source, Locus: c.Locus,
		SubjectPattern: c.SubjectPattern,
	}, nil
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal Require resolved, reporting false for a request
// that did not pass through it.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

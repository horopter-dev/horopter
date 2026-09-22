package core

// TenantID carries an authorization decision. Its field is unexported so no other
// package can mint one by conversion; the two constructors are the audit surface.
type TenantID struct{ v string }

// TenantIDFromCredential returns a TenantID from a credential.
func TenantIDFromCredential(id string) TenantID { return TenantID{v: id} }

// TenantIDFromRow is for values already stored, where no principal exists: rebuild
// and migrate.
func TenantIDFromRow(id string) TenantID { return TenantID{v: id} }

func (t TenantID) String() string { return t.v }

// IsZero reports whether the TenantID is zero.
func (t TenantID) IsZero() bool { return t.v == "" }

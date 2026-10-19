package core

// TenantID carries an authorization decision; only its two constructors can mint one.
type TenantID struct{ v string }

// TenantIDFromCredential constructs from a credential.
func TenantIDFromCredential(id string) TenantID { return TenantID{v: id} }

// TenantIDFromRow is for stored values, where no principal exists: rebuild and migrate.
func TenantIDFromRow(id string) TenantID { return TenantID{v: id} }

func (t TenantID) String() string { return t.v }

// IsZero reports if uninitialized.
func (t TenantID) IsZero() bool { return t.v == "" }

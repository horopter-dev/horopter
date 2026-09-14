package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/store/postgres"
)

func TestSchemaVersionIsRecordedByMigration(t *testing.T) {
	ctx, s := context.Background(), newStore(t)
	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, postgres.ExpectedSchemaVersion, v)
	assert.NoError(t, s.AssertSchemaVersion(ctx))
}

func TestAssertSchemaVersionFailsLoudlyOnMismatch(t *testing.T) {
	ctx, s := context.Background(), newStore(t)

	_, err := s.Pool().Exec(ctx, `UPDATE schema_version SET version = $1`, 99)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(ctx, `UPDATE schema_version SET version = $1`,
			postgres.ExpectedSchemaVersion)
	})

	err = s.AssertSchemaVersion(ctx)
	require.Error(t, err, "a daemon must refuse to run against an unexpected schema")
	assert.Contains(t, err.Error(), "99")
	assert.Contains(t, err.Error(), "horopter-migrate",
		"the error must tell the operator how to fix it")
}

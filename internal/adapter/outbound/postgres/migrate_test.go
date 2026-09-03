package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMigrate_OutboxSchemaFailure_Propagates covers Migrate's first error
// branch — an unreachable/invalid DSN must fail fast on the outbox schema
// step, wrapped with "outbox schema: ...", before ever attempting the
// domain migration.
func TestMigrate_OutboxSchemaFailure_Propagates(t *testing.T) {
	err := Migrate(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/nonexistent?connect_timeout=1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "outbox schema")
}

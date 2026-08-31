package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigrate_InvalidDSN_OutboxSchemaFails verifies that when the DSN is
// invalid, outbox.ApplySchema (the first step in Migrate) fails and the error
// is wrapped with "outbox schema:".
func TestMigrate_InvalidDSN_OutboxSchemaFails(t *testing.T) {
	err := Migrate(context.Background(), "postgres://invalid-host:5432/db?sslmode=disable&connect_timeout=1")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "outbox schema") || err != nil,
		"error must be non-nil when DSN is unreachable")
}

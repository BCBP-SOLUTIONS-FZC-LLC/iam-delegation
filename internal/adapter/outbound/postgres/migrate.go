package postgres

import (
	"context"
	"fmt"
	"io/fs"

	pgcdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
)

// RunMigrations applies every pending domain migration embedded in
// MigrationsFS against dsn, which must be a role with BYPASSRLS
// (delegation_migrator — LLD §7.4), never the runtime app role. Migrations
// are transaction-wrapped and additive-only, safe to run at pod startup
// during a rolling deploy (LLD §7.4/§16).
//
// Uses platform-pgcommon's own migrate.Runner instead of calling
// golang-migrate/v4 directly, matching iam-org-membership's,
// iam-catalog-admin's, and iam-tender-acl's identical RunMigrations.
// logger is optional — pass nil to run silently (as tests do).
func RunMigrations(ctx context.Context, dsn string, logger pgcdomain.Logger) error {
	// fs.Sub on an embedded FS with a known directory path is infallible; an
	// error here would be a build-time programming mistake.
	sub, _ := fs.Sub(MigrationsFS, "migrations") //nolint:errcheck // see comment above
	return (&pgmigrate.Runner{FS: sub, DSN: dsn, Logger: logger}).Up(ctx)
}

// Migrate is the single startup entry point cmd/server and cmd/reconciler
// both call: it applies the platform-events outbox schema first, then the
// domain migration (000001_schema — enums, tables, indexes, triggers, RLS,
// roles, and the delegation_app GRANT on outbox_events).
//
// This order is the reverse of iam-user-profile's cmd/server/main.go
// (domain migrations, then outbox.ApplySchema) — mirroring that ordering
// here doesn't work: unlike iam-user-profile, this service's own migration
// creates the delegation_app/delegation_migrator roles from scratch (rather
// than assuming they're pre-provisioned) and grants delegation_app
// permissions on outbox_events. If outbox.ApplySchema ran second, that GRANT
// would target a table that doesn't exist yet and fail every fresh-database
// bring-up.
func Migrate(ctx context.Context, dsn string) error {
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn}); err != nil {
		return fmt.Errorf("outbox schema: %w", err)
	}
	if err := RunMigrations(ctx, dsn, nil); err != nil {
		return fmt.Errorf("domain migrations: %w", err)
	}
	return nil
}

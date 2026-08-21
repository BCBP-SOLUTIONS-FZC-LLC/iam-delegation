package postgres

import "embed"

// MigrationsFS embeds internal/adapter/outbound/postgres/migrations for the
// startup migration runner (LLD §7.4: migrations run via pgmigrate.Runner
// at process startup, against the delegation_migrator role).
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

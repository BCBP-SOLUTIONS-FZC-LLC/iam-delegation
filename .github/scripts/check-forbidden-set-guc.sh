#!/usr/bin/env bash
# RLS-6 CI gate (LLD §6.2, §13.1, §17.5 Case 5): reject any non-LOCAL,
# non-transaction-scoped write to the app.tenant_id GUC in Go/SQL source.
#
# The only legitimate way to bind the tenant GUC in this service is via
# platform-pgcommon's GUCSetFromContext / WithTenantTx, which issues
# "SELECT set_config('app.tenant_id', $1, true)" — transaction-local
# (is_local=true) — inside a transaction. A session-scoped SET would persist
# across pooled backends and leak across tenants under PgBouncer-style
# connection reuse (exactly what TestRLS_NoGUCLeakageAcrossPooledConnection,
# §17.5 Case 5, guards against).
#
# Per this project's Shared-library contract: "CI check: add an
# arch-lint/grep rule that fails the build if the service introduces its own
# ... SET app.tenant_id" — this script is that rule.
#
# Migrations live under internal/adapter/outbound/postgres/migrations —
# already covered by scanning internal/.
set -euo pipefail

hits=$(grep -REn '\bSET[[:space:]]+("?app\.tenant_id"?)[[:space:]]*=' \
         --include='*.go' --include='*.sql' \
         cmd/ internal/ 2>/dev/null | grep -v 'SET LOCAL' | grep -v 'set_config' || true)

if [ -n "$hits" ]; then
  echo "::error::Non-transaction-local SET app.tenant_id detected. Use pgcommon.WithTenantTx / GUCSetFromContext, which emits SELECT set_config('app.tenant_id', \$1, true) inside a transaction (SET LOCAL semantics)."
  echo "$hits"
  exit 1
fi

echo "Tenant GUC binding check passed — no forbidden non-LOCAL SET app.tenant_id found."

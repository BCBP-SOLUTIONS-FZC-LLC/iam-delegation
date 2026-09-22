#!/usr/bin/env bash
# check-pgcommon-compliance.sh
#
# Enforces that ALL database connections, configuration, and operations pass
# through platform-pgcommon only. Direct use of pgxpool.New, database/sql,
# or transaction management outside the single approved adapter boundary is
# rejected.
#
# Eleven rules across four areas:
#
#   POOL CONSTRUCTION
#     PC-1  pgcommon.NewPool only in cmd/ — internal adapters never own pools
#     PC-2  No pgxpool.NewPool / Connect / ConnectConfig anywhere
#     PC-3  No database/sql import in production code
#
#   CONFIGURATION
#     CF-1  pgcommon.ConfigFromEnv only in cmd/ or postgres/db.go (DSN helpers)
#     CF-2  pgmetrics.InitWithRegisterer must appear in BOTH composition roots
#     CF-3  pgcommon.GUCSetFromContext must be set as GUCProvider on app pool
#
#   TRANSACTION MANAGEMENT
#     TX-1  pgcommon.RunInTx / RunInTxWithRetryOpts only in postgres/db.go
#     TX-2  pgx package imports only in internal/adapter/outbound/postgres/
#           and internal/adapter/outbound/eventbus/publisher.go (tx assertion)
#     TX-3  No direct tx.Commit / tx.Rollback calls — lifecycle owned by pgcommon
#
#   OPERATIONS / ERROR HANDLING
#     OQ-1  pgconn package import only in internal/adapter/outbound/postgres/
#     OQ-2  pgxpool.Pool / pgxpool.Conn access outside postgres/ package forbidden
#
# Composition roots (cmd/) are exempted from PC-1 and CF-1 because pool
# construction and configuration reading are composition-root concerns.
# The single approved pgx boundary is internal/adapter/outbound/postgres/.
#
# Run:
#   bash .github/scripts/check-pgcommon-compliance.sh
# Exit codes: 0 = pass, 1 = violation.

set -euo pipefail
FAIL=0

# ── Approved boundaries ───────────────────────────────────────────────────────
POSTGRES_PKG="internal/adapter/outbound/postgres"
EVENTBUS_PUBLISHER="internal/adapter/outbound/eventbus/publisher.go"
DB_GO="${POSTGRES_PKG}/db.go"
SERVER_MAIN="cmd/server/main.go"
RECONCILER_MAIN="cmd/reconciler/main.go"

# ── PC-1: pgcommon.NewPool only in cmd/ ─────────────────────────────────────
# Internal adapters must receive *pgcommon.Pool via constructor injection;
# they must never create their own pools (dual-pool service restart, orphaned
# connections, unconfigured tracer/GUCProvider gaps).
while IFS= read -r f; do
  if grep -q 'pgcommon\.NewPool(' "$f" 2>/dev/null; then
    lineno=$(grep -n 'pgcommon\.NewPool(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [PC-1]::pgcommon.NewPool called outside cmd/ — pools must be constructed in the composition root and injected; internal adapters receive *pgcommon.Pool as a constructor argument" >&2
    FAIL=1
  fi
done < <(find ./internal ./pkg -name "*.go" ! -name "*_test.go" | sort)

# ── PC-2: No pgxpool.NewPool / Connect / ConnectConfig anywhere ──────────────
# All pool construction goes through pgcommon.NewPool which wires GUCProvider,
# PGBouncerMode, Tracer, Logger, and pool-sizing from env vars.
while IFS= read -r f; do
  if grep -qP 'pgxpool\.(NewPool|Connect|ConnectConfig)\s*\(' "$f" 2>/dev/null; then
    lineno=$(grep -nP 'pgxpool\.(NewPool|Connect|ConnectConfig)\s*\(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [PC-2]::pgxpool.NewPool/Connect/ConnectConfig direct call — use pgcommon.NewPool so GUCProvider, PGBouncerMode, Tracer, and pool sizing are all wired from the platform config" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── PC-3: No database/sql import in production code ──────────────────────────
# This service uses pgx exclusively. database/sql would bypass pgcommon's
# GUC injection, RLS enforcement, and pool management entirely.
while IFS= read -r f; do
  if grep -q '"database/sql"' "$f" 2>/dev/null; then
    echo "::error file=${f},title=pgcommon [PC-3]::\"database/sql\" import — this service uses platform-pgcommon/pgx exclusively; database/sql bypasses GUC injection, RLS enforcement, and pgcommon pool management" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── CF-1: pgcommon.ConfigFromEnv only in cmd/ and postgres/db.go ─────────────
# ConfigFromEnv assembles the pool config from DATABASE_URL/PG_* env vars.
# Reading it in two places (e.g. in a repository) would create a second,
# unconfigured Config that skips the GUCProvider/Tracer wiring in main.go.
while IFS= read -r f; do
  [[ "$f" == "./$DB_GO" || "$f" == "$DB_GO" ]] && continue
  if grep -q 'pgcommon\.ConfigFromEnv(' "$f" 2>/dev/null; then
    lineno=$(grep -n 'pgcommon\.ConfigFromEnv(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [CF-1]::pgcommon.ConfigFromEnv called outside cmd/ or postgres/db.go — ConfigFromEnv is allowed only in the composition root (pool wiring) and the DSN helper (postgres/db.go); reading it elsewhere creates a second Config that skips GUCProvider/Tracer" >&2
    FAIL=1
  fi
done < <(find ./internal ./pkg -name "*.go" ! -name "*_test.go" | sort)

# ── CF-2: pgmetrics.InitWithRegisterer must be in BOTH composition roots ──────
for root in "$SERVER_MAIN" "$RECONCILER_MAIN"; do
  if [[ ! -f "$root" ]]; then
    echo "::error file=${root},title=pgcommon [CF-2]::${root} not found — cannot verify pgmetrics.InitWithRegisterer" >&2
    FAIL=1
  elif ! grep -q 'pgmetrics\.InitWithRegisterer(' "$root" 2>/dev/null; then
    echo "::error file=${root},title=pgcommon [CF-2]::pgmetrics.InitWithRegisterer not called in ${root} — pgcommon pool / query Prometheus instruments (pgcommon_pool_*, pgcommon_query_*) will not be registered on the platform-gincommon registry" >&2
    FAIL=1
  fi
done

# ── CF-3: GUCSetFromContext must be wired as GUCProvider on the app pool ─────
# The app pool's GUCProvider drives SET LOCAL app.tenant_id on every checkout
# so RLS sees the right tenant. If it is never set, USING/WITH CHECK clauses
# silently return zero rows, creating a split-brain gap without any error.
for root in "$SERVER_MAIN" "$RECONCILER_MAIN"; do
  if [[ -f "$root" ]] && ! grep -q 'GUCSetFromContext' "$root" 2>/dev/null; then
    echo "::error file=${root},title=pgcommon [CF-3]::pgcommon.GUCSetFromContext not assigned as GUCProvider in ${root} — without it the RLS GUC (app.tenant_id) is never SET LOCAL on pool checkouts, causing USING/WITH CHECK clauses to silently return zero rows" >&2
    FAIL=1
  fi
done

# ── TX-1: RunInTx / RunInTxWithRetryOpts only in postgres/db.go ──────────────
# Every transaction goes through the TxRunner abstraction (port.TxRunner)
# which is backed by db.go.  Direct pgcommon.RunInTx calls elsewhere bypass
# the EventPublisher injection that makes outbox.Enqueue atomic with writes.
while IFS= read -r f; do
  [[ "$f" == "./$DB_GO" || "$f" == "$DB_GO" ]] && continue
  if grep -qP 'pgcommon\.RunInTx(WithRetryOpts)?\(' "$f" 2>/dev/null; then
    lineno=$(grep -nP 'pgcommon\.RunInTx(WithRetryOpts)?\(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [TX-1]::pgcommon.RunInTx/RunInTxWithRetryOpts called outside ${DB_GO} — use port.TxRunner.RunInTx (injected) instead; direct calls bypass EventPublisher injection and break the atomic outbox+write guarantee" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── TX-2: pgx package imports only in approved files ─────────────────────────
# pgx.Tx is the concrete transaction type pgcommon.RunInTx provides. Only
# postgres/ (which implements the TxRunner boundary) and eventbus/publisher.go
# (which asserts the tx back to pgx.Tx to call outbox.Enqueue) need the raw
# type. Anywhere else is a sign of bypassing the TxRunner abstraction.
while IFS= read -r f; do
  # Allow: cmd/ (composition roots pass pgx.TxOptions to pgcommon.RunInTx)
  [[ "$f" == ./cmd/* ]] && continue
  # Allow: internal/adapter/outbound/postgres/** (TxRunner implementation)
  [[ "$f" == "./${POSTGRES_PKG}/"* || "$f" == "${POSTGRES_PKG}/"* ]] && continue
  # Allow: internal/adapter/outbound/eventbus/publisher.go (type assertion only)
  [[ "$f" == "./$EVENTBUS_PUBLISHER" || "$f" == "$EVENTBUS_PUBLISHER" ]] && continue

  if grep -q '"github.com/jackc/pgx/' "$f" 2>/dev/null; then
    lineno=$(grep -n '"github.com/jackc/pgx/' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [TX-2]::pgx package import outside approved boundary — pgx.Tx handling is the exclusive concern of ${POSTGRES_PKG}/ and ${EVENTBUS_PUBLISHER}; use port.TxRunner / port.DelegationRepository instead" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── TX-3: No direct tx.Commit / tx.Rollback ──────────────────────────────────
# Transaction lifecycle (Begin, Commit, Rollback) is owned by pgcommon.RunInTx.
# Direct calls are a sign of hand-rolled transaction management that bypasses
# retry logic, GUC injection, and EventPublisher injection.
while IFS= read -r f; do
  if grep -qP '\btx\.(Commit|Rollback)\s*\(' "$f" 2>/dev/null; then
    lineno=$(grep -nP '\btx\.(Commit|Rollback)\s*\(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [TX-3]::direct tx.Commit/tx.Rollback call — transaction lifecycle is owned by pgcommon.RunInTx (via port.TxRunner); never commit or roll back a pgx.Tx manually" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── OQ-1: pgconn import only in internal/adapter/outbound/postgres/ ───────────
# pgconn is the low-level Postgres protocol package. Only the postgres adapter
# may need it for SQLSTATE inspection. Importing it in http/, service/, or
# domain/ breaks the Clean Architecture dependency rule (outer layers only).
while IFS= read -r f; do
  [[ "$f" == "./${POSTGRES_PKG}/"* || "$f" == "${POSTGRES_PKG}/"* ]] && continue
  if grep -q '"github.com/jackc/pgx/v5/pgconn"' "$f" 2>/dev/null; then
    lineno=$(grep -n '"github.com/jackc/pgx/v5/pgconn"' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [OQ-1]::pgconn imported outside ${POSTGRES_PKG} — use pgcommon.IsConnectionException / pgcommon.IsInsufficientResources / pgcommon.IsPgError helpers so callers never depend on the raw pgconn package" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── OQ-2: pgxpool.Pool / pgxpool.Conn only in postgres/ ──────────────────────
# The concrete *pgcommon.Pool wraps pgxpool.Pool. Accessing the underlying
# pgxpool types directly outside the adapter boundary is a bypass.
while IFS= read -r f; do
  [[ "$f" == "./${POSTGRES_PKG}/"* || "$f" == "${POSTGRES_PKG}/"* ]] && continue
  if grep -q '"github.com/jackc/pgx/v5/pgxpool"' "$f" 2>/dev/null; then
    lineno=$(grep -n '"github.com/jackc/pgx/v5/pgxpool"' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=pgcommon [OQ-2]::pgxpool imported outside ${POSTGRES_PKG} — use *pgcommon.Pool (injected) instead of accessing the underlying pgxpool types directly" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── Result ────────────────────────────────────────────────────────────────────
if [[ $FAIL -eq 0 ]]; then
  echo "pgcommon compliance checks passed."
  echo "  Pool construction : pgcommon.NewPool only in cmd/; no pgxpool/database/sql bypass (PC-1..PC-3) ✓"
  echo "  Configuration     : ConfigFromEnv in cmd/+postgres/db.go; pgmetrics + GUCSetFromContext in both roots (CF-1..CF-3) ✓"
  echo "  Transaction mgmt  : RunInTx only in postgres/db.go; pgx imports in approved boundary; no manual Commit/Rollback (TX-1..TX-3) ✓"
  echo "  Operations        : pgconn + pgxpool imports only in postgres/ adapter (OQ-1..OQ-2) ✓"
fi
exit $FAIL

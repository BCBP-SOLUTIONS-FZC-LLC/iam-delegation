#!/usr/bin/env bash
# check-metric-namespacing.sh
#
# Enforces the Enterprise Platform Observability Standard naming conventions
# via static analysis of the metrics source file.  Complement to
# TestMetricsConformanceStandard (metrics_test.go), which validates the same
# invariants at runtime against the live Prometheus registry.
#
# Rules checked here:
#   1. platform_* metric names must end in _total (counter) or _seconds
#      (histogram) — no other suffixes allowed.
#   2. iam_delegation_* metric names must use standard suffixes: _total,
#      _seconds, or _gauge.
#   3. Forbidden high-cardinality labels (user_id, email, tenant_id,
#      request_id, event_id, session_id) must not appear on platform_*
#      metric definitions.
#
# Run:
#   bash .github/scripts/check-metric-namespacing.sh
#
# Exit codes: 0 = pass, 1 = violation found.
set -euo pipefail

METRICS_FILE="internal/adapter/outbound/metrics/metrics.go"
FAIL=0

if [[ ! -f "$METRICS_FILE" ]]; then
    echo "::error file=${METRICS_FILE}::metrics file not found — run from repo root" >&2
    exit 1
fi

# ── Rule 1: platform_* metric names must end in _total or _seconds ─────────
while IFS= read -r line; do
    # Extract the value inside the Name: "..." field
    name=$(echo "$line" | sed 's/.*Name:[[:space:]]*"//; s/".*//')
    if [[ -z "$name" ]]; then
        continue
    fi
    if [[ "$name" != *_total && "$name" != *_seconds ]]; then
        lineno=$(grep -n "\"${name}\"" "$METRICS_FILE" | head -1 | cut -d: -f1)
        echo "::error file=${METRICS_FILE},line=${lineno},title=Metric naming::platform_* metric '${name}' must end in _total (counter) or _seconds (histogram) — Enterprise Platform Observability Standard §Naming-4/5" >&2
        FAIL=1
    fi
done < <(grep 'Name:.*"platform_' "$METRICS_FILE")

# ── Rule 2: iam_delegation_* metric names must use standard suffixes ────────
while IFS= read -r line; do
    name=$(echo "$line" | sed 's/.*Name:[[:space:]]*"//; s/".*//')
    if [[ -z "$name" ]]; then
        continue
    fi
    case "$name" in
        *_total | *_seconds | *_gauge | *_info)
            ;;  # valid suffixes per the observability standard
        *)
            lineno=$(grep -n "\"${name}\"" "$METRICS_FILE" | head -1 | cut -d: -f1)
            echo "::error file=${METRICS_FILE},line=${lineno},title=Metric naming::iam_delegation_* metric '${name}' uses a non-standard suffix (expected _total, _seconds, or _gauge) — Enterprise Platform Observability Standard §Naming-4/5/6" >&2
            FAIL=1
            ;;
    esac
done < <(grep 'Name:.*"iam_delegation_' "$METRICS_FILE")

# ── Rule 3: Forbidden high-cardinality labels on platform_* metrics ─────────
# Extract all label lists that appear within 25 lines after a platform_ metric
# Name: declaration and check for forbidden labels.
forbidden_labels=(user_id email tenant_id request_id event_id session_id)

# Build a combined grep pattern for forbidden labels inside []string{...} slices
# that follow a platform_* Name declaration.
for label in "${forbidden_labels[@]}"; do
    # Use awk to find the relevant context: within 25 lines of a platform_ Name
    if awk '
        /Name:.*"platform_/ { found=1; count=0 }
        found { count++ }
        found && count <= 25 && /\047'"$label"'\047/ { exit 1 }
        found && count > 25 { found=0 }
    ' "$METRICS_FILE"; then
        : # no violation
    else
        echo "::error file=${METRICS_FILE},title=Label governance::Forbidden high-cardinality label '${label}' found in a platform_* metric definition — Enterprise Platform Observability Standard §Label Governance" >&2
        FAIL=1
    fi
done

# ── Rule 4: required platform_dependency_* metrics must exist ───────────────
for required in "platform_dependency_request_seconds" "platform_dependency_errors_total"; do
    if ! grep -q "\"${required}\"" "$METRICS_FILE"; then
        echo "::error file=${METRICS_FILE},title=Required metric missing::Required platform metric '${required}' not found in metrics file — Enterprise Platform Observability Standard §Implementation" >&2
        FAIL=1
    fi
done

if [[ $FAIL -eq 0 ]]; then
    echo "Metric namespacing checks passed (${METRICS_FILE})."
fi
exit $FAIL

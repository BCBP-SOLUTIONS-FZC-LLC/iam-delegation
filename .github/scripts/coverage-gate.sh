#!/usr/bin/env bash
# Enforces minimum total test coverage.
#
# 70% matches iam-tender-acl's starting baseline (its own sibling and
# closest precedent per the LLD §4 relationship note) — not an aspiration,
# a floor. This service's LLD carries no different coverage bar (§17
# Testing Strategy specifies suite composition, not a numeric threshold), so
# there is no basis to diverge from the sibling default. Ratchet this up
# over time as more tests are added; never lower it to make a failing PR
# pass.
set -euo pipefail

THRESHOLD="${COVERAGE_THRESHOLD:-70}"

test -f coverage.out || {
  echo "::error file=coverage.out,title=Coverage gate::coverage.out missing — run 'make test-ci' before the coverage gate"
  exit 1
}

echo "::group::Coverage report summary"
pct=$(go tool cover -func=coverage.out | tail -1 | awk '{print $3}' | tr -d '%')
echo "Total coverage: ${pct}%"
echo "::endgroup::"

# Emit before the gate check so the value is available even when coverage fails.
echo "pct=${pct}" >> "$GITHUB_OUTPUT"

gate=$(awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN { print (p+0 < t) ? "FAIL" : "OK" }')
if [ "${gate}" = "FAIL" ]; then
  echo "::error file=coverage.out,title=Coverage gate::Coverage is ${pct}% — below the ${THRESHOLD}% threshold. Run 'make cover-func' locally to identify gaps."
  exit 1
fi

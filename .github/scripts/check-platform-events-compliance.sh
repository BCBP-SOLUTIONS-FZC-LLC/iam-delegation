#!/usr/bin/env bash
# check-platform-events-compliance.sh
#
# Enforces that events, outbox writes, outbox lifecycle management, SQS
# consumer wiring, and consumer-side dedup ALL pass through platform-events
# only — no bypass of the approved single-call-site architecture.
#
# Complements the two existing narrower scripts:
#   check-forbidden-events-bypass.sh  — SNS/SQS SDK transport + envelope literals
#   check-outbox-access.sh            — hand-rolled SQL on outbox_events
#
# This script covers what those two miss:
#
#   PUBLISH PATH
#     P-1  outbox.Enqueue must only be called in eventbus/publisher.go
#     P-2  events.NewEnvelope must only be called in eventbus/publisher.go
#     P-3  events.NewSNSPublisher must only be called in cmd/server/wiring.go
#     P-4  outbox.NewRunner must only be called in cmd/server/main.go
#     P-5  outbox.ApplySchema must only be called in postgres/migrate.go
#     P-6  outbox.Runner.PrunePublished must only be called in cmd/server/main.go
#     P-7  events.InitWithRegisterer must be present in BOTH composition roots
#
#   CONSUME PATH
#     C-1  events.NewSQSConsumerWithClient must only be called in consumer/wiring.go
#     C-2  events.WithConsumerCodec must appear in the cascade consumer setup
#
#   DEDUP PATH
#     D-1  SQL against processed_events must only appear in postgres/processed_events.go
#     D-2  IsProcessed/MarkProcessed must only be called through the idempotencyStore
#          interface (consumer/), never from service/ or domain/
#
# Run:
#   bash .github/scripts/check-platform-events-compliance.sh
# Exit codes: 0 = pass, 1 = violation.

set -euo pipefail
FAIL=0

# ── Approved single call sites ───────────────────────────────────────────────
EVENTBUS_PUBLISHER="internal/adapter/outbound/eventbus/publisher.go"
SERVER_WIRING="cmd/server/wiring.go"
SERVER_MAIN="cmd/server/main.go"
RECONCILER_MAIN="cmd/reconciler/main.go"
CONSUMER_WIRING="internal/adapter/inbound/consumer/wiring.go"
PROCESSED_EVENTS_REPO="internal/adapter/outbound/postgres/processed_events.go"
POSTGRES_MIGRATE="internal/adapter/outbound/postgres/migrate.go"

# ── P-1: outbox.Enqueue only in eventbus/publisher.go ────────────────────────
while IFS= read -r f; do
  [[ "$f" == "./$EVENTBUS_PUBLISHER" || "$f" == "$EVENTBUS_PUBLISHER" ]] && continue
  if grep -q 'outbox\.Enqueue(' "$f" 2>/dev/null; then
    lineno=$(grep -n 'outbox\.Enqueue(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Platform-Events [P-1]::outbox.Enqueue called outside ${EVENTBUS_PUBLISHER} — event enqueue must go through eventbus.Publisher.EnqueueCtx only" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── P-2: events.NewEnvelope only in eventbus/publisher.go ────────────────────
while IFS= read -r f; do
  [[ "$f" == "./$EVENTBUS_PUBLISHER" || "$f" == "$EVENTBUS_PUBLISHER" ]] && continue
  if grep -q 'events\.NewEnvelope(' "$f" 2>/dev/null; then
    lineno=$(grep -n 'events\.NewEnvelope(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Platform-Events [P-2]::events.NewEnvelope called outside ${EVENTBUS_PUBLISHER} — envelope construction must go through eventbus.Publisher; use domain.DomainEvent instead" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── P-3: events.NewSNSPublisher only in cmd/server/wiring.go ─────────────────
# grep -v '^\s*//' excludes pure comment lines so doc-comment references
# in codec.go / publisher.go don't trigger a false positive.
while IFS= read -r f; do
  [[ "$f" == "./$SERVER_WIRING" || "$f" == "$SERVER_WIRING" ]] && continue
  if grep -v '^\s*//' "$f" 2>/dev/null | grep -q 'events\.NewSNSPublisher('; then
    lineno=$(grep -v '^\s*//' "$f" | grep -n 'events\.NewSNSPublisher(' | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Platform-Events [P-3]::events.NewSNSPublisher called outside ${SERVER_WIRING} — SNS publisher construction is a composition-root concern; wire it in wiring.go only" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── P-4: outbox.NewRunner only in cmd/server/main.go ─────────────────────────
while IFS= read -r f; do
  [[ "$f" == "./$SERVER_MAIN" || "$f" == "$SERVER_MAIN" ]] && continue
  if grep -q 'outbox\.NewRunner(' "$f" 2>/dev/null; then
    lineno=$(grep -n 'outbox\.NewRunner(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Platform-Events [P-4]::outbox.NewRunner called outside ${SERVER_MAIN} — the outbox runner must be a singleton constructed in the composition root only" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── P-5: outbox.ApplySchema only in postgres/migrate.go ──────────────────────
while IFS= read -r f; do
  [[ "$f" == "./$POSTGRES_MIGRATE" || "$f" == "$POSTGRES_MIGRATE" ]] && continue
  if grep -q 'outbox\.ApplySchema(' "$f" 2>/dev/null; then
    lineno=$(grep -n 'outbox\.ApplySchema(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Platform-Events [P-5]::outbox.ApplySchema called outside ${POSTGRES_MIGRATE} — the outbox schema bootstrap is a migration-layer concern only" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── P-6: PrunePublished only in cmd/server/main.go ───────────────────────────
while IFS= read -r f; do
  [[ "$f" == "./$SERVER_MAIN" || "$f" == "$SERVER_MAIN" ]] && continue
  if grep -q '\.PrunePublished(' "$f" 2>/dev/null; then
    lineno=$(grep -n '\.PrunePublished(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Platform-Events [P-6]::outbox.Runner.PrunePublished called outside ${SERVER_MAIN} — the prune sweep is a composition-root background goroutine; do not duplicate it in reconcilers or adapters" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── P-7: events.InitWithRegisterer must be present in BOTH composition roots ──
for root in "$SERVER_MAIN" "$RECONCILER_MAIN"; do
  if [[ ! -f "$root" ]]; then
    echo "::error file=${root},title=Platform-Events [P-7]::${root} not found — cannot verify events.InitWithRegisterer" >&2
    FAIL=1
  elif ! grep -q 'events\.InitWithRegisterer(' "$root" 2>/dev/null; then
    echo "::error file=${root},title=Platform-Events [P-7]::events.InitWithRegisterer not called in ${root} — platform-events metrics (platform_messages_*, outbox_*) will not be registered on the platform-gincommon registry" >&2
    FAIL=1
  fi
done

# ── C-1: events.NewSQSConsumerWithClient only in consumer/wiring.go ──────────
while IFS= read -r f; do
  [[ "$f" == "./$CONSUMER_WIRING" || "$f" == "$CONSUMER_WIRING" ]] && continue
  if grep -q 'events\.NewSQSConsumerWithClient(' "$f" 2>/dev/null; then
    lineno=$(grep -n 'events\.NewSQSConsumerWithClient(' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Platform-Events [C-1]::events.NewSQSConsumerWithClient called outside ${CONSUMER_WIRING} — consumer construction must be centralised in wiring.go so options like WithConsumerCodec(GlueDecodeCodec{}) cannot be accidentally omitted" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── C-2: events.WithConsumerCodec must appear in the cascade consumer setup ───
if ! grep -q 'events\.WithConsumerCodec(' "$CONSUMER_WIRING" 2>/dev/null && \
   ! grep -q 'events\.WithConsumerCodec(' "$SERVER_MAIN" 2>/dev/null; then
  echo "::error file=${CONSUMER_WIRING},title=Platform-Events [C-2]::events.WithConsumerCodec not found in consumer wiring — GlueDecodeCodec must be registered so Glue-encoded messages from iam-org-membership are decoded correctly before the handler receives them" >&2
  FAIL=1
fi

# ── D-1: SQL against processed_events only in postgres/processed_events.go ────
# Uses Python to scan backtick SQL literals (same approach as check-outbox-access.sh)
# so multi-line SQL strings are matched without false-positives on doc comments.
python3 - <<'PYEOF'
import re, sys
from pathlib import Path

ROOTS = ["internal", "cmd", "pkg"]
ALLOWED = "internal/adapter/outbound/postgres/processed_events.go"
SQL_RE  = re.compile(r'\b(from|into|update|delete\s+from)\s+processed_events\b', re.IGNORECASE)
BT_RE   = re.compile(r'`([^`]*)`', re.DOTALL)

violations = []
for root in ROOTS:
    for path in Path(root).rglob("*.go"):
        posix = path.as_posix()
        if posix.endswith("_test.go"):
            continue
        if posix == ALLOWED:
            continue
        text = path.read_text(encoding="utf-8")
        for m in BT_RE.finditer(text):
            if SQL_RE.search(m.group(1)):
                line = text.count("\n", 0, m.start()) + 1
                violations.append(f"{posix}:{line}")

if violations:
    for v in violations:
        print(f"::error file={v}::hand-rolled SQL against processed_events outside {ALLOWED} — use ProcessedEventsRepository (IsProcessed / MarkProcessed / Prune); never bypass the dedup ledger with a direct query")
    sys.exit(1)
PYEOF

# ── D-2: IsProcessed/MarkProcessed only called from consumer/ (not service/ or domain/) ─
for forbidden_dir in "internal/core/service" "internal/core/domain"; do
  if find "$forbidden_dir" -name "*.go" ! -name "*_test.go" \
       -exec grep -l 'IsProcessed\|MarkProcessed' {} \; 2>/dev/null | grep -q .; then
    echo "::error file=${forbidden_dir},title=Platform-Events [D-2]::IsProcessed/MarkProcessed called from ${forbidden_dir} — dedup ledger access must stay in the consumer layer (internal/adapter/inbound/consumer/); core domain/service code must remain unaware of the SQS idempotency mechanism" >&2
    FAIL=1
  fi
done

# ── Result ────────────────────────────────────────────────────────────────────
if [[ $FAIL -eq 0 ]]; then
  echo "Platform-events compliance checks passed."
  echo "  Publish path : outbox.Enqueue → eventbus/publisher.go only (P-1..P-7) ✓"
  echo "  Consume path : NewSQSConsumerWithClient → consumer/wiring.go only; GlueDecodeCodec wired (C-1..C-2) ✓"
  echo "  Dedup path   : processed_events SQL → postgres/processed_events.go only; IsProcessed/MarkProcessed outside core (D-1..D-2) ✓"
fi
exit $FAIL

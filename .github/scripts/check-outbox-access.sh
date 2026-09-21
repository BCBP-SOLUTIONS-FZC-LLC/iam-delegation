#!/usr/bin/env bash
# Enforces that outbox_events is never written/read via hand-rolled SQL in
# production code — every mutation must go through platform-events'
# outbox.Enqueue (write) / outbox.Runner.PrunePublished (delete) / internal
# poll loop (read), never a local INSERT/UPDATE/DELETE/SELECT. Ported from
# iam-org-membership's identical script, whose header rationale is a real
# historical violation in that repo (a hand-rolled batched DELETE against
# outbox_events that duplicated outbox.Runner.PrunePublished, since
# removed) — this service has never hit that failure mode, but the same
# architectural gap (nothing stops someone from adding one) applies here
# too, and this repo already runs the analogous
# check-forbidden-events-bypass.sh for the AWS-SDK/Envelope-level bypass —
# this closes the SQL-level one.
#
# Scope: internal/, cmd/, pkg/ only — colocated *_test.go files are
# deliberately excluded, since tests legitimately assert on outbox_events
# state directly (e.g. "did Enqueue actually write a row") and that is not
# a bypass of anything production code is supposed to funnel through.
# Migration .sql files are out of scope too (this script only walks *.go
# files) — the GRANT on outbox_events in 000001_schema.up.sql is necessary
# DDL/DCL, not a data-access bypass.
#
# Detection: scans Go raw-string (backtick) literals only, not plain prose —
# a naive whole-line text grep for "into outbox_events"/"from outbox_events"
# false-positives on ordinary doc comments describing the mechanism (e.g.
# "Enqueue... inserts it into outbox_events on the transaction..."), which
# this repo's own comments do use. Scanning backtick literals specifically
# (DOTALL, so a `\n`-continued multi-line SQL string is still one match)
# sidesteps that without needing a full Go AST parse.
set -euo pipefail

python3 - <<'PYEOF'
import re
import sys
from pathlib import Path

ROOTS = ["internal", "cmd", "pkg"]
ALLOWED_DIR = "internal/adapter/outbound/eventbus"
SQL_RE = re.compile(r'\b(from|into|update)\s+outbox_events\b', re.IGNORECASE)
BACKTICK_RE = re.compile(r'`([^`]*)`', re.DOTALL)

violations = []
for root in ROOTS:
    for path in Path(root).rglob("*.go"):
        posix = path.as_posix()
        if posix.endswith("_test.go"):
            continue
        if posix.startswith(ALLOWED_DIR):
            continue
        text = path.read_text(encoding="utf-8")
        for m in BACKTICK_RE.finditer(text):
            literal = m.group(1)
            if SQL_RE.search(literal):
                line = text.count("\n", 0, m.start()) + 1
                violations.append(f"{posix}:{line}")

if violations:
    for v in violations:
        print(f"::error file={v}::hand-rolled SQL against outbox_events outside {ALLOWED_DIR} — use platform-events (outbox.Enqueue / outbox.Runner.PrunePublished), not a local INSERT/UPDATE/DELETE/SELECT")
    print(f"\n{len(violations)} violation(s). See .github/scripts/check-outbox-access.sh for rationale.")
    sys.exit(1)

print("  ✔  no hand-rolled SQL against outbox_events outside platform-events")
PYEOF

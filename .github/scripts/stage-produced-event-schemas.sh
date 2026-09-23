#!/usr/bin/env bash
# Copy this service's produced-event schema files into DEST, renamed to
# their PascalCase Glue schema name.
#
# schema-gov register (0.4) names each Glue schema after the FILE STEM, but
# internal/eventschema/*.json are snake_case (delegation_started.json, ...)
# while GlueCodec resolves the PascalCase event-type constants
# (DelegationStarted, ...) — see codec.go. Registering the raw directory
# would create snake_case schemas the service never looks up, and every pod
# would then fail startup (NewGlueCodec: "isn't registered yet"). Both
# schema-registry.yml register steps and `make schema-register` therefore
# register this staged copy instead, and so do usage-check and the diff
# steps, which also take the name from the stem. Only validate and the
# extract sync check read internal/eventschema directly.
#
# internal/eventschema also holds the CONSUMED schemas (MembershipRevoked,
# TenantMembershipsPurged, UserUpdated — other services' events, DLG-D51).
# They are skipped here by name, so they are never registered in this
# service's registry or usage-classified; any file that is neither mapped
# nor listed as consumed fails the script, so a new schema can't slip
# through unregistered.
#
# The mapping mirrors SCHEMA_NAME_MAP in schema-registry.yml's diff steps. A
# `case` statement (not a bash 4+ associative array) keeps this runnable
# under macOS's default /bin/bash 3.2 via `make schema-register`. A new
# schema needs one line added below.
#
# Usage: stage-produced-event-schemas.sh DEST
set -euo pipefail

DEST="${1:?destination directory required}"
SRC="${SCHEMA_SRC:-internal/eventschema}"

name_for() {
  case "$1" in
    # consumed (owned + registered by their producers) — never staged
    membership_revoked|tenant_memberships_purged|user_updated) echo CONSUMED ;;
    delegation_started)              echo DelegationStarted ;;
    delegation_ended)                echo DelegationEnded ;;
    delegation_review_requested)     echo DelegationReviewRequested ;;
    delegation_escalation_requested) echo DelegationEscalationRequested ;;
    *)                               echo "" ;;
  esac
}

mkdir -p "$DEST"

shopt -s nullglob
copied=0
for file in "$SRC"/*.json; do
  stem=$(basename "$file" .json)
  name=$(name_for "$stem")
  if [ -z "$name" ]; then
    echo "no Glue name mapped for $file — add it to name_for in $0 (or to its consumed list)" >&2
    exit 1
  fi
  [ "$name" = CONSUMED ] && continue
  cp "$file" "$DEST/${name}.json"
  copied=$((copied + 1))
done

if [ "$copied" -eq 0 ]; then
  echo "no produced schemas staged into $DEST" >&2
  exit 1
fi
echo "staged $copied produced schema(s) into $DEST"

#!/usr/bin/env bash
# Downloads SLSA provenance attestation from GHCR and writes the given
# output file. Invoked once per image (server / reconciler) by release.yml,
# with OUTPUT_FILE set per-leg so both provenance files can coexist.
set -euo pipefail
set -o pipefail

: "${IMAGE_NAME:?IMAGE_NAME is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"
OUTPUT_FILE="${OUTPUT_FILE:-provenance.slsa.json}"

IMAGE_REF="${IMAGE_NAME}@${IMAGE_DIGEST}"

if ! cosign download attestation \
  --predicate-type=https://slsa.dev/provenance/v1 \
  "${IMAGE_REF}" \
  | jq -er 'if type == "array" then .[0].payload else .payload end' \
  | base64 -d \
  | jq . \
  | tee "${OUTPUT_FILE}" >/dev/null; then
  echo "::error title=Provenance export::SLSA provenance attestation not found on ${IMAGE_REF}"
  exit 1
fi

echo "  ✔  exported SLSA provenance to ${OUTPUT_FILE}"

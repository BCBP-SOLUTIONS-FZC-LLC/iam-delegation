#!/usr/bin/env bash
# Image size + startup gate smoke tests for a CI-built Docker image.
# Invoked by ci.yml once per matrix leg (server / reconciler) with
# IMAGE_TAG and BINARY set — keeps shell operators out of inline YAML run
# blocks.
set -euo pipefail

: "${IMAGE_TAG:?IMAGE_TAG is required}"
: "${BINARY:?BINARY is required}"

echo "::group::Image size check (${BINARY}, linux/amd64)"
# arm64 is typically within ±5 MB of amd64 for a distroless Go binary; the
# limit is deliberately generous so it catches regressions (e.g.
# accidentally COPYing third_party build artefacts or embedding test
# assets), not normal arch variance.
MAX_MB=200
size=$(docker image inspect "${IMAGE_TAG}" --format='{{.Size}}')
mb=$((size / 1024 / 1024))
echo "Image size: ${mb} MB (limit: ${MAX_MB} MB)"
[ "${mb}" -le "${MAX_MB}" ] &
P1=$!
echo "::endgroup::"

echo "::group::Startup gate (${BINARY})"
# The binary must exit non-zero on missing required env vars, proving its
# config-loading validation actually fires (DATABASE_URL is required for
# both binaries; server additionally requires SQS_QUEUE_URL).
# timeout 10s kills the container if it hangs instead of exiting.
exit_code=0
timeout 10s docker run --rm "${IMAGE_TAG}" 2>/dev/null || exit_code=$?
echo "Container exit code: ${exit_code} (expected non-zero)"
[ "${exit_code}" -ne 0 ] &
P2=$!
echo "::endgroup::"

wait $P1 || {
  echo "::error title=Image size (${BINARY})::Image is ${mb} MB, exceeds ${MAX_MB} MB limit — check COPY instructions for accidental inclusions"
  exit 1
}
wait $P2 || {
  echo "::error title=Startup gate (${BINARY})::Binary exited 0 on missing required env vars — config loading must exit non-zero"
  exit 1
}

{
  echo "### Smoke test results — ${BINARY}"
  echo "- ✅ Startup gate: binary exits ${exit_code} on missing required env vars"
  echo "- 📦 Image size (linux/amd64): **${mb} MB** (limit: ${MAX_MB} MB)"
} >> "$GITHUB_STEP_SUMMARY"

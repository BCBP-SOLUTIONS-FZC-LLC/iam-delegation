#!/usr/bin/env bash
# Creates the GitHub release with versioned assets attached for both
# binaries/images (server + reconciler).
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${SERVER_IMAGE_DIGEST:?SERVER_IMAGE_DIGEST is required}"
: "${RECONCILER_IMAGE_DIGEST:?RECONCILER_IMAGE_DIGEST is required}"

test -f release-asset-server.name || {
  echo "::error::release-asset-server.name missing — run prepare-release-binary.sh first"
  exit 1
}
test -f release-asset-reconciler.name || {
  echo "::error::release-asset-reconciler.name missing — run prepare-release-binary.sh first"
  exit 1
}

SERVER_ASSET=$(cat release-asset-server.name)
RECONCILER_ASSET=$(cat release-asset-reconciler.name)

for f in release-notes.md \
         "$SERVER_ASSET" "${SERVER_ASSET}.sha256" \
         "$RECONCILER_ASSET" "${RECONCILER_ASSET}.sha256" \
         provenance-server.slsa.json provenance-reconciler.slsa.json \
         sbom-server.cyclonedx.json sbom-reconciler.cyclonedx.json; do
  test -f "$f" || {
    echo "::error::Release asset missing: ${f}"
    exit 1
  }
done

# Aggregate per-binary checksums into a single verifiable file.
# Format matches `sha256sum --check checksums.txt`.
sha256sum iam-delegation-server_* iam-delegation-reconciler_* | grep -v '\.sha256$' > checksums.txt

PRERELEASE_FLAG=""
if echo "$RELEASE_TAG" | grep -q -- '-'; then
  PRERELEASE_FLAG="--prerelease"
fi

DIGEST_SHORT="${SERVER_IMAGE_DIGEST#sha256:}"
DIGEST_SHORT="${DIGEST_SHORT:0:12}"
RELEASE_TITLE="${RELEASE_TAG} · server sha256:${DIGEST_SHORT}"

gh release create "$RELEASE_TAG" \
  --title "$RELEASE_TITLE" \
  --notes-file release-notes.md \
  "$SERVER_ASSET" "${SERVER_ASSET}.sha256" \
  "$RECONCILER_ASSET" "${RECONCILER_ASSET}.sha256" \
  checksums.txt \
  provenance-server.slsa.json provenance-reconciler.slsa.json \
  sbom-server.cyclonedx.json sbom-reconciler.cyclonedx.json \
  $PRERELEASE_FLAG

#!/usr/bin/env bash
# Creates the GitHub release with versioned assets attached: the two release
# binaries (server + reconciler — still built and checksummed separately,
# LLD §16.1) and the one image's SBOM/provenance (server + reconciler ship
# in a single container image, per the repo-root Dockerfile).
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"

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
         provenance.slsa.json \
         sbom.cyclonedx.json; do
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

DIGEST_SHORT="${IMAGE_DIGEST#sha256:}"
DIGEST_SHORT="${DIGEST_SHORT:0:12}"
RELEASE_TITLE="${RELEASE_TAG} · sha256:${DIGEST_SHORT}"

gh release create "$RELEASE_TAG" \
  --title "$RELEASE_TITLE" \
  --notes-file release-notes.md \
  "$SERVER_ASSET" "${SERVER_ASSET}.sha256" \
  "$RECONCILER_ASSET" "${RECONCILER_ASSET}.sha256" \
  checksums.txt \
  provenance.slsa.json \
  sbom.cyclonedx.json \
  $PRERELEASE_FLAG

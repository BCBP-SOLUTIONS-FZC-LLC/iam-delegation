#!/usr/bin/env bash
# Extracts the RELEASE_TAG section from CHANGELOG.md and appends metadata
# for the published image (one image carries both binaries — server +
# reconciler, per the repo-root Dockerfile).
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${IMAGE_NAME:?IMAGE_NAME is required}"
: "${IMAGE_VERSION:?IMAGE_VERSION is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"

VERSION="${RELEASE_TAG#v}"
awk "/^## \\[${VERSION}\\]/{found=1; next} /^## \\[/{if(found) exit} found{print}" CHANGELOG.md \
  | sed -e 's/^[[:space:]]*$//' \
  | tee release-notes.md | true

if [ ! -s release-notes.md ]; then
  echo "::error file=CHANGELOG.md::Missing entry for ${VERSION}"
  exit 1
fi

{
  cat release-notes.md
  echo ""
  echo "---"
  echo "**Image:** \`${IMAGE_NAME}:${IMAGE_VERSION}\` (carries both /iam-delegation-server and /iam-delegation-reconciler)"
  echo "**Digest:** \`${IMAGE_DIGEST}\`"
} | tee release-notes.md.tmp | true
mv release-notes.md.tmp release-notes.md

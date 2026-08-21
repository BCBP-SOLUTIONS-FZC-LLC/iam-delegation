#!/usr/bin/env bash
# Extracts the RELEASE_TAG section from CHANGELOG.md and appends metadata
# for both published images (server + reconciler — this repo ships two
# binaries/images per LLD §16.1, unlike single-binary iam-tender-acl).
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${SERVER_IMAGE_NAME:?SERVER_IMAGE_NAME is required}"
: "${SERVER_IMAGE_VERSION:?SERVER_IMAGE_VERSION is required}"
: "${SERVER_IMAGE_DIGEST:?SERVER_IMAGE_DIGEST is required}"
: "${RECONCILER_IMAGE_NAME:?RECONCILER_IMAGE_NAME is required}"
: "${RECONCILER_IMAGE_VERSION:?RECONCILER_IMAGE_VERSION is required}"
: "${RECONCILER_IMAGE_DIGEST:?RECONCILER_IMAGE_DIGEST is required}"

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
  echo "**Server image:** \`${SERVER_IMAGE_NAME}:${SERVER_IMAGE_VERSION}\`"
  echo "**Server digest:** \`${SERVER_IMAGE_DIGEST}\`"
  echo "**Reconciler image:** \`${RECONCILER_IMAGE_NAME}:${RECONCILER_IMAGE_VERSION}\`"
  echo "**Reconciler digest:** \`${RECONCILER_IMAGE_DIGEST}\`"
} | tee release-notes.md.tmp | true
mv release-notes.md.tmp release-notes.md

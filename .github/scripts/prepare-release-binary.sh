#!/usr/bin/env bash
# Cross-compiles both release binaries (server + reconciler — LLD §16.1:
# a "server" Deployment plus a "reconciler" running the three CronJobs) for
# all target platforms and writes per-binary checksums. The artifact
# directory is the working directory when this runs.
#
# This service ships two binaries:
#   cmd/server      -- HTTP API (DLG-1..7, DLG-I1..I4) + cascade SQS consumer
#   cmd/reconciler  -- the three CronJob entry points (jobs/delegation_*.go)
#
# Produces for each platform x binary:
#   iam-delegation-{server,reconciler}_{version}_{os}_{arch}[.exe]
#   iam-delegation-{server,reconciler}_{version}_{os}_{arch}[.exe].sha256
#
# The caller (create-github-release.sh) aggregates the .sha256 files into
# a single checksums.txt.
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"

TARGETS=(
  "linux   amd64"
  "linux   arm64"
  "darwin  amd64"
  "darwin  arm64"
  "windows amd64"
)

BINARIES=(
  "server      ./cmd/server"
  "reconciler  ./cmd/reconciler"
)

echo "Building release binaries for tag ${RELEASE_TAG}"

for binary in "${BINARIES[@]}"; do
  read -r BIN_NAME BIN_PKG <<< "$binary"

  for target in "${TARGETS[@]}"; do
    read -r GOOS GOARCH <<< "$target"
    EXT=""
    [ "${GOOS}" = "windows" ] && EXT=".exe"
    ASSET_NAME="iam-delegation-${BIN_NAME}_${RELEASE_TAG}_${GOOS}_${GOARCH}${EXT}"

    echo "  → ${BIN_NAME} ${GOOS}/${GOARCH}"
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
      go build -trimpath \
        -ldflags "-s -w -X main.buildVersion=${RELEASE_TAG}" \
        -o "${ASSET_NAME}" \
        "${BIN_PKG}"

    chmod +x "${ASSET_NAME}"
    sha256sum "${ASSET_NAME}" > "${ASSET_NAME}.sha256"
    echo "    ✔  ${ASSET_NAME}"
  done
done

# Write the primary asset names (linux/amd64) for downstream steps that
# need a canonical file reference per binary.
echo "iam-delegation-server_${RELEASE_TAG}_linux_amd64" > release-asset-server.name
echo "iam-delegation-reconciler_${RELEASE_TAG}_linux_amd64" > release-asset-reconciler.name
echo "All binaries prepared."

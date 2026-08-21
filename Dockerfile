# syntax=docker/dockerfile:1.7
#
# iam-delegation
#
# Multi-stage build producing a minimal, non-root, distroless runtime image
# carrying BOTH binaries this service ships (LLD §16.1):
#   /iam-delegation-server      HTTP API (DLG-1..7, DLG-I1..I4) + the
#                               delegation-cascade-q SQS consumer, in one
#                               process via errgroup — the image's default
#                               ENTRYPOINT.
#   /iam-delegation-reconciler  the three CronJob entry points
#                               (delegation-expiry/-review/-cleanup,
#                               jobs/delegation_*.go), selected at
#                               invocation time via --job=<name>. No
#                               long-running HTTP/metrics server, no ports
#                               exposed for it.
#
# One image, mirroring iam-user-profile's Dockerfile: the Deployment runs
# it unmodified; each CronJob template
# (deploy/helm/iam-delegation/templates/cronjob-*.yaml) overrides `command`
# to invoke /iam-delegation-reconciler --job=<name> against the SAME image
# reference (no separate reconciler image/GHCR repo to build, tag, or scan).

########################################
# Stage: builder
########################################
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS builder

ARG BUILD_VERSION=dev
ARG SOURCE_DATE_EPOCH
ENV SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}

WORKDIR /src

# Copy go.mod/go.sum first so dependency resolution is cached independently
# of application source changes.
COPY go.mod go.sum ./

# platform-gincommon, platform-pgcommon, and platform-events are private
# github.com/BCBP-SOLUTIONS-FZC-LLC modules (not vendored locally), fetched
# via git using a short-lived token — same secret-handling pattern as every
# sibling IAM service's Dockerfile.
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*"
ENV GONOSUMDB="github.com/BCBP-SOLUTIONS-FZC-LLC/*"

RUN --mount=type=secret,id=go_private_token \
    TOKEN=$(cat /run/secrets/go_private_token 2>/dev/null || true) && \
    if [ -z "$TOKEN" ]; then echo "ERROR: go_private_token secret is missing or empty — pass --secret id=go_private_token,src=<token-file>"; exit 1; fi && \
    git config --global url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/" && \
    go mod download && \
    git config --global --unset url."https://x-access-token:${TOKEN}@github.com/".insteadOf

# Now copy the remainder of the source tree.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*" \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-delegation-server \
    ./cmd/server && \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-delegation-reconciler \
    ./cmd/reconciler

########################################
# Stage: runtime
########################################
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a AS runtime

ARG BUILD_VERSION=dev
ENV BUILD_VERSION=${BUILD_VERSION}

LABEL org.opencontainers.image.title="iam-delegation" \
      org.opencontainers.image.description="Delegation Service — HTTP API (DLG-1..7, DLG-I1..I4) + cascade SQS consumer + the three reconciler CronJobs — ADR-0008 Wave 4 extraction from iam-org-membership" \
      org.opencontainers.image.source="https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation" \
      org.opencontainers.image.vendor="BCBP Solutions" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      org.opencontainers.image.revision="${BUILD_VERSION}"

WORKDIR /

COPY --from=builder /out/iam-delegation-server /iam-delegation-server
COPY --from=builder /out/iam-delegation-reconciler /iam-delegation-reconciler

USER nonroot:nonroot

EXPOSE 8080 9090

ENTRYPOINT ["/iam-delegation-server"]

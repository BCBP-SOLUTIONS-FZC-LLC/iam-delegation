SHELL := /bin/sh
.SHELLFLAGS := -eu -c

# ---------------------------------------------------------------------------
# iam-delegation build tooling
#
# Structured to mirror iam-tender-acl's / iam-group-mapping's Makefile
# conventions (config block, .env sourcing, tidy/fmt-check/vet/lint/test-ci
# pipeline, per-suite test targets, coverage, docker-up/down, godoc,
# pin-base-images) so switching between IAM repos feels the same.
#
# Unlike iam-tender-acl (single binary), this repo ships TWO binaries (LLD
# §16.1): cmd/server (HTTP API DLG-1..7/DLG-I1..I4 + the delegation-cascade-q
# SQS consumer) and cmd/reconciler (the three CronJob entry points under
# cmd/reconciler/jobs — delegation_expiry.go, delegation_review.go,
# delegation_cleanup.go). `make build` builds both.
# ---------------------------------------------------------------------------

# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-delegation
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# platform-gincommon/platform-pgcommon/platform-events are private
# github.com/BCBP-SOLUTIONS-FZC-LLC modules, not vendored — fetched via git
# using SSH or a GO_PRIVATE_TOKEN-backed credential helper.
export GOPRIVATE ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

export APP_NAME APP_ENV BUILD_VERSION

MODULE          := github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation
SERVER_BINARY   := iam-delegation-server
SERVER_CMD_PKG  := ./cmd/server
RECONCILER_BINARY  := iam-delegation-reconciler
RECONCILER_CMD_PKG := ./cmd/reconciler
BUILD_DIR       := bin
GOFLAGS         ?=
LDFLAGS         := -s -w -X main.buildVersion=$(BUILD_VERSION)
# One image carries both binaries (single Dockerfile — see its header
# comment); the Deployment runs it unmodified, each CronJob overrides
# `command` to invoke /iam-delegation-reconciler against the same image.
IMAGE ?= iam-delegation:latest

ALL_TEST_TAGS := integration,rls,e2e

# Unlike iam-tender-acl's test/{integration,rls,e2e}/+build-tag split, every
# test in this repo is colocated white-box (package-internal) —
# e.g. internal/adapter/outbound/postgres/{delegation_repository,
# settings_repository,rls}_test.go spin up their own Postgres testcontainer
# directly, no //go:build gate, no external test/ tree. Plain "./..." IS the
# complete suite (unit + Postgres/testcontainer integration + the full
# §17.5 RLS matrix all run together); there is nothing separate for the
# integration/rls/e2e targets below to select via -tags, so they alias to
# the same full run rather than pointing at directories that don't exist
# here. -tags=$(ALL_TEST_TAGS) is consequently a no-op (no file declares
# those build tags) but harmless, and kept so `vet`/`lint`/`build` still
# exercise the tagged-build code path in case a future suite adds one.
TEST_UNIT_PKGS        := ./...
TEST_INTEGRATION_PKGS := ./...
TEST_RLS_PKGS         := ./...
TEST_E2E_PKGS         := ./...

COVER_PKG_LIST := $(shell $(GO) list ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

# The migration role MAY have BYPASSRLS; in docker-compose that's simply the
# postgres superuser. Production points this at a dedicated, more
# privileged migration role (delegation_migrator) provisioned by
# infrastructure tooling, never at the runtime app role (delegation_app).
DATABASE_MIGRATION_URL ?= postgres://delegation:delegation@localhost:5432/delegation?sslmode=disable
# Migrations live under the outbound Postgres adapter (LLD §7.4), not a
# top-level migrations/ directory.
MIGRATIONS_DIR := internal/adapter/outbound/postgres/migrations
MIGRATE_IMAGE  := migrate/migrate:v4.17.1

.PHONY: all setup install-hooks help godoc pin-base-images \
        tidy fmt fmt-check vet mod-verify vuln-check lint \
        build build-server build-reconciler run run-server run-reconciler clean \
        test test-unit test-integration test-rls test-e2e \
        _test-unit-plain _test-integration-plain _test-rls-plain \
        race _race-unit _race-integration _race-rls _race-e2e \
        test-ci _test-unit-cov _test-integration-cov _test-rls-cov _merge-coverage \
        cover cover-func \
        docker-build docker-push docker-up docker-up-pro docker-down compose-up compose-down \
        migrate-up migrate-down migrate-create \
        swag swag-check arch-lint generate ci

all: build

# -----------------------------
# SETUP
# -----------------------------

setup:
	@test -f .env || cp .env.example .env
	@mkdir -p .git/hooks
	@test -f .githooks/pre-commit && cp .githooks/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit || true
	@echo "Environment ready (.env)"

install-hooks:
	@mkdir -p .git/hooks
	@cp .githooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed git hooks"

godoc:
	@echo "Starting pkgsite at http://localhost:8080 — press Ctrl-C to stop"
	$(GO) run golang.org/x/pkgsite/cmd/pkgsite@latest -open .

# pin-base-images: fetch and pin the current SHA digests for the
# Dockerfile's base images. Writes the digests both to the Dockerfile FROM
# lines and to .docker-digests (a checked-in provenance record).
pin-base-images:
	@echo "Fetching SHA digests for Dockerfile base images..."
	@GOLANG_DIGEST=$$(docker buildx imagetools inspect golang:1.26.6-bookworm --format '{{.Manifest.Digest}}') && \
	 DISTROLESS_DIGEST=$$(docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}') && \
	 sed -E -i.bak \
	   -e "s|FROM golang:1\.26\.6-bookworm(@sha256:[a-f0-9]+)?|FROM golang:1.26.6-bookworm@$$GOLANG_DIGEST|" \
	   -e "s|FROM gcr\.io/distroless/static-debian12:nonroot(@sha256:[a-f0-9]+)?|FROM gcr.io/distroless/static-debian12:nonroot@$$DISTROLESS_DIGEST|" \
	   Dockerfile && rm -f Dockerfile.bak && \
	 echo "golang:1.26.6-bookworm $$GOLANG_DIGEST" > .docker-digests && \
	 echo "gcr.io/distroless/static-debian12:nonroot $$DISTROLESS_DIGEST" >> .docker-digests && \
	 echo "Digests written to .docker-digests — commit Dockerfile and .docker-digests"

help:
	@echo "Available commands:"
	@echo "  make setup            - copy .env.example to .env if missing, install git hooks"
	@echo "  make install-hooks    - install .githooks/pre-commit into .git/hooks"
	@echo "  make tidy             - go mod tidy"
	@echo "  make fmt              - format source with gofmt"
	@echo "  make fmt-check        - verify gofmt formatting (mirrors CI)"
	@echo "  make vet              - go vet (default build + every test build tag)"
	@echo "  make lint             - run golangci-lint (via go tool)"
	@echo "  make mod-verify       - go mod verify"
	@echo "  make vuln-check       - govulncheck on cmd/ + internal/ + pkg/"
	@echo "  make test             - unit + integration + rls tests, in parallel (requires Docker)"
	@echo "  make test-unit        - unit tests only (no Docker required)"
	@echo "  make test-integration - integration tests: Postgres+Valkey+SQS-compatible via Testcontainers (requires Docker)"
	@echo "  make test-rls         - Postgres Row-Level-Security tests via Testcontainers (requires Docker)"
	@echo "  make test-e2e         - end-to-end tests: Postgres+Valkey+SQS-compatible via Testcontainers (requires Docker)"
	@echo "  make race             - all four suites with -race, in parallel (requires Docker)"
	@echo "  make test-ci          - race + coverage, merged into coverage.out (used in CI, requires Docker)"
	@echo "  make cover            - coverage HTML report"
	@echo "  make cover-func       - coverage summary by function"
	@echo "  make run              - run the server locally (go run cmd/server), sourcing .env if present"
	@echo "  make run-server       - alias for 'make run'"
	@echo "  make run-reconciler   - run the reconciler locally (go run cmd/reconciler); pass JOB=delegation-activation|delegation-expiry|delegation-review|delegation-cleanup"
	@echo "  make build            - compile both binaries (iam-delegation-server, iam-delegation-reconciler) to bin/"
	@echo "  make build-server     - compile only cmd/server"
	@echo "  make build-reconciler - compile only cmd/reconciler"
	@echo "  make arch-lint        - run go-arch-lint against .go-arch-lint.yml"
	@echo "  make swag             - regenerate docs/swagger/ from handler annotations"
	@echo "  make swag-check       - fail if Swagger regeneration would change docs/swagger/ (CI drift gate)"
	@echo "  make ci               - tidy + fmt-check + vet + lint + arch-lint + test-ci + build (matches 'make ci' in CI docs)"
	@echo "  make docker-build     - build the container image (IMAGE to override, carries both binaries)"
	@echo "  make docker-push      - push the container image"
	@echo "  make docker-up        - start local Postgres + Valkey + LocalStack community (no token needed)"
	@echo "  make docker-up-pro    - start local Postgres + Valkey + LocalStack Pro (Glue Schema Registry; requires LOCALSTACK_AUTH_TOKEN in .env)"
	@echo "  make docker-down      - stop containers started by docker-up/compose-up"
	@echo "  make compose-up       - start the full local dev stack (postgres, valkey, localstack, server — self-migrates at startup)"
	@echo "  make compose-down     - stop and remove the local dev stack, including volumes"
	@echo "  make migrate-up       - apply all pending migrations against DATABASE_MIGRATION_URL (manual/CI use; the server binary also self-migrates at startup)"
	@echo "  make migrate-down     - roll back one migration against DATABASE_MIGRATION_URL"
	@echo "  make migrate-create   - create a new migration pair (NAME=add_foo_table)"
	@echo "  make godoc            - serve local godoc/pkgsite at http://localhost:8080"
	@echo "  make pin-base-images  - fetch + pin SHA digests for the Dockerfile's base images"
	@echo "  make generate         - run any go:generate directives (currently none)"
	@echo "  make clean            - remove build artifacts and coverage output"
	@echo ""
	@echo "Schema governance (platform-schemagov 0.4):"
	@echo "  make schema-pull      - pull the schema-gov Docker image"
	@echo "  make schema-validate  - validate AsyncAPI + event schemas — 8 passes (no AWS required)"
	@echo "  make schema-diff      - diff two schema files: CURRENT=<path> PROPOSED=<path>"
	@echo "  make schema-register  - register event schemas to Glue (requires AWS/LocalStack)"
	@echo "  make schema-verify    - pre-deploy check: fail if PascalCase schemas are missing (requires AWS)"
	@echo "  make schema-prune     - dry-run: list orphaned Glue schemas (requires AWS)"

# -----------------------------
# GO BASICS
# -----------------------------

tidy:
	$(GO) mod tidy

fmt:
	gofmt -s -w .

fmt-check:
	@unformatted=$$(gofmt -l cmd/ internal/ pkg/ test/ 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "FAIL: unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "gofmt: all files formatted"

vet:
	$(GO) vet ./...
	$(GO) vet -tags=$(ALL_TEST_TAGS) ./...

mod-verify:
	$(GO) mod verify

vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./cmd/... ./internal/... ./pkg/...

lint:
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=$(ALL_TEST_TAGS) ./...

# Architecture layering (LLD §6/§6.2) — see .go-arch-lint.yml.
arch-lint:
	bash .github/scripts/arch-lint.sh

# -----------------------------
# BUILD / RUN
# -----------------------------

build: build-server build-reconciler
	$(GO) build -tags=$(ALL_TEST_TAGS) ./...

build-server:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(SERVER_BINARY) $(SERVER_CMD_PKG)

build-reconciler:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(RECONCILER_BINARY) $(RECONCILER_CMD_PKG)

run: run-server

run-server:
	@-lsof -ti :$${HTTP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	@if [ -f .env ]; then set -a && . ./.env && set +a && $(GO) run $(SERVER_CMD_PKG); else $(GO) run $(SERVER_CMD_PKG); fi

# JOB selects which of the four CronJob entry points to run locally
# (delegation-activation | delegation-expiry | delegation-review |
# delegation-cleanup — see deploy/helm/iam-delegation/templates/cronjob-*.yaml
# for the convention).
run-reconciler:
	@if [ -f .env ]; then set -a && . ./.env && set +a && $(GO) run $(RECONCILER_CMD_PKG) --job=$${JOB:-delegation-expiry}; else $(GO) run $(RECONCILER_CMD_PKG) --job=$${JOB:-delegation-expiry}; fi

clean:
	rm -rf $(BUILD_DIR) .coverage
	rm -f coverage.out coverage.html

# -----------------------------
# TESTS
# -----------------------------

.coverage:
	@mkdir -p .coverage

test:
	$(MAKE) -j3 _test-unit-plain _test-integration-plain _test-rls-plain

_test-unit-plain:
	$(GO) test $(TEST_UNIT_PKGS) -count=1 -timeout 120s
_test-integration-plain:
	$(GO) test $(TEST_INTEGRATION_PKGS) -tags=integration -count=1 -timeout 300s
_test-rls-plain:
	$(GO) test $(TEST_RLS_PKGS) -tags=rls -count=1 -timeout 300s

test-unit:
	$(GO) test $(TEST_UNIT_PKGS) -count=1 -timeout 120s -v

test-integration:
	$(GO) test $(TEST_INTEGRATION_PKGS) -tags=integration -count=1 -timeout 300s -v

test-rls:
	$(GO) test $(TEST_RLS_PKGS) -tags=rls -count=1 -timeout 300s -v

test-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -count=1 -timeout 300s -v

race:
	$(MAKE) -j4 _race-unit _race-integration _race-rls _race-e2e

_race-unit:
	$(GO) test $(TEST_UNIT_PKGS) -race -count=1 -timeout 300s
_race-integration:
	$(GO) test $(TEST_INTEGRATION_PKGS) -tags=integration -race -count=1 -timeout 300s
_race-rls:
	$(GO) test $(TEST_RLS_PKGS) -tags=rls -race -count=1 -timeout 300s
_race-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -race -count=1 -timeout 300s

_test-unit-cov: | .coverage
	$(GO) test $(TEST_UNIT_PKGS) -race -count=1 -timeout 300s -coverpkg=$(COVER_PKG_LIST) -coverprofile=.coverage/unit.out
_test-integration-cov: | .coverage
	$(GO) test $(TEST_INTEGRATION_PKGS) -tags=integration -race -count=1 -timeout 300s -coverpkg=$(COVER_PKG_LIST) -coverprofile=.coverage/integration.out
_test-rls-cov: | .coverage
	$(GO) test $(TEST_RLS_PKGS) -tags=rls -race -count=1 -timeout 300s -coverpkg=$(COVER_PKG_LIST) -coverprofile=.coverage/rls.out

# Merge the three per-suite profiles into a single coverage.out (max-count
# strategy — any suite covering a block wins). Mirrors iam-tender-acl.
# scripts/merge_coverage.py is expected alongside the other test tooling
# other agents are adding under scripts/ — this target is a no-op-safe
# fallback (go tool covdata-free concatenation) if that script isn't
# present yet.
_merge-coverage:
	@if [ -f scripts/merge_coverage.py ]; then \
		python3 scripts/merge_coverage.py \
		  .coverage/unit.out .coverage/integration.out .coverage/rls.out \
		  > coverage.out; \
	else \
		echo "mode: atomic" > coverage.out; \
		tail -n +2 -q .coverage/unit.out .coverage/integration.out .coverage/rls.out >> coverage.out 2>/dev/null || true; \
	fi
	@echo "==> coverage.out merged from all suites"

# This repo has no build-tag split: unit/integration/rls all select ./...
# (see TEST_*_PKGS). Running that suite three times in parallel with
# -race + Testcontainers blew the 300s per-suite timeout on GitHub-hosted
# runners after the coverage expansion. One pass writes coverage.out
# directly for the coverage-gate.sh step. 15m is the per-binary budget
# (postgres used to spawn ~50 containers; it now reuses one).
test-ci: | .coverage
	$(GO) test $(TEST_UNIT_PKGS) -race -count=1 -timeout 15m \
	  -coverpkg=$(COVER_PKG_LIST) -coverprofile=coverage.out
	@sed -i '' '/^$$/d' coverage.out

cover: test-ci
	$(GO) tool cover -html=coverage.out

cover-func: test-ci
	$(GO) tool cover -func=coverage.out

# -----------------------------
# DOCKER
# -----------------------------

docker-build:
	docker build -t $(IMAGE) .

docker-push: docker-build
	docker push $(IMAGE)

docker-up:
	@echo "Starting local PostgreSQL + Valkey + LocalStack (community)..."
	docker compose up -d postgres valkey localstack

docker-up-pro:
	@echo "Starting local PostgreSQL + Valkey + LocalStack Pro (Glue Schema Registry)..."
	@grep -q '^LOCALSTACK_AUTH_TOKEN=.\+' .env 2>/dev/null || { echo "ERROR: LOCALSTACK_AUTH_TOKEN not set in .env"; exit 1; }
	docker compose -f docker-compose.yml -f docker-compose.pro.yml up -d postgres valkey localstack

docker-down:
	docker compose down

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down -v

# -----------------------------
# MIGRATIONS
# -----------------------------

migrate-up:
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
		$(MIGRATE_IMAGE) -path=/migrations -database="$(DATABASE_MIGRATION_URL)" up

migrate-down:
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
		$(MIGRATE_IMAGE) -path=/migrations -database="$(DATABASE_MIGRATION_URL)" down 1

migrate-create:
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations \
		$(MIGRATE_IMAGE) create -ext sql -dir /migrations -seq $(NAME)

# -----------------------------
# DOCS
# -----------------------------
# OpenAPI and AsyncAPI specs have different sources of truth, mirroring
# iam-org-membership / iam-tender-acl:
#   - OpenAPI (docs/swagger/{docs.go,swagger.json,swagger.yaml}) is
#     GENERATED from swag `@Summary`/`@Tags`/`@Router` annotations on
#     handler functions via `make swag` — never hand-edited.
#   - AsyncAPI (api/asyncapi.yaml) IS hand-maintained (LLD §10.3) — embedded
#     via api/embed.go; keep it in sync with the actual event contract by
#     hand when it changes. Validated by platform-schemagov against a live
#     Glue registry (DLG-D20, supersedes DLG-D15's CI-governance scope) by
#     .github/workflows/schema-registry.yml — see the SCHEMA GOVERNANCE
#     targets below.
.PHONY: swag
swag:
	@echo "Generating Swagger docs..."
	$(GO) tool swag init \
	  -g swagger_info.go \
	  -d cmd/server,internal/adapter/inbound/http \
	  --output docs/swagger \
	  --parseDependency \
	  --parseInternal
	@echo "Swagger docs written to docs/swagger/"

.PHONY: swag-check
swag-check:
	bash .github/scripts/check-swagger-stale.sh

# -----------------------------
# SCHEMA GOVERNANCE
# -----------------------------
# Unlike iam-user-profile, api/asyncapi.yaml and internal/eventschema/*.json
# are BOTH hand-maintained here (no extract-schemas step) — asyncapi.yaml is
# not the generative source for the JSON files, so schema-validate does not
# regenerate them first.
SCHEMA_GOV_IMAGE ?= ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4

# schema-pull: pull the platform-schemagov Docker image.
.PHONY: schema-pull
schema-pull:
	docker pull "$(SCHEMA_GOV_IMAGE)"

# schema-validate: validate AsyncAPI spec + event schemas — 8 passes:
# (1) structure, (2) draft-07, (3) enum drift, (4) lifecycle annotations,
# (5) open-schema guard, (6) consumer strict-mode, (7) coverage, (8) AsyncAPI structure.
# No AWS credentials needed.
.PHONY: schema-validate
schema-validate:
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" validate \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/eventschema

# schema-diff: show compatibility diff between two schema files.
# Usage: make schema-diff CURRENT=<path-to-current.json> PROPOSED=<path-to-proposed.json>
#        Optionally: SCHEMA_NAME=<name> (defaults to the PROPOSED filename stem)
.PHONY: schema-diff
schema-diff:
	@test -n "$(CURRENT)" && test -n "$(PROPOSED)" || { \
	  echo "Usage: make schema-diff CURRENT=<current.json> PROPOSED=<proposed.json> [SCHEMA_NAME=<name>]"; \
	  exit 1; \
	}
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" diff \
	  --current     "$(CURRENT)" \
	  --proposed    "$(PROPOSED)" \
	  --schema-name "$(or $(SCHEMA_NAME),$(notdir $(basename $(PROPOSED))))"

# schema-prune: dry-run scan for orphaned Glue schemas (exist in Glue, not in repo).
# Pass EXECUTE=true to archive and delete: make schema-prune EXECUTE=true
# Requires GLUE_REGISTRY_NAME and AWS credentials.
.PHONY: schema-prune
schema-prune:
	@test -n "$(GLUE_REGISTRY_NAME)" || { \
	  echo "GLUE_REGISTRY_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  "$(SCHEMA_GOV_IMAGE)" prune \
	  --registry   "$(GLUE_REGISTRY_NAME)" \
	  $(if $(filter true,$(EXECUTE)),--execute,)

# schema-register: register event schemas to Glue (requires AWS credentials or LocalStack).
# Set AWS_ENDPOINT_URL=http://localhost:4566 in .env for LocalStack.
.PHONY: schema-register
schema-register:
	@test -n "$(GLUE_REGISTRY_NAME)" || { \
	  echo "GLUE_REGISTRY_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" register \
	  --registry   "$(GLUE_REGISTRY_NAME)" \
	  --schema-dir internal/eventschema

# schema-verify: fail if any of the three expected PascalCase schema names is
# missing from the Glue registry. Names match the domain.EventDelegation*
# constants (already PascalCase — no translation table needed, unlike
# iam-user-profile). Surfaces a mismatch pre-deploy rather than at first-event
# publish. Requires GLUE_REGISTRY_NAME and AWS credentials.
.PHONY: schema-verify
schema-verify:
	@test -n "$(GLUE_REGISTRY_NAME)" || { \
	  echo "GLUE_REGISTRY_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@missing=""; \
	for name in DelegationStarted DelegationEnded DelegationReviewRequested; do \
	  if ! aws glue get-schema \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_NAME),SchemaName=$$name" \
	      --region "$(AWS_REGION)" >/dev/null 2>&1; then \
	    missing="$$missing $$name"; \
	  fi; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo "FAIL: missing Glue schemas in registry '$(GLUE_REGISTRY_NAME)':$$missing"; \
	  echo "     run 'make schema-register' to create them"; \
	  exit 1; \
	fi; \
	echo "OK: all three schemas present in registry '$(GLUE_REGISTRY_NAME)'"

# -----------------------------
# CI
# -----------------------------

ci: tidy fmt-check vet lint arch-lint test-ci build

generate:
	$(GO) generate ./...

# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-user-profile
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private modules — resolved via SSH (git@github.com:) using the global URL rewrite.
# No tokens needed; requires an SSH key registered with github.com/BCBP-SOLUTIONS-FZC-LLC.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

export APP_NAME APP_ENV BUILD_VERSION

# Test package groups (explicit to handle per-group build tags cleanly)
TEST_UNIT_PKGS     := ./test/unit/...
TEST_POSTGRES_PKGS := ./test/postgres/...
TEST_INT_PKGS      := ./test/integration/...
TEST_E2E_PKGS      := ./test/e2e/...

# White-box (package-internal) tests that need no Docker.
# Included in unit runs and coverage but kept separate so they build cleanly.
TEST_INTERNAL_PKGS := ./internal/adapter/inbound/http/... \
                      ./internal/adapter/outbound/eventbus/... \
                      ./internal/adapter/outbound/postgres/... \
                      ./internal/adapter/outbound/s3/... \
                      ./internal/core/service/...

# Source packages measured for coverage (excludes test helpers and cmd).
# Uses tr+sed instead of paste -sd, because macOS BSD paste rejects combined flags.
COVER_PKG_LIST := $(shell $(GO) list ./internal/... ./pkg/... | tr '\n' ',' | sed 's/,$$//')

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup:
	@test -f .env || cp .env-example .env
	@mkdir -p .git/hooks
	@cp .githooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Environment ready (.env)"

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup           - copy .env-example to .env if missing; install .githooks/pre-commit"
	@echo "  make tidy            - go mod tidy"
	@echo "  make fmt             - go fmt ./..."
	@echo "  make vet             - go vet all packages"
	@echo "  make lint            - run golangci-lint"
	@echo "  make test            - unit + postgres + integration tests (requires Docker)"
	@echo "  make test-ci         - test with race detector + coverage (used in CI)"
	@echo "  make test-unit       - unit tests only (no Docker required)"
	@echo "  make test-postgres   - Postgres + RLS integration tests (requires Docker)"
	@echo "  make test-e2e        - end-to-end tests (requires Docker)"
	@echo "  make test-smoke      - smoke tests against a running APP_URL (via .github/scripts/smoke-tests.sh)"
	@echo "  make race            - all tests with -race flag"
	@echo "  make run             - run the server locally (go run)"
	@echo "  make build           - compile server binary to bin/"
	@echo "  make cover           - coverage profile + open HTML report"
	@echo "  make cover-func      - coverage summary by function"
	@echo "  make ci              - tidy + fmt-check + vet + lint + test-ci + build"
	@echo "  make docker-up       - start local infra with floci (S3/SNS/SQS/Glue, no token needed)"
	@echo "  make docker-down     - stop local containers"
	@echo "  make fmt-check       - verify gofmt formatting (no changes applied)"
	@echo "  make mod-verify      - go mod verify (check module download integrity)"
	@echo "  make vuln-check      - govulncheck on internal and pkg packages"
	@echo "  make extract-schemas  - derive internal/eventschema/*.json from api/asyncapi.yaml"
	@echo "  make swag             - generate Swagger docs from annotations (output: docs/swagger/)"
	@echo "  make swag-check       - fail if Swagger regeneration changes docs/swagger/"
	@echo "  make install-hooks    - install local git hooks (.githooks → .git/hooks)"
	@echo "  make godoc            - serve docs locally via pkgsite (http://localhost:8080)"
	@echo "  make clean            - remove build artefacts"
	@echo "  make pin-base-images  - fetch SHA digests and pin Dockerfile base images"
	@echo ""
	@echo "Schema governance (platform-schemagov 0.4):"
	@echo "  make schema-pull      - pull the schema-gov Docker image"
	@echo "  make schema-validate  - validate AsyncAPI + event schemas — 8 passes (no AWS required)"
	@echo "  make schema-diff      - diff two schema files: CURRENT=<path> PROPOSED=<path>"
	@echo "  make schema-register  - register event schemas to Glue (requires AWS/floci)"
	@echo "  make schema-verify    - pre-deploy check: fail if PascalCase schemas are missing (requires AWS)"
	@echo "  make schema-prune     - dry-run: list orphaned Glue schemas (requires AWS)"

# -----------------------------
# GO BASICS
# -----------------------------


.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

# -----------------------------
# LINT
# -----------------------------

.PHONY: lint
lint:
	@echo "Running linter..."
	$(GO) tool golangci-lint run

# -----------------------------
# TESTS
# -----------------------------

# ── Private per-suite targets (run in parallel by test-ci / test) ────────────
# Each writes its own coverage profile so profiles can be merged afterward.

.PHONY: _test-unit
_test-unit: | .coverage
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) \
	  -race -count=1 -timeout 120s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/unit.out

.PHONY: _test-postgres
_test-postgres: | .coverage
	$(GO) test $(TEST_POSTGRES_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/postgres.out

.PHONY: _test-integration
_test-integration: | .coverage
	$(GO) test $(TEST_INT_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/integration.out

# Merge the three per-suite profiles into a single coverage.out.
# Takes the MAX count per block so any suite covering a block is reflected.
.PHONY: _merge-coverage
_merge-coverage:
	@python3 scripts/merge_coverage.py \
	  .coverage/unit.out .coverage/postgres.out .coverage/integration.out \
	  > coverage.out
	@echo "==> coverage.out merged from all suites (max-count strategy)"

# ── test: parallel run (verbose, no coverage) ─────────────────────────────────
# Runs all three suites concurrently; wall-clock time ≈ slowest suite (~60 s).
.PHONY: test
test:
	$(MAKE) -j3 _test-unit-plain _test-postgres-plain _test-integration-plain

# Verbose, no-coverage variants — used by 'make test' above.
.PHONY: _test-unit-plain _test-postgres-plain _test-integration-plain
_test-unit-plain:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) \
	  -count=1 -timeout 120s -v
_test-postgres-plain:
	$(GO) test $(TEST_POSTGRES_PKGS) \
	  -tags=integration -count=1 -timeout 300s -v
_test-integration-plain:
	$(GO) test $(TEST_INT_PKGS) \
	  -tags=integration -count=1 -timeout 300s -v

# ── test-ci: parallel + race + coverage (used by CI / 'make ci') ─────────────
# All three suites run concurrently; profiles are merged into coverage.out.
# Wall-clock: ~60 s (was ~120 s sequential).
.PHONY: test-ci
test-ci: | .coverage
	$(MAKE) -j3 _test-unit _test-postgres _test-integration
	$(MAKE) _merge-coverage

# ── test-unit: unit + white-box internal tests (no Docker) ───────────────────
.PHONY: test-unit
test-unit:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) \
	  -count=1 -timeout 60s -v

.PHONY: test-postgres
test-postgres:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -v

.PHONY: test-e2e
test-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -count=1 -timeout 300s -v

# test-smoke: image size + startup gate checks against the CI-built image.
# Uses .github/scripts/smoke-tests.sh (same script the CI 'smoke' job runs). CI's
# 'smoke' job builds iam-user-profile-ci-test via docker/build-push-action before
# invoking the script; this target does the equivalent build locally first since
# the script itself only inspects/runs an already-loaded image — it never makes
# an HTTP request, so no running deployment or APP_URL is required.
.PHONY: test-smoke
test-smoke:
	@if [ -z "$$GO_PRIVATE_TOKEN" ]; then \
		echo "GO_PRIVATE_TOKEN must be set to a GitHub token with read access to BCBP-SOLUTIONS-FZC-LLC private repos (used by the Dockerfile's go_private_token build secret)."; \
		exit 1; \
	fi
	docker buildx build \
	  --load \
	  --tag iam-user-profile-ci-test \
	  --build-arg BUILD_VERSION=smoke-local \
	  --secret id=go_private_token,env=GO_PRIVATE_TOKEN \
	  .
	bash .github/scripts/smoke-tests.sh

# -----------------------------
# RUN
# -----------------------------

.PHONY: run
run:
	@-lsof -ti :$${APP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/server'

# -----------------------------
# BUILD
# -----------------------------

.PHONY: build
build:
	@echo "Building binary..."
	@mkdir -p bin
	$(GO) build -ldflags "-X main.version=$(BUILD_VERSION)" -o bin/$(APP_NAME) ./cmd/server
	@echo "Verifying library packages compile..."
	$(GO) build ./internal/... ./pkg/...

# -----------------------------
# DOCKER (LOCAL POSTGRES + VALKEY)
# -----------------------------

.PHONY: docker-up
docker-up:
	@echo "Starting local PostgreSQL + Valkey + floci (SNS/SQS/Glue) + floci-ui..."
	docker compose up -d postgres valkey floci floci-ui

.PHONY: docker-down
docker-down:
	@echo "Stopping local containers..."
	docker compose down

.PHONY: pin-base-images
pin-base-images:
	@echo "Fetching SHA digests for Dockerfile base images..."
	@GOLANG_DIGEST=$$(docker buildx imagetools inspect golang:1.26.6-alpine --format '{{.Manifest.Digest}}') && \
	 DISTROLESS_DIGEST=$$(docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}') && \
	 sed -i.bak \
	   -e "s|FROM golang:1.26.6-alpine|FROM golang:1.26.6-alpine@$$GOLANG_DIGEST|" \
	   -e "s|FROM gcr.io/distroless/static-debian12:nonroot|FROM gcr.io/distroless/static-debian12:nonroot@$$DISTROLESS_DIGEST|" \
	   Dockerfile && rm -f Dockerfile.bak && \
	 echo "golang:1.26.6-alpine $$GOLANG_DIGEST" > .docker-digests && \
	 echo "gcr.io/distroless/static-debian12:nonroot $$DISTROLESS_DIGEST" >> .docker-digests && \
	 echo "Digests written to .docker-digests — commit both Dockerfile and .docker-digests"

# -----------------------------
# CI
# -----------------------------

.PHONY: ci
ci: tidy fmt-check vet lint test-ci build

# -----------------------------
# COVERAGE
# -----------------------------

# cover / cover-func: parallel suites → merge → open report / print functions.
.PHONY: cover
cover: test-ci
	$(GO) tool cover -html=coverage.out

.PHONY: cover-func
cover-func: test-ci
	$(GO) tool cover -func=coverage.out

# -----------------------------
# DEBUG HELPERS
# -----------------------------

# race: quick parallel race-detector run without coverage overhead.
.PHONY: race
race:
	$(MAKE) -j3 _test-unit _test-postgres _test-integration

# -----------------------------
# SWAGGER
# -----------------------------

# extract-schemas: derive internal/eventschema/*.json from api/asyncapi.yaml.
# api/asyncapi.yaml is the single source of truth for event payload schemas.
# Re-run whenever asyncapi.yaml changes; commit the updated JSON files alongside.
# The JSON files are read by schema-gov validate --schema-dir internal/eventschema.
.PHONY: extract-schemas
extract-schemas:
	@echo "Extracting event schemas from api/asyncapi.yaml..."
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" extract \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/eventschema
	@echo "Done. Run 'git add internal/eventschema/' to stage the changes."

# swag: generate Swagger JSON/YAML from handler annotations into docs/swagger/.
# Re-run whenever annotations change. The output is checked in to the repo.
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
	@python3 scripts/patch-swagger-extensions.py

.PHONY: swag-check
swag-check:
	bash .github/scripts/check-swagger-stale.sh

.PHONY: install-hooks
install-hooks:
	@mkdir -p .git/hooks
	@cp .githooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed git hooks"

# -----------------------------
# GODOC
# -----------------------------

# godoc: serve package documentation locally using pkgsite.
# Opens http://localhost:8080 — browse to the module path in the UI.
.PHONY: godoc
godoc:
	@echo "Starting pkgsite at http://localhost:8080 — press Ctrl-C to stop"
	$(GO) run golang.org/x/pkgsite/cmd/pkgsite@latest -open .

# -----------------------------
# SCHEMA GOVERNANCE
# -----------------------------
SCHEMA_GOV_IMAGE     ?= ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4
# Force linux/amd64 for all schema-gov docker invocations — the image has no arm64 manifest.
# Docker Desktop's Rosetta 2 emulation on Apple Silicon runs amd64 containers transparently.
SCHEMA_GOV_PLATFORM ?= linux/amd64

# schema-pull: pull the platform-schemagov Docker image.
# Image is published linux/amd64 only (platform-schemagov CI); --platform forces
# emulation on Apple Silicon (requires Rosetta emulation enabled in Docker Desktop).
.PHONY: schema-pull
schema-pull:
	docker pull --platform "$(SCHEMA_GOV_PLATFORM)" "$(SCHEMA_GOV_IMAGE)"

# schema-validate: validate AsyncAPI spec + event schemas — 8 passes:
# (1) structure, (2) draft-07, (3) enum drift, (4) lifecycle annotations,
# (5) open-schema guard, (6) consumer strict-mode, (7) coverage, (8) AsyncAPI structure.
# No AWS credentials needed.
.PHONY: schema-validate
schema-validate: extract-schemas
	docker run --rm --platform linux/amd64 \
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
	docker run --rm --platform linux/amd64 \
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
	docker run --rm --platform linux/amd64 \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  "$(SCHEMA_GOV_IMAGE)" prune \
	  --registry   "$(GLUE_REGISTRY_NAME)" \
	  $(if $(filter true,$(EXECUTE)),--execute,)

# schema-register: register event schemas to Glue (requires AWS credentials or floci).
# Set AWS_ENDPOINT_URL=http://localhost:4566 in .env for floci.
.PHONY: schema-register
schema-register:
	@test -n "$(GLUE_REGISTRY_NAME)" || { \
	  echo "GLUE_REGISTRY_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	docker run --rm --platform linux/amd64 \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" register \
	  --registry   "$(GLUE_REGISTRY_NAME)" \
	  --schema-dir internal/eventschema

# schema-verify: fail if any of the four expected PascalCase schema names is
# missing from the Glue registry. Names match domain.GlueSchemaName + LLD
# §7.3.1 registry-layout table. Surfaces a mismatch pre-deploy rather than at
# first-event publish. Requires GLUE_REGISTRY_NAME and AWS credentials.
.PHONY: schema-verify
schema-verify:
	@test -n "$(GLUE_REGISTRY_NAME)" || { \
	  echo "GLUE_REGISTRY_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@missing=""; \
	for name in DelegationStarted DelegationEnded DelegationReviewRequested DelegationEscalationRequested; do \
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
	echo "OK: all four schemas present in registry '$(GLUE_REGISTRY_NAME)'"

# -----------------------------
# CHECKS (mirror what CI runs; safe to call locally before pushing)
# -----------------------------

# fmt-check: verify formatting without modifying files (mirrors CI gofmt step).
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l cmd/ internal/ pkg/ test/); \
	if [ -n "$$unformatted" ]; then \
		echo "FAIL: unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "gofmt: all files formatted"

# mod-verify: check that downloaded module zips match go.sum hashes.
.PHONY: mod-verify
mod-verify:
	$(GO) mod verify

# vuln-check: scan library packages for known vulnerabilities (excludes cmd/).
.PHONY: vuln-check
vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./internal/... ./pkg/...

# -----------------------------
# CLEAN
# -----------------------------

# Directory for per-suite coverage profiles (created on demand).
.coverage:
	@mkdir -p .coverage

.PHONY: clean
clean:
	rm -rf bin .coverage
	rm -f coverage.out coverage.html

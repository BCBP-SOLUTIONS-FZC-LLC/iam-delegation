# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-delegation
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private modules — resolved via SSH (git@github.com:) using the global URL rewrite.
# No tokens needed; requires an SSH key registered with github.com/BCBP-SOLUTIONS-FZC-LLC.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

export APP_NAME APP_ENV BUILD_VERSION

# Test package groups.
# This service has no separate test/ directory — all tests are colocated
# white-box (*_test.go next to the source, package-internal). TEST_UNIT_PKGS,
# TEST_POSTGRES_PKGS, and TEST_INT_PKGS are intentionally empty; CI suites
# that would normally run those directories are no-ops here.
TEST_UNIT_PKGS     :=
TEST_POSTGRES_PKGS :=
TEST_INT_PKGS      :=
TEST_E2E_PKGS      := ./cmd/server/...

# The Postgres adapter package — repository tests plus the §17.5 RLS matrix
# (rls_test.go), all against a Testcontainers Postgres. Used by test-postgres
# and test-rls (TEST_POSTGRES_PKGS stays empty so the colocated test-ci
# suites don't run it twice). Raise the -parallel cap on a bigger box, e.g.
# `make test-postgres TEST_POSTGRES_PARALLEL=8` (matches iam-org-membership).
POSTGRES_ADAPTER_PKGS  := ./internal/adapter/outbound/postgres/...
TEST_POSTGRES_PARALLEL ?= 4

# All colocated white-box tests — every package in this service that has
# *_test.go files. Run without Docker for unit suite; some need testcontainers
# (valkey, postgres) and are guarded by t.Skip when the container isn't up.
TEST_INTERNAL_PKGS := ./internal/adapter/inbound/consumer/... \
                      ./internal/adapter/inbound/http/... \
                      ./internal/adapter/outbound/catalogadmin/... \
                      ./internal/adapter/outbound/eventbus/... \
                      ./internal/adapter/outbound/httpx/... \
                      ./internal/adapter/outbound/metrics/... \
                      ./internal/adapter/outbound/orgmembership/... \
                      ./internal/adapter/outbound/postgres/... \
                      ./internal/adapter/outbound/tender/... \
                      ./internal/adapter/outbound/userprofile/... \
                      ./internal/adapter/outbound/valkey/... \
                      ./internal/core/domain/... \
                      ./internal/core/service/... \
                      ./pkg/... \
                      ./cmd/reconciler/... \
                      ./cmd/server/...

# Source packages measured for coverage (excludes test helpers and cmd).
# Uses tr+sed instead of paste -sd, because macOS BSD paste rejects combined flags.
COVER_PKG_LIST := $(shell $(GO) list ./internal/... ./pkg/... | tr '\n' ',' | sed 's/,$$//')

# Binaries, image and migrations. One image carries both binaries (single
# Dockerfile — see its header comment); the Deployment runs it unmodified,
# each CronJob overrides `command` to invoke /iam-delegation-reconciler.
SERVER_BINARY      := iam-delegation-server
SERVER_CMD_PKG     := ./cmd/server
RECONCILER_BINARY  := iam-delegation-reconciler
RECONCILER_CMD_PKG := ./cmd/reconciler
BUILD_DIR          := bin
GOFLAGS            ?=
LDFLAGS            := -s -w -X main.buildVersion=$(BUILD_VERSION)
IMAGE              ?= iam-delegation:latest

# The migration role MAY have BYPASSRLS; in docker-compose that's simply the
# postgres superuser. Production points this at a dedicated migration role
# (delegation_migrator), never at the runtime app role (delegation_app).
DATABASE_MIGRATION_URL ?= postgres://delegation:delegation@localhost:5432/delegation?sslmode=disable
MIGRATIONS_DIR         := internal/adapter/outbound/postgres/migrations
MIGRATE_IMAGE          := migrate/migrate:v4.17.1

# Every build tag a test file here may declare (today only cmd/server's
# e2e_test.go uses one, `e2e`). vet and lint run a second pass with all of
# them so tagged files are checked too — CI's quality gate calls `make vet`
# and `make lint`, so without it the e2e suite was never vetted or linted.
ALL_TEST_TAGS := integration,rls,e2e

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup:
	@test -f .env || cp .env-example .env
	@mkdir -p .git/hooks
	@test -f .githooks/pre-commit && cp .githooks/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit || true
	@echo "Environment ready (.env)"

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup           - copy .env-example to .env if missing; install .githooks/pre-commit"
	@echo "  make tidy            - go mod tidy"
	@echo "  make fmt             - gofmt -w every Go file"
	@echo "  make vet             - go vet (default build + every test build tag)"
	@echo "  make lint            - run golangci-lint (default build + every test build tag)"
	@echo "  make arch-lint       - run go-arch-lint against .go-arch-lint.yml (LLD §6/§6.2)"
	@echo "  make test            - unit + postgres + integration tests (requires Docker)"
	@echo "  make test-ci         - test with race detector + coverage (used in CI)"
	@echo "  make test-unit       - unit tests only (no Docker required)"
	@echo "  make test-postgres   - Postgres + RLS integration tests (requires Docker)"
	@echo "  make test-integration - full colocated suite incl. Postgres/Valkey/floci Testcontainers (requires Docker)"
	@echo "  make test-rls        - the §17.5 Row-Level-Security matrix only (requires Docker)"
	@echo "  make test-e2e        - end-to-end tests (requires Docker)"
	@echo "  make test-smoke      - build the image, then image-size + startup-gate checks for both binaries (smoke-tests.sh)"
	@echo "  make race            - all tests with -race flag"
	@echo "  make run             - run the server locally (go run)"
	@echo "  make run-server      - alias for 'make run'"
	@echo "  make run-reconciler  - run the reconciler locally; pass JOB=delegation-activation|delegation-expiry|delegation-review|delegation-cleanup"
	@echo "  make all             - alias for 'make build'"
	@echo "  make generate        - run any go:generate directives (currently none)"
	@echo "  make build           - compile both binaries (iam-delegation-server, iam-delegation-reconciler) to bin/"
	@echo "  make build-server    - compile only cmd/server"
	@echo "  make build-reconciler - compile only cmd/reconciler"
	@echo "  make cover           - coverage profile + open HTML report"
	@echo "  make cover-func      - coverage summary by function"
	@echo "  make ci              - tidy + fmt-check + vet + lint + arch-lint + test-ci + build"
	@echo "  make docker-up       - start local infra with floci (S3/SNS/SQS/Glue, no token needed)"
	@echo "  make docker-down     - stop local containers"
	@echo "  make compose-up      - start the full local dev stack (postgres, valkey, floci, floci-ui, server — self-migrates at startup)"
	@echo "  make compose-down    - stop and remove the full local dev stack, including volumes"
	@echo "  make docker-build    - build the container image (IMAGE to override, carries both binaries)"
	@echo "  make docker-push     - push the container image"
	@echo "  make migrate-up      - apply pending migrations against DATABASE_MIGRATION_URL (the server also self-migrates at startup)"
	@echo "  make migrate-down    - roll back one migration against DATABASE_MIGRATION_URL"
	@echo "  make migrate-create  - create a new migration pair (NAME=add_foo_table)"
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
	@echo "  make schema-sync-check - fail if internal/eventschema/*.json drifted from api/asyncapi.yaml"
	@echo "  make schema-verify    - pre-deploy check: fail unless each schema definition is registered + AVAILABLE (requires AWS)"
	@echo "  make schema-prune     - dry-run: list orphaned Glue schemas (requires AWS)"

# -----------------------------
# GO BASICS
# -----------------------------


.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	@gofmt -l -w .

.PHONY: vet
vet:
	$(GO) vet ./...
	$(GO) vet -tags=$(ALL_TEST_TAGS) ./...

# -----------------------------
# LINT
# -----------------------------

.PHONY: lint
lint:
	@echo "Running linter..."
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=$(ALL_TEST_TAGS) ./...

# arch-lint: enforce .go-arch-lint.yml component boundaries (LLD §6/§6.2) —
# the same script CI's validate-test.yml runs, matching iam-realm-provisioner.
.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

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
	@# No separate postgres test directory — all tests are colocated (see TEST_INTERNAL_PKGS).
	@# Create an empty profile so _merge-coverage has a valid input file.
	@echo "mode: atomic" > .coverage/postgres.out

.PHONY: _test-integration
_test-integration: | .coverage
	@# No separate integration test directory — all tests are colocated (see TEST_INTERNAL_PKGS).
	@# Create an empty profile so _merge-coverage has a valid input file.
	@echo "mode: atomic" > .coverage/integration.out

# Merge the three per-suite profiles into a single coverage.out.
# Takes the MAX count per block so any suite covering a block is reflected.
# merge_coverage.py handles empty/missing profiles gracefully (FileNotFoundError pass).
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
	@echo "No separate postgres test directory — skipping (tests are colocated)."
_test-integration-plain:
	@echo "No separate integration test directory — skipping (tests are colocated)."

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
	$(GO) test $(POSTGRES_ADAPTER_PKGS) -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v

# test-integration / test-rls: every test here is colocated white-box with no
# integration/rls build tags (see TEST_UNIT_PKGS above), so these select by
# package: the full colocated suite (Postgres/Valkey/floci via
# Testcontainers), and the Postgres adapter package that holds the §17.5 RLS
# matrix (rls_test.go) plus the repository tests. Both need Docker.
.PHONY: test-integration
test-integration:
	$(GO) test $(TEST_INTERNAL_PKGS) -count=1 -timeout 300s -v

.PHONY: test-rls
test-rls:
	$(GO) test $(POSTGRES_ADAPTER_PKGS) -run 'RLS' -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v

.PHONY: test-e2e
test-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -count=1 -timeout 300s -v

# test-smoke: image size + startup gate checks against the CI-built image.
# Uses .github/scripts/smoke-tests.sh (same script the CI 'smoke' job runs). CI's
# 'smoke' job builds iam-delegation-ci-test via docker/build-push-action before
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
	  --tag iam-delegation-ci-test \
	  --build-arg BUILD_VERSION=smoke-local \
	  --secret id=go_private_token,env=GO_PRIVATE_TOKEN \
	  .
	IMAGE_TAG=iam-delegation-ci-test BINARY=server bash .github/scripts/smoke-tests.sh
	IMAGE_TAG=iam-delegation-ci-test BINARY=reconciler ENTRYPOINT=/iam-delegation-reconciler \
	  bash .github/scripts/smoke-tests.sh

# -----------------------------
# RUN
# -----------------------------

.PHONY: run
run:
	@-lsof -ti :$${APP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/server'

# run-server: alias for `run`, kept for the paired run-server/run-reconciler names.
.PHONY: run-server
run-server: run

# run-reconciler: one CronJob pass locally. JOB=delegation-activation |
# delegation-expiry (default) | delegation-review | delegation-cleanup.
.PHONY: run-reconciler
run-reconciler:
	@if [ -f .env ]; then set -a && . ./.env && set +a && $(GO) run $(RECONCILER_CMD_PKG) --job=$${JOB:-delegation-expiry}; else $(GO) run $(RECONCILER_CMD_PKG) --job=$${JOB:-delegation-expiry}; fi

# -----------------------------
# BUILD
# -----------------------------

# build: both binaries (the image ships both — server + reconciler). The
# server's version is stamped via -X main.buildVersion; the reconciler reads
# BUILD_VERSION from its environment (the -X is a harmless no-op there).
.PHONY: all
all: build

# generate: run any go:generate directives (none today — kept so adding one
# needs no Makefile change).
.PHONY: generate
generate:
	$(GO) generate ./...

.PHONY: build
build: build-server build-reconciler
	@echo "Verifying library packages compile..."
	$(GO) build ./internal/... ./pkg/...

.PHONY: build-server
build-server:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(SERVER_BINARY) $(SERVER_CMD_PKG)

.PHONY: build-reconciler
build-reconciler:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(RECONCILER_BINARY) $(RECONCILER_CMD_PKG)

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

# compose-up / compose-down: the FULL stack, including the server container
# (docker-up starts only the infra, for `make run` against it).
.PHONY: compose-up
compose-up:
	docker compose up --build -d

.PHONY: compose-down
compose-down:
	docker compose down -v

.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE) .

.PHONY: docker-push
docker-push: docker-build
	docker push $(IMAGE)

# migrate-up: manual/CI use — the server binary also self-migrates at startup.
.PHONY: migrate-up
migrate-up:
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
		$(MIGRATE_IMAGE) -path=/migrations -database="$(DATABASE_MIGRATION_URL)" up

.PHONY: migrate-down
migrate-down:
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
		$(MIGRATE_IMAGE) -path=/migrations -database="$(DATABASE_MIGRATION_URL)" down 1

# migrate-create: NAME=add_foo_table. Note this service currently folds
# schema fixes into the single 000001_schema migration until first deploy
# (see CHANGELOG), so only use this once that policy ends.
.PHONY: migrate-create
migrate-create:
	@test -n "$(NAME)" || { echo "NAME is required, e.g. make migrate-create NAME=add_foo_table"; exit 1; }
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations \
		$(MIGRATE_IMAGE) create -ext sql -dir /migrations -seq $(NAME)

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
ci: tidy fmt-check vet lint arch-lint test-ci build

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
# api/asyncapi.yaml is the single source of truth for event payload schemas:
# each message's <Name>Envelope wraps a flat, $ref-free <Name>Payload, and
# extract writes exactly those payloads (4 produced + 3 consumed) — the files
# ValidatingCodec, ConsumedValidator and GlueCodec embed (DLG-D51). Re-run
# whenever asyncapi.yaml changes and commit the JSON alongside; never
# hand-edit it. CI's "Event schema sync check" fails on drift.
.PHONY: extract-schemas
extract-schemas:
	@echo "Extracting event schemas from api/asyncapi.yaml..."
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" extract \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/eventschema
	@echo "Done. Run 'git add internal/eventschema/' to stage the changes."

# schema-sync-check: fail if internal/eventschema/*.json has drifted from
# api/asyncapi.yaml (the same check CI runs). No AWS credentials needed.
.PHONY: schema-sync-check
schema-sync-check:
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" extract \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/eventschema \
	  --check

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
	@python3 scripts/patch-swagger-extensions.py
	@echo "Swagger docs written to docs/swagger/"

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
#
# Regenerates the JSON from api/asyncapi.yaml first (like iam-org-membership),
# so it validates what the committed files SHOULD be; `make schema-sync-check`
# (and CI) is what fails when the committed files have drifted. Before
# DLG-D51 asyncapi's payloads were envelope-allOf schemas, so extract emitted
# a $ref to EventEnvelope that could not be embedded and the JSON had to be
# hand-maintained; the payloads are now flat and $ref-free, so extract output
# is exactly the committed files.
.PHONY: schema-validate
schema-validate: extract-schemas
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" \
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
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" \
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
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  "$(SCHEMA_GOV_IMAGE)" prune \
	  --registry   "$(GLUE_REGISTRY_NAME)" \
	  $(if $(filter true,$(EXECUTE)),--execute,)

# schema-register: register event schemas to Glue (requires AWS credentials or floci).
# Registers a PascalCase-renamed, produced-only copy (stage-produced-event-schemas.sh)
# — schema-gov names each schema after its file stem, and GlueCodec resolves
# the PascalCase event-type names (DelegationStarted, ...). Consumed schemas
# are never registered here.
# Set AWS_ENDPOINT_URL=http://localhost:4566 in .env for floci.
.PHONY: schema-register
schema-register:
	@test -n "$(GLUE_REGISTRY_NAME)" || { \
	  echo "GLUE_REGISTRY_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@rm -rf .tmp/glue-schemas
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" register \
	  --registry   "$(GLUE_REGISTRY_NAME)" \
	  --schema-dir .tmp/glue-schemas

# schema-verify: fail unless each of the four PascalCase schemas has an
# AVAILABLE version whose definition matches this checkout's
# internal/eventschema file — the exact lookup GlueCodec does at startup
# (GetSchemaByDefinition, compact + ASCII-escaped like schema-gov register
# uploads it). Surfaces "pod would CrashLoop on NewGlueCodec" pre-deploy.
# Requires GLUE_REGISTRY_NAME, AWS credentials (or AWS_ENDPOINT_URL for
# floci) and python3.
.PHONY: schema-verify
schema-verify:
	@test -n "$(GLUE_REGISTRY_NAME)" || { \
	  echo "GLUE_REGISTRY_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@rm -rf .tmp/glue-schemas-verify
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas-verify >/dev/null
	@failed=""; \
	for file in .tmp/glue-schemas-verify/*.json; do \
	  name=$$(basename "$$file" .json); \
	  def=$$(python3 -c 'import json,sys; sys.stdout.write(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")))' "$$file"); \
	  status=$$(aws glue get-schema-by-definition \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_NAME),SchemaName=$$name" \
	      --schema-definition "$$def" \
	      --region "$(AWS_REGION)" --query Status --output text 2>/dev/null); \
	  if [ "$$status" != "AVAILABLE" ]; then \
	    failed="$$failed $$name($${status:-not-registered})"; \
	  fi; \
	done; \
	rm -rf .tmp/glue-schemas-verify; \
	if [ -n "$$failed" ]; then \
	  echo "FAIL: this checkout's schema definition is not registered+AVAILABLE in '$(GLUE_REGISTRY_NAME)':$$failed"; \
	  echo "     run 'make schema-register' (or wait for schema-registry.yml) to register it"; \
	  exit 1; \
	fi; \
	echo "OK: all four schema definitions registered and AVAILABLE in '$(GLUE_REGISTRY_NAME)'"

# -----------------------------
# CHECKS (mirror what CI runs; safe to call locally before pushing)
# -----------------------------

# fmt-check: verify formatting without modifying files (mirrors CI gofmt step).
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l . 2>/dev/null); \
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

# POST monorepo entry point (docs/65, docs/66).
#
# Cross-language commands live here. No Turborepo: pnpm controls only the
# web/ui/contracts workspace, the Go root module and the uv-managed Python
# adapter stay out of it (T0002 decision).
#
# `make check` is the single documented command covering the basic Go, Web
# and Python checks (T0002 acceptance criterion).

SHELL := /bin/bash

.PHONY: help bootstrap check build test dev sync-schemas check-schema-drift \
	infra-up infra-init infra infra-down infra-ps infra-logs

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN { FS = ":.*## " } { printf "  %-20s %s\n", $$1, $$2 }'

bootstrap: ## install every toolchain's pinned dependencies
	pnpm install --frozen-lockfile
	go mod download
	cd services/scientific-adapter && uv sync --frozen
	$(MAKE) sync-schemas

check: ## one-command basic check: Go + Web + Python (+ schema drift)
	$(MAKE) check-schema-drift
	go vet ./...
	go build ./...
	go test ./...
	pnpm --filter @post/ui typecheck
	pnpm --filter @post/web typecheck
	pnpm --filter @post/web lint
	node --disable-warning=MODULE_TYPELESS_PACKAGE_JSON --test "apps/web/lib/config.test.mjs"
	cd services/scientific-adapter && uv run pytest -q

build: ## production builds and smoke across the three languages
	go build ./cmd/...
	pnpm --filter @post/web build
	cd services/scientific-adapter && uv run python -c "import post_scientific_adapter; print('scientific-adapter', post_scientific_adapter.__version__)"

test: ## full test suites (Go unit + web config + Python adapter)
	go test ./...
	node --disable-warning=MODULE_TYPELESS_PACKAGE_JSON --test "apps/web/lib/config.test.mjs"
	cd services/scientific-adapter && uv run pytest

sync-schemas: ## copy canonical JSON Schemas from specs/schemas/ to packages/schemas/
	bash packages/schemas/scripts/sync-schemas.sh

check-schema-drift: ## fail when packages/schemas/ diverges from specs/schemas/
	bash packages/schemas/scripts/check-schema-drift.sh

dev: ## run the Go API and the web dev server together (Ctrl-C stops both)
	@trap 'kill 0' INT TERM; go run ./cmd/api & pnpm --filter @post/web dev

# Local infrastructure (Docker Compose, T0003). Applications stay host-native
# (docs/66 §2); only the infra dependencies below run in containers.
# Details, ports, dev-only credentials: infra/docker/README.md.
infra-up: ## start the infra stack (docker compose up -d --wait; waits for healthy)
	docker compose up -d --wait --wait-timeout 300
	@docker compose ps

infra-init: ## idempotent one-shot init: MinIO bucket + Gitea admin/org/service account
	bash infra/docker/init.sh

infra: infra-up infra-init ## infra-up + infra-init

infra-down: ## stop the infra stack (named volumes are kept; data survives)
	docker compose down

infra-ps: ## show infra stack status
	docker compose ps

infra-logs: ## follow infra stack logs (Ctrl-C to exit)
	docker compose logs -f

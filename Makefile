# POST monorepo entry point (docs/65, docs/66).
#
# Cross-language commands live here. No Turborepo: pnpm controls only the
# web/ui/contracts workspace, the Go root module and the uv-managed Python
# adapter stay out of it (T0002 decision).
#
# `make check` is the single documented command covering the basic Go, Web
# and Python checks (T0002 acceptance criterion).

SHELL := /bin/bash

.PHONY: help bootstrap check build test dev smoke sync-schemas check-schema-drift \
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

dev: ## start all five applications host-native from .env.dev (Ctrl-C stops them all)
	@test -f .env.dev || { \
		echo "error: .env.dev not found — copy .env.example and fill in dev values:"; \
		echo "  cp .env.example .env.dev"; \
		exit 1; }
	@set -a; . ./.env.dev || { \
		echo "error: .env.dev failed to source (see the bash error above — a value with shell metacharacters, e.g. POST_GITEA_TOKEN=<token>). Fix the file and retry; nothing starts on a guessed environment."; \
		exit 1; }; set +a; \
	trap 'kill 0' INT TERM EXIT; \
	echo ">> api=$${POST_API_ADDR} mcp=$${POST_MCP_ADDR} worker(redis=$${POST_REDIS_ADDR}) web(3000) scientific-adapter=$${POST_SCIENTIFIC_ADAPTER_PORT}"; \
	go run ./cmd/api & \
	go run ./cmd/worker & \
	go run ./cmd/mcp-server & \
	pnpm --filter @post/web dev & \
	(cd services/scientific-adapter && uv run scientific-adapter) & \
	wait

# Docker-free CI smoke (T0006): builds the binaries and proves, with real
# HTTP requests against real processes, that
#   1. every app starts on validated configuration and fails closed without it;
#   2. /healthz is liveness-only and /readyz reports the dependency truth
#      (PostgreSQL is pointed at a closed port on purpose: readiness must
#      answer 503 not_ready, never a false "ok" and never a crash);
#   3. the worker really consumes a job from Redis and completes it — the
#      Redis server is cmd/devredis (in-process Redis protocol, dev tool),
#      the client is the canonical redis-cli 7.x (docs/64).
# Ports are the smoke's private range; the web build needs the production
# layer (Next sets NODE_ENV=production, the loader requires POST_ENV=prod).
SMOKE_BIN := bin/smoke
SMOKE_ENV := POST_ENV=test \
	POST_API_ADDR=127.0.0.1:18080 POST_MCP_ADDR=127.0.0.1:19080 \
	POST_REDIS_ADDR=127.0.0.1:16379 \
	POST_DB_HOST=127.0.0.1 POST_DB_PORT=15432 \
	POST_DB_PASSWORD=smoke-test-pw POST_DB_SSLMODE=disable \
	POST_BLOB_ACCESS_KEY=smoke-ak POST_BLOB_SECRET_KEY=smoke-sk \
	POST_GITEA_TOKEN=smoke-tok
SMOKE_WEB_ENV := POST_ENV=prod \
	API_BASE_URL=http://127.0.0.1:18080 SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100

smoke: ## Docker-free CI smoke: start/check every app with real requests (no infra stack needed)
	@rm -rf $(SMOKE_BIN) && mkdir -p $(SMOKE_BIN)
	@go build -o $(SMOKE_BIN)/ ./cmd/api ./cmd/worker ./cmd/mcp-server ./cmd/devredis
	@echo ">> binaries built; building web (production layer, NODE_ENV=production requires POST_ENV=prod)"
	@env $(SMOKE_WEB_ENV) pnpm --filter @post/web build >/tmp/post-smoke-web-build.log 2>&1
	@env $(SMOKE_ENV) bash -c 'set -euo pipefail; \
		bin/smoke/devredis -addr 127.0.0.1:16379 >/tmp/post-smoke-devredis.log 2>&1 & REDIS_PID=$$!; \
		bin/smoke/api >/tmp/post-smoke-api.log 2>&1 & API_PID=$$!; \
		bin/smoke/mcp-server >/tmp/post-smoke-mcp.log 2>&1 & MCP_PID=$$!; \
		(cd services/scientific-adapter && POST_SCIENTIFIC_ADAPTER_PORT=19100 \
			uv run scientific-adapter) >/tmp/post-smoke-adapter.log 2>&1 & ADAPTER_PID=$$!; \
		bin/smoke/worker >/tmp/post-smoke-worker.log 2>&1 & WORKER_PID=$$!; \
		setsid bash -c "POST_ENV=prod API_BASE_URL=http://127.0.0.1:18080 \
			SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100 \
			pnpm --filter @post/web exec next start -p 13000" >/tmp/post-smoke-web.log 2>&1 & WEB_PID=$$!; \
		trap "kill -- -$$WEB_PID 2>/dev/null; kill $$REDIS_PID $$API_PID $$MCP_PID $$ADAPTER_PID $$WORKER_PID 2>/dev/null || true" EXIT; \
		for i in $$(seq 1 60); do curl -fsS http://127.0.0.1:18080/healthz >/dev/null 2>&1 && break; sleep 0.5; done; \
		echo ">> api /healthz:"; curl -fsS http://127.0.0.1:18080/healthz; echo; \
		echo ">> api /readyz (postgres down => 503 not_ready):"; \
		code=$$(curl -sS -o /tmp/post-smoke-readyz.json -w "%{http_code}" http://127.0.0.1:18080/readyz); \
		cat /tmp/post-smoke-readyz.json; echo; \
		test "$$code" = "503"; grep -q "not_ready" /tmp/post-smoke-readyz.json; \
		grep -q "\"postgresql\":{\"status\":\"down\"" /tmp/post-smoke-readyz.json; \
		echo ">> mcp /healthz + /readyz:"; \
		curl -fsS http://127.0.0.1:19080/healthz; echo; \
		curl -fsS http://127.0.0.1:19080/readyz; echo; \
		echo ">> adapter /healthz + /readyz (port 19100):"; \
		for i in $$(seq 1 60); do curl -fsS http://127.0.0.1:19100/healthz >/dev/null 2>&1 && break; sleep 0.5; done; \
		curl -fsS http://127.0.0.1:19100/healthz; echo; \
		curl -fsS http://127.0.0.1:19100/readyz; echo; \
		echo ">> worker: enqueue a real job and wait for completion (idempotency seen key)"; \
		redis-cli -p 16379 RPUSH post:queue:jobs "{\"id\":\"smoke-1\",\"type\":\"smoke\",\"correlation_id\":\"smoke-run\",\"payload\":{\"ref\":\"smoke\"}}" >/dev/null; \
		seen=0; for i in $$(seq 1 40); do \
			[ "$$(redis-cli -p 16379 EXISTS post:worker:seen:smoke-1)" = "1" ] && { seen=1; break; }; sleep 0.5; \
		done; \
		test "$$seen" = "1" || { echo "worker did not complete the job"; cat /tmp/post-smoke-worker.log; exit 1; }; \
		test "$$(redis-cli -p 16379 LLEN post:queue:processing)" = "0"; \
		echo ">> worker completed job smoke-1 (queue drained)"; \
		grep -q "worker: job completed" /tmp/post-smoke-worker.log; \
		echo ">> web status page (live data from the real API above):"; \
		for i in $$(seq 1 120); do curl -fsS http://127.0.0.1:13000/ >/dev/null 2>&1 && break; sleep 0.5; done; \
		curl -fsS http://127.0.0.1:13000/ | grep -oE "(Development status|Go API|Scientific adapter|readiness=[a-z_() ,:]+|down)" | head -8; \
		echo ">> fail-closed: api without POST_ENV must exit non-zero and name the variable:"; \
		set +e; env -u POST_ENV bin/smoke/api 2>/tmp/post-smoke-noconfig.txt; rc=$$?; set -e; \
		test "$$rc" -ne 0; grep -q "POST_ENV" /tmp/post-smoke-noconfig.txt; \
		echo "exit=$$rc"; head -4 /tmp/post-smoke-noconfig.txt; \
		echo ">> fail-closed: next start without POST_ENV must fail naming it:"; \
		set +e; timeout 60 env -u POST_ENV pnpm --filter @post/web exec next start -p 13001 \
			>/tmp/post-smoke-web-noconfig.log 2>&1; rc=$$?; set -e; \
		test "$$rc" -ne 0; grep -q "POST_ENV" /tmp/post-smoke-web-noconfig.log; \
		grep -q "Failed to prepare server" /tmp/post-smoke-web-noconfig.log; \
		echo "exit=$$rc (next 16 logs the ConfigError and never serves, but does not always exit by itself; timeout guarantees termination)"; \
		grep -m1 "POST_ENV:" /tmp/post-smoke-web-noconfig.log; \
		echo ">> smoke OK";'

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

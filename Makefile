# POST monorepo entry point (docs/65, docs/66).
#
# Cross-language commands live here. No Turborepo: pnpm controls only the
# web/ui/contracts workspace, the Go root module and the uv-managed Python
# adapter stay out of it (T0002 decision).
#
# `make check` is the single documented command covering the basic Go, Web
# and Python checks (T0002 acceptance criterion). Gate split (T0008):
# `make check` / `make test` are infrastructure-free — no Docker, no
# database, they must pass on a bare host. Everything that needs a real
# PostgreSQL lives in `make test-integration`, which fails loudly with a
# printed reason when no database is reachable (never a silent skip).

SHELL := /bin/bash

# Unit-test packages only: tests/integration connects to real PostgreSQL
# (the DSN default lives in the test-integration target below) and belongs to
# test-integration, not check/test.
GO_UNIT_PKGS := $(shell go list ./... | grep -v '/tests/integration')
STATICCHECK_VER := 2026.2.1

.PHONY: help bootstrap check build rddev test test-integration dev smoke sync-schemas \
	check-schema-drift check-schema-snapshot check-openapi check-spec-version fmt-check staticcheck lint-python type-python \
	progress ci migrate search-rebuild search-embed infra-up infra-init infra infra-down infra-ps infra-logs

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN { FS = ":.*## " } { printf "  %-20s %s\n", $$1, $$2 }'

bootstrap: ## install every toolchain's pinned dependencies
	pnpm install --frozen-lockfile
	go mod download
	cd services/scientific-adapter && uv sync --frozen
	$(MAKE) sync-schemas

check: ## one-command basic check: Go + Web + Python (+ schema/OpenAPI drift); no Docker, no database
	$(MAKE) check-schema-drift
	$(MAKE) check-schema-snapshot
	$(MAKE) check-openapi
	$(MAKE) check-spec-version
	@# The three Go steps below mirror CI's `go` job (.github/workflows/ci.yml)
	@# on purpose. They were missing, and "make check is green" therefore did
	@# NOT mean "CI's Go job is green": on 2026-09-18 two tasks lost a round to
	@# exactly that gap — T0711 shipped an unformatted file (fmt-check) and
	@# T1110 a staticcheck finding, both with a green `make check`. A local
	@# command that is a similar-looking subset of the gate is worse than a
	@# smaller command that says what it covers.
	$(MAKE) fmt-check
	go vet ./...
	$(MAKE) staticcheck
	bash scripts/tests/staticcheck-unit-test.sh
	go build ./...
	go test $(GO_UNIT_PKGS)
	pnpm --filter @post/ui typecheck
	pnpm --filter @post/web typecheck
	pnpm --filter @post/web lint
	bash scripts/web-unit-tests.sh
	cd services/scientific-adapter && uv run pytest -q

rddev: ## rebuild bin/rddev — the binary the four-gate loop runs (see #135)
	@# bin/ is gitignored and nothing else rebuilds it, so this and the guard in
	@# cmd/rddev/staleness.go are the two halves of one rule: the tool that grades
	@# the work must be the tool the source describes.
	go build -o bin/rddev ./cmd/rddev

build: ## production builds and smoke across the three languages
	go build ./cmd/...
	pnpm --filter @post/web build
	cd services/scientific-adapter && uv run python -c "import post_scientific_adapter; print('scientific-adapter', post_scientific_adapter.__version__)"

test: ## unit test suites (Go + web + Python adapter); no Docker, no database
	go test $(GO_UNIT_PKGS)
	bash scripts/web-unit-tests.sh
	cd services/scientific-adapter && uv run pytest

migrate: ## apply the embedded migrations to the dev database (idempotent)
# The missing half of the documented local flow: `make infra-up` starts
# PostgreSQL and `make dev` starts the applications, but nothing ever created
# the schema, so the apps ran against an empty database. URL comes from
# POSTGRES_TEST_ADMIN_URL or the same default as test-integration.
	go run ./cmd/rddev db migrate

# POST_WORKER_DB is the configuration every `go run ./cmd/worker` command-line
# target below runs with: the connection named the way `make migrate` names it
# (POST_DB_*), with the dev defaults the rest of this file uses and
# POSTGRES_TEST_ADMIN_URL taking precedence when it is set, so the documented
# flow (`make infra-up && make migrate && make search-rebuild`) works on a
# fresh clone without exporting anything. It lives in one place because the
# two search commands take the same configuration, and the same twenty lines
# of shell in two targets is how one of them silently drifts from the other.
# `#` must be written `\#` here. In a recipe line make does not treat `#` as a
# comment, so the same text was harmless while these twenty lines lived inside
# each target; in a VARIABLE VALUE it is a comment, and make stripped everything
# from the first `${...#...}` (line 2 of the value) to the end of the logical
# line — the `if ... fi`, all ten POST_* assignments and the trailing `go run
# ./cmd/worker` — leaving both search targets expanding to a shell fragment that
# ends mid-word. The integration tests did not catch it because they invoke the
# binary directly; `make -n search-embed` shows it in one line.
POST_WORKER_DB = PG_DB_PASSWORD="$${POST_DB_PASSWORD:-postgres_dev_pw}"; \
	if [ -n "$${POSTGRES_TEST_ADMIN_URL:-}" ]; then \
		raw="$${POSTGRES_TEST_ADMIN_URL\#postgres://}"; raw="$${raw\#postgresql://}"; \
		creds="$${raw%%@*}"; hostpart="$${raw\#*@}"; \
		PG_DB_USER="$${creds%%:*}"; PG_DB_PASSWORD="$${creds\#*:}"; \
		hostport="$${hostpart%%/*}"; PG_DB_NAME="$${hostpart\#*/}"; PG_DB_NAME="$${PG_DB_NAME%%\?*}"; \
		PG_DB_HOST="$${hostport%%:*}"; PG_DB_PORT="$${hostport\#\#*:}"; \
	fi; \
	POST_ENV="$${POST_ENV:-dev}" \
	POST_DB_HOST="$${PG_DB_HOST:-$${POST_DB_HOST:-127.0.0.1}}" \
	POST_DB_PORT="$${PG_DB_PORT:-$${POST_DB_PORT:-5432}}" \
	POST_DB_USER="$${PG_DB_USER:-$${PG_DB_USER:-postgres}}" \
	POST_DB_PASSWORD="$$PG_DB_PASSWORD" \
	POST_DB_NAME="$${PG_DB_NAME:-$${POST_DB_NAME:-post}}" \
	POST_DB_SSLMODE="$${POST_DB_SSLMODE:-disable}" \
	POST_BLOB_ACCESS_KEY="$${POST_BLOB_ACCESS_KEY:-dev-ak}" \
	POST_BLOB_SECRET_KEY="$${POST_BLOB_SECRET_KEY:-dev-sk}" \
	POST_GITEA_TOKEN="$${POST_GITEA_TOKEN:-dev-token}" \
	go run ./cmd/worker

search-rebuild: ## rebuild the search projection from its sources (idempotent; needs PostgreSQL only)
# The projection is derived state, so it is rebuildable: this re-derives every
# search_documents row from the entities it indexes (T0901,
# internal/search/rebuild.go). It is idempotent — truncate and re-derive in one
# transaction — and it is the only operation that can repair a row whose
# visibility inputs changed without an event of its own, because it reads the
# sources rather than replaying the event log. The same rebuild is the
# search.rebuild job type the worker registers; this target is the operator's
# direct entry point, and it needs no Redis.
#
# The database connection is named the way `make migrate` and `make smoke` name
# it (POST_DB_*), with the dev defaults the rest of this file uses, so the
# documented flow (`make infra-up && make migrate && make search-rebuild`) works
# on a fresh clone without exporting anything.
	@$(POST_WORKER_DB) -search-rebuild

search-embed: ## recompute every search document embedding that is stale (idempotent; needs PostgreSQL only)
# The embedding column is derived state too, so it is recomputable: this
# recomputes the vector (and the model provenance 00092 records beside it) of
# every search_documents row whose stored vector is not the current model's
# (T0902, internal/search/embedding). It is idempotent in the strong sense —
# a second run selects nothing because every row already carries the current
# model's provenance, so it writes nothing and reports embedded=0 — and it is
# what an operator runs after the embedder changes, since a vector from a
# replaced model must be recomputed, not kept. The same recompute is the
# search.embed job type the worker registers; this target is the operator's
# direct entry point, and it needs no Redis.
	@$(POST_WORKER_DB) -search-embed

test-integration: ## integration suite against real PostgreSQL; loud failure (with reason) when unreachable
# The default port is 5432, the port `make infra-up` actually publishes: it is
# what docker-compose.yml defaults to, what infra/docker/README.md and
# ops/DEV_COMMANDS.md document, and what CI's service container uses. This line
# used to say 15432 — the port the compose file documents as the override for a
# *second* stack — so the documented flow (`make infra-up && make
# test-integration`) failed with "no PostgreSQL reachable" on a fresh clone, and
# a Worker, which cannot start Docker itself, had no way to reach a database.
# Override with POSTGRES_TEST_ADMIN_URL for a non-default stack.
#
# -timeout 20m, not Go's 10m default: measured over 26 consecutive main runs on
# 2026-09-18/19, this suite's CI job takes 280-330s normally, but three runs on
# *three different GitHub-hosted runners* exceeded 600s and were killed — at
# 699s, 710s and 711s. The tree was byte-identical between a 320s run and a
# 710s one (only tasks/decisions.md differed, verified with git diff), and in
# both inspected kills the panic dump showed a test that had started 1s/4s
# earlier, so tests were still completing at the deadline: that is runner
# contention, not a hang and not a race. The 10m default turned that into an
# intermittent false red on main (~12% of runs), which blocks merges for no
# reason. Nothing is masked by this: every test must still pass, and a genuine
# hang is still caught, ten minutes later. If you are reading this because you
# want to raise it again, get the same shape of evidence first — a test that
# is *stuck*, not a suite that is *slow*.
	@PG_TEST_URL="$${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"; \
	if python3 scripts/pg-ready.py "$$PG_TEST_URL"; then \
		echo ">> test-integration: running integration suite against $$PG_TEST_URL"; \
		POSTGRES_TEST_ADMIN_URL="$$PG_TEST_URL" go test ./tests/integration -count=1 -timeout 20m; \
	else \
		echo ">> test-integration: FAILED — integration tests require a real PostgreSQL and none is reachable (see pg-ready above)." >&2; \
		exit 1; \
	fi

sync-schemas: ## copy canonical JSON Schemas from specs/schemas/ to packages/schemas/
	bash packages/schemas/scripts/sync-schemas.sh

check-schema-drift: ## fail when packages/schemas/ diverges from specs/schemas/
	bash packages/schemas/scripts/check-schema-drift.sh

check-schema-snapshot: ## fail when specs/database/postgres.sql is not the ordered migrations
	python3 scripts/gen_schema_snapshot.py --check

check-openapi: ## validate the OpenAPI contract: parse + internal $ref integrity + structure
	python3 scripts/validate_openapi.py

check-spec-version: ## fail when specs/SPEC_VERSION.json is not the digest of its inputs
# The same family as the two above it: a derived artifact that drifts from its
# inputs and fails CI, not the local loop. It was missing here, and main went red
# on 6ec746c for it — a one-line edit to tasks/tasks.json (an INPUT of this
# digest) committed without regenerating the marker. `make check` is the
# one-command local check, so the check belongs in it.
	python3 scripts/spec_version.py --check

fmt-check: ## fail when any Go file is not gofmt-formatted (legacy baseline: ops/ci/gofmt-baseline.txt)
# .rddev/ is runtime state (Worker worktrees under .rddev/worktrees/<TASK>), not source.
# gofmt has no module awareness, so without this exclusion an in-flight Worker's copy of a
# grandfathered file is reported as a NEW violation and the Supervisor's own gate fails.
	@out="$$(gofmt -l . | grep -vE '^\.rddev/' | grep -vxF -f <(grep -v '^#' ops/ci/gofmt-baseline.txt) || true)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt: these files are not formatted:" >&2; \
		echo "$$out" >&2; \
		echo "run: gofmt -w <file> (new files must be clean; grandfathered list: ops/ci/gofmt-baseline.txt)" >&2; \
		exit 1; \
	else \
		echo "gofmt: clean"; \
	fi

staticcheck: ## honnef.co static analysis, pinned version (grandfathered baseline: ops/ci/staticcheck-baseline.txt)
	bash scripts/staticcheck.sh

lint-python: ## ruff lint over the scientific adapter (config: ops/ci/ruff.toml)
	cd services/scientific-adapter && uvx ruff check . --config ../../ops/ci/ruff.toml

type-python: ## mypy type-check over the scientific adapter source (config: ops/ci/mypy.ini)
	cd services/scientific-adapter && uvx mypy --config-file ../../ops/ci/mypy.ini src/post_scientific_adapter

progress: ## regenerate the auto section of tasks/progress.md from task_status.json
	python3 scripts/update_progress.py

ci: ## run every CI stage locally in order (scripts/ci.sh); integration stage needs a database
	bash scripts/ci.sh

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

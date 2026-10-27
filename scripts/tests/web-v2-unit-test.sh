#!/usr/bin/env bash
# WEB-01 harness self-tests (required test: "missing service/account entry
# point fails the run instead of skipping").
#
# A checker that has never been shown to fail is indistinguishable from a
# checker that does nothing. This script proves, in the negative direction,
# that the WEB-01 harness's guards actually fire:
#
#   1. the static anti-bypass scan rejects a journey module that uses
#      page.request, and accepts a clean one;
#   2. the step accounting rejects a declared step that did not run;
#   3. the entry-point check rejects a login page with no account-
#      establishment entry, and accepts a complete one;
#   4. an unreachable login service fails the entry-point check;
#   5. run.sh --preflight-only fails on unreachable PostgreSQL, unreachable
#      Redis, and a missing web dependency — never prints a skip and exits 0.
#
# Every case asserts BOTH the non-zero exit and the specific reason, because
# "it failed" is not the property required: "it failed for the reason this
# guard exists" is.
#
# Usage: bash scripts/tests/web-v2-unit-test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
V2_DIR="$ROOT/tests/web-v2"
SCAN="$ROOT/scripts/tests/web-v2-bypass-scan.mjs"
FIXTURES="$ROOT/scripts/tests/fixtures"
FIXTURE_PORT="${WEB_V2_FIXTURE_PORT:-18080}"
FIXTURE_URL="http://127.0.0.1:$FIXTURE_PORT"
WORK="$(mktemp -d)"
FIXTURE_PID=""

cleanup() {
  if [ -n "$FIXTURE_PID" ]; then
    kill "$FIXTURE_PID" 2>/dev/null || true
    wait "$FIXTURE_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

pass=0
failures=0

ok() {
  pass=$((pass + 1))
  echo "PASS  $1"
}

bad() {
  failures=$((failures + 1))
  echo "FAIL  $1" >&2
}

# expect_failure <description> <expected-reason-regex> <command...>
# Runs the command, requires a non-zero exit and the reason in its output.
expect_failure() {
  local description="$1" reason="$2"
  shift 2
  local output status
  set +e
  output="$("$@" 2>&1)"
  status=$?
  set -e
  if [ "$status" -eq 0 ]; then
    bad "$description — expected failure, command exited 0"
    echo "$output" | tail -20 >&2
    return
  fi
  if ! grep -qE "$reason" <<<"$output"; then
    bad "$description — failed, but not for the expected reason ($reason)"
    echo "$output" | tail -20 >&2
    return
  fi
  ok "$description (exit $status; reason matched)"
}

# expect_success <description> <command...>
expect_success() {
  local description="$1"
  shift
  local output status
  set +e
  output="$("$@" 2>&1)"
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    bad "$description — expected success, command exited $status"
    echo "$output" | tail -20 >&2
    return
  fi
  ok "$description"
}

command -v node >/dev/null 2>&1 || { echo "web-v2 unit test: FAILED — node is not on PATH" >&2; exit 1; }
node --version >/dev/null

# An ephemeral port that is closed right now, so the "unreachable service"
# case cannot accidentally hit a service someone else started on a
# hardcoded port. Bound on 127.0.0.1, then verified to refuse connections.
free_port() {
  local attempt port
  for attempt in $(seq 1 20); do
    port="$(node -e 'const net=require("net");const s=net.createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
    if node -e 'const net=require("net");const s=net.createConnection({host:"127.0.0.1",port:Number(process.argv[1])});s.on("connect",()=>{s.destroy();process.exit(1)});s.on("error",()=>process.exit(0))' "$port"; then
      echo "$port"
      return 0
    fi
  done
  return 1
}

# ---- 1. Static anti-bypass scan: clean passes, every bypass spelling fails --
mkdir -p "$WORK/clean" "$WORK/bypass" "$WORK/context-request" "$WORK/locator-evaluate" \
  "$WORK/locator-evaluate-all" "$WORK/page-eval" "$WORK/empty"
cat > "$WORK/clean/journey.mjs" <<'EOF'
// prose may name the forbidden APIs: page.request, context.request,
// page.evaluate and page.$eval are all rejected in expressions, not comments.
export async function drive(page) {
  await page.goto("http://127.0.0.1/");
  const button = page.getByRole("button", { name: "Sign in" });
  await button.click();
  await page.locator(".global-nav-user-handle").waitFor({ state: "visible" });
}
EOF
cat > "$WORK/bypass/journey.mjs" <<'EOF'
export async function drive(page) {
  await page.request.get("http://127.0.0.1/api/v1/auth/session");
  await page.goto("http://127.0.0.1/");
}
EOF
cat > "$WORK/context-request/journey.mjs" <<'EOF'
export async function drive(page, context) {
  await context.request.post("http://127.0.0.1/api/v1/auth/login", { data: { email: "a@b.test" } });
  await page.goto("http://127.0.0.1/");
}
EOF
cat > "$WORK/locator-evaluate/journey.mjs" <<'EOF'
export async function drive(page) {
  const form = page.locator("form");
  await form.evaluate((node) => node.submit());
}
EOF
cat > "$WORK/locator-evaluate-all/journey.mjs" <<'EOF'
export async function drive(page) {
  await page.locator("form").evaluateAll((nodes) => nodes.length);
}
EOF
cat > "$WORK/page-eval/journey.mjs" <<'EOF'
export async function drive(page) {
  await page.$eval("form", (form) => form.submit());
  await page.$$eval("form", (forms) => forms.length);
}
EOF
expect_success "static scan accepts a DOM-only journey (forbidden names in comments)" node "$SCAN" "$WORK/clean"
expect_failure "static scan rejects page.request" "BYPASS .*page\\.request" node "$SCAN" "$WORK/bypass"
expect_failure "static scan rejects context.request" "BYPASS .*context\\.request" node "$SCAN" "$WORK/context-request"
expect_failure "static scan rejects locator.evaluate" "BYPASS .*evaluate/evaluateAll" node "$SCAN" "$WORK/locator-evaluate"
expect_failure "static scan rejects locator.evaluateAll" "BYPASS .*evaluate/evaluateAll" node "$SCAN" "$WORK/locator-evaluate-all"
expect_failure "static scan rejects page.\$eval / page.\$\$eval" "BYPASS .*page\\.\\\$eval" node "$SCAN" "$WORK/page-eval"
expect_failure "static scan rejects an empty directory" "empty scan is not a clean scan" node "$SCAN" "$WORK/empty"
expect_success "static scan accepts the real harness" node "$SCAN" "$V2_DIR"

# ---- 2. Helper unit tests: step accounting + evidence-failure guards --------
expect_success "helper unit tests (step accounting, evidence-failure guard)" \
  node --test "$V2_DIR/lib/steps.test.mjs" "$V2_DIR/lib/evidence.test.mjs"

# ---- 3+4. Entry-point check: complete passes, missing entry / service fail --
if [ ! -d "$V2_DIR/node_modules/playwright" ]; then
  echo "web-v2 unit test: installing harness deps for the entry-point check"
  ( cd "$V2_DIR" && npm ci --no-audit --no-fund )
fi
if ! ( cd "$V2_DIR" && node -e 'require("playwright")' ) >/dev/null 2>&1; then
  ( cd "$V2_DIR" && PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell )
fi
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")" || { echo "web-v2 unit test: FAILED — browser library bootstrap failed" >&2; exit 1; }
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

node "$ROOT/scripts/tests/web-v2-fixture-server.mjs" "$FIXTURES" "$FIXTURE_PORT" > "$WORK/fixture.log" 2>&1 &
FIXTURE_PID=$!
fixture_ready=0
for _ in $(seq 1 40); do
  if curl -sf -o /dev/null "$FIXTURE_URL/web-v2-login-complete.html"; then
    fixture_ready=1
    break
  fi
  sleep 0.2
done
if [ "$fixture_ready" != 1 ]; then
  echo "web-v2 unit test: FAILED — the fixture server did not become ready" >&2
  cat "$WORK/fixture.log" >&2
  exit 1
fi

expect_success "entry-point check accepts a complete login page" \
  node "$V2_DIR/entrypoint-check.mjs" "$FIXTURE_URL/web-v2-login-complete.html"
expect_failure "entry-point check rejects a missing account-establishment entry" \
  "account-establishment entry.*missing|is missing:.*auth-sub-after" \
  node "$V2_DIR/entrypoint-check.mjs" "$FIXTURE_URL/web-v2-login-no-entry.html"
CLOSED_PORT="$(free_port)" || { echo "web-v2 unit test: FAILED — could not find a closed port for the unreachable-service case" >&2; exit 1; }
echo "INFO  unreachable-service case uses verified-closed port 127.0.0.1:$CLOSED_PORT"
expect_failure "entry-point check fails on an unreachable service" \
  "service unreachable" \
  node "$V2_DIR/entrypoint-check.mjs" "http://127.0.0.1:$CLOSED_PORT/login"

# ---- 5. Missing service / missing dependency fails the runner --------------
# The valid stack for this section is the repository's documented dev stack.
export POSTGRES_TEST_ADMIN_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
export WEB_V2_WEB_ORIGIN="http://127.0.0.1:31119"

# The Go harness carries no built-in DSN (gosec G101, and a silent fallback
# could point a run at the wrong database). With neither variable set it
# must fail with the named configuration error, not start.
(
  cd "$ROOT"
  go build -o "$WORK/web-v2-harness" ./tests/web-v2/harness
) || { echo "web-v2 unit test: FAILED — the harness did not compile" >&2; exit 1; }
expect_failure "harness refuses to start without a database DSN" \
  "configuration error: .*POSTGRES" \
  env -u WEB_V2_POSTGRES_ADMIN_URL -u POSTGRES_TEST_ADMIN_URL \
  WEB_V2_WEB_ORIGIN="$WEB_V2_WEB_ORIGIN" \
  "$WORK/web-v2-harness" --preflight

expect_failure "run.sh fails when PostgreSQL is unreachable" \
  "postgres unreachable" \
  env POSTGRES_TEST_ADMIN_URL="postgres://postgres:postgres_dev_pw@127.0.0.1:59999/post" \
  bash "$V2_DIR/run.sh" --preflight-only

expect_failure "run.sh fails when Redis is unreachable" \
  "redis unreachable" \
  env POSTGRES_TEST_ADMIN_URL="$POSTGRES_TEST_ADMIN_URL" WEB_V2_REDIS_ADDR="127.0.0.1:59998" \
  bash "$V2_DIR/run.sh" --preflight-only

mkdir -p "$WORK/no-web-deps"
expect_failure "run.sh fails when a web dependency is missing" \
  "missing dependency: .*node_modules" \
  env POSTGRES_TEST_ADMIN_URL="$POSTGRES_TEST_ADMIN_URL" WEB_V2_WEB_DIR="$WORK/no-web-deps" \
  bash "$V2_DIR/run.sh" --preflight-only

echo
if [ "$failures" -ne 0 ]; then
  echo "web-v2 unit test: FAILED — $failures failure(s), $pass pass(es)" >&2
  exit 1
fi
echo "web-v2 unit test: PASSED — $pass checks (the negative guards fire for the reasons they exist)"

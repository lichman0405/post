#!/usr/bin/env bash
#
# Install every tool the Master Security/Quality Gate shells out to, at exactly
# the versions ops/security/tool-versions.sh pins.
#
#   make security-tools          # one command; safe to re-run at any time
#   bash tests/security/install-security-tools.sh
#
# WHY THIS IS ONE COMMAND AND NOT A PARAGRAPH OF INSTRUCTIONS
# -----------------------------------------------------------
# The new gate rows (sast-go/python/node, sbom-*, license-audit) each assert
# the version of the tool they ran and REFUSE to report a verdict from a
# different one. That is the right behaviour for a security gate and a
# miserable thing to discover one row at a time, so the pins live in one file,
# the assertions read that file, and this script installs exactly it. The
# Supervisor's CI job runs this as the step before the gate.
#
# It is deliberately NOT wired into `make bootstrap`: bootstrap sets up what
# the product needs to build and test, and this installs scanners. A developer
# who only wants to run the unit tests should not have to download gosec.
#
# WHAT "FAILS LOUDLY" MEANS HERE
# ------------------------------
# No step is allowed to fail quietly: every install is followed by a version
# assertion against the pin, so a PATH shadowed by an older scanner is a
# failing install rather than a green gate running yesterday's tool. Exit code
# 1 with the command that failed, 2 for usage.
#
# One tool is ASSERTED WITHOUT BEING INSTALLED — uv. It is a prerequisite of
# this script rather than one of its products (it installs bandit, and it
# syncs the adapter's environment three steps below), so the assertion is
# about what is already on PATH: the wrong uv stops the install here instead
# of letting the Python SBOM and the vuln-python audit run under a generator
# nobody chose. `pip install uv==<pin>` or `uv self update <pin>` is the fix
# the failure message names.
#
# WHAT IT ALSO INSTALLS (and why those are not optional)
# -----------------------------------------------------
#   * pnpm's workspace dependencies (`pnpm install --frozen-lockfile`) — the
#     Node SBOM reads the INSTALLED workspace, and without node_modules pnpm
#     emits components with no licence data at all.
#   * the adapter's virtualenv (`uv sync --frozen`) — the Python SBOM's
#     licences come from the installed distributions' METADATA, which a bare
#     `uv export` does not carry.
# Both are lockfile-driven and re-runnable, and both write only ignored paths.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

VERSIONS="$ROOT/ops/security/tool-versions.sh"
# shellcheck source=/dev/null
. "$VERSIONS" || { echo "security-tools: cannot read $VERSIONS" >&2; exit 2; }

[ $# -eq 0 ] || { echo "security-tools: takes no arguments (got: $*)" >&2; exit 2; }

step() { printf '\n== %s\n' "$*"; }
die() { echo "security-tools: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- Go scanners -----------------------------------------------------------
# `go install` drops the binary in GOBIN (or GOPATH/bin). Both the install and
# the assertion below use the same directory, and the assertion reads the
# MODULE version out of the binary, because that is what the pin names and
# because gosec's own -version prints "dev" for anything built this way.
#
# The install path and the module path are deliberately separate arguments:
# `go install .../gosec/v2@v2.29.0` is not a main package (it fails with
# "package github.com/securego/gosec/v2 is not a main package"), while
# `go version -m` reports the module as `github.com/securego/gosec/v2` with no
# /cmd/ in it. One string cannot be both.
GOBIN_DIR="${GOBIN:-$(go env GOPATH)/bin}"
export PATH="$GOBIN_DIR:$PATH"

install_go_tool() { # install_go_tool <binary> <install-path> <module> <version>
  local bin="$1" pkg="$2" mod="$3" ver="$4"
  step "$bin $ver (go install $pkg@$ver)"
  GOBIN="$GOBIN_DIR" go install "$pkg@$ver" || die "go install $pkg@$ver failed"
  local path have
  path="$(command -v "$bin")" || die "$bin is not on PATH after installing into $GOBIN_DIR — add that directory to PATH"
  have="$(go version -m "$path" 2>/dev/null | awk -v m="$mod" '$1=="mod" && $2==m {print $3}')"
  [ -n "$have" ] || die "cannot read the module version out of $path (expected the module $mod — a binary built from a vendor directory or a fork reports its own module here)"
  [ "$have" = "$ver" ] || die "$path is $have, the pin is $ver — the install did not take (a differently-built $bin earlier on PATH?)"
  echo "   $path -> $have (module $mod)"
}

have go || die "go is not on PATH (https://go.dev/dl)"
install_go_tool gosec github.com/securego/gosec/v2/cmd/gosec github.com/securego/gosec/v2 "$GOSEC_VERSION"
install_go_tool cyclonedx-gomod github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod github.com/CycloneDX/cyclonedx-gomod "$CYCLONEDX_GOMOD_VERSION"
# The vuln-go row's scanner. Installed here and asserted below exactly like
# the other two: before this, the only place its version was written down was
# the CI job, so a local run of the gate used whatever `@latest` had resolved
# to on that machine.
install_go_tool govulncheck golang.org/x/vuln/cmd/govulncheck golang.org/x/vuln "$GOVULNCHECK_VERSION"

# --- the Python toolchain (uv) ---------------------------------------------
# uv is assert-only: it is a PREREQUISITE of this script (it is the installer
# below, and the adapter's environment comes from `uv sync`), so there is
# nothing here to install it from — pip-installing a Python tool from a shell
# script that assumes it may already exist is how two uv installs end up on
# one PATH. What this script does instead is refuse to go on with the wrong
# one, because three verdicts come out of this binary: the document shape the
# licence audit judges (`uv export`), the Python dependency audit (`uv audit`)
# and the bandit install below.
step "uv $UV_VERSION (asserted against ops/security/tool-versions.sh)"
have uv || die "uv is not on PATH — install the pinned one first (pip install uv==$UV_VERSION, or https://docs.astral.sh/uv)"
UV_PATH="$(command -v uv)"
HAVE="$(uv --version 2>/dev/null | awk '{print $2}')"
[ "$HAVE" = "$UV_VERSION" ] || die "uv at $UV_PATH is ${HAVE:-unknown}, the pin is $UV_VERSION \
(ops/security/tool-versions.sh). uv decides the shape of the document the licence audit judges and is the \
instrument of the vuln-python row — a scan and an inventory from an unknown generator are not evidence. \
Install the pinned one: pip install uv==$UV_VERSION, or 'uv self update $UV_VERSION'."
echo "   $UV_PATH -> $HAVE"

# --- Python scanner --------------------------------------------------------
step "bandit $BANDIT_VERSION (uv tool install)"
uv tool install --force "bandit==$BANDIT_VERSION" || die "uv tool install bandit==$BANDIT_VERSION failed"
BANDIT="$(command -v bandit)" || die "bandit is not on PATH after 'uv tool install' — uv's tool bin directory (~/.local/bin by default) has to be on PATH"
HAVE="$(bandit --version 2>&1 | awk '$1=="bandit" {print $2; exit}')"
[ "$HAVE" = "$BANDIT_VERSION" ] || die "bandit at $BANDIT is ${HAVE:-unknown}, the pin is $BANDIT_VERSION"
echo "   $BANDIT -> $HAVE"

# --- Node SAST toolchain ---------------------------------------------------
# An isolated npm project (tests/security/node-tools), NOT a workspace member:
# adding eslint-plugin-security to apps/web would put a security rule set into
# the app's own lint config, where a next lint run could disable or weaken it.
step "the Node SAST toolchain (npm ci in tests/security/node-tools)"
have npm || die "npm is not on PATH (it ships with Node)"
( cd tests/security/node-tools && npm ci --no-audit --no-fund ) || die "npm ci failed in tests/security/node-tools"
TOOLS="$ROOT/tests/security/node-tools"
for pair in "eslint:$ESLINT_VERSION" "eslint-plugin-security:$ESLINT_PLUGIN_SECURITY_VERSION" "typescript-eslint:$TYPESCRIPT_ESLINT_VERSION"; do
  pkg="${pair%%:*}"; want="${pair#*:}"
  got="$(node -p "require('$TOOLS/node_modules/$pkg/package.json').version" 2>/dev/null)"
  [ "$got" = "$want" ] || die "$pkg at $TOOLS is ${got:-unknown}, the pin is $want"
  echo "   $pkg -> $got"
done

# --- The two lockfile-driven environments the SBOM rows read ---------------
step "the pnpm workspace (pnpm install --frozen-lockfile; the Node SBOM reads the installed tree)"
have pnpm || die "pnpm is not on PATH (https://pnpm.io)"
HAVE="$(pnpm --version)"
PIN="$(node -p "require('./package.json').packageManager.replace(/^pnpm@/, '')")"
[ "$HAVE" = "$PIN" ] || die "pnpm on PATH is $HAVE and package.json pins pnpm@$PIN"
pnpm install --frozen-lockfile || die "pnpm install --frozen-lockfile failed"
echo "   pnpm $HAVE"

step "the scientific adapter's environment (uv sync --frozen; the Python SBOM reads its dist-info METADATA)"
( cd services/scientific-adapter && uv sync --frozen ) || die "uv sync --frozen failed in services/scientific-adapter"
echo "   $(cd services/scientific-adapter && uv --version)"

# --- What the gate will now run with --------------------------------------
cat <<EOF

security-tools: every pinned tool is installed. The gate asserts these exact
versions, so a different one is a red row, not a warning:

  gosec            $(go version -m "$(command -v gosec)" 2>/dev/null | awk '$1=="mod" && $2=="github.com/securego/gosec/v2" {print $3}')  (read out of the binary; gosec's own -version prints "dev" for a go-installed one)
  cyclonedx-gomod  $CYCLONEDX_GOMOD_VERSION
  govulncheck      $GOVULNCHECK_VERSION
  bandit           $BANDIT_VERSION
  eslint           $ESLINT_VERSION (+ eslint-plugin-security $ESLINT_PLUGIN_SECURITY_VERSION, typescript-eslint $TYPESCRIPT_ESLINT_VERSION)
  pnpm             $PIN (package.json)
  uv               $UV_VERSION (asserted above: this script does not install it)

Next: bash tests/security/master-security-gate.sh
EOF

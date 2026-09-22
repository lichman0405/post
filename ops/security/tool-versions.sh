# The pinned versions of every tool the Master Security/Quality Gate
# (tests/security/master-security-gate.sh) shells out to and does not get from
# the repository's own lockfiles.
#
# WHY THIS FILE EXISTS
# --------------------
# A security row whose tool floats is a row whose verdict floats: `gosec@latest`
# is a different scanner next month, and "the gate was green yesterday" stops
# meaning anything about today. Every row that consumes a pin below asserts the
# version it actually ran, and `make security-tools` installs exactly these
# versions and refuses to install anything else. Three files have to agree —
# this one, the row's evidence line, and the developer's PATH — so the pin
# lives in exactly one place and everything reads it from here.
#
# TOOLS THAT ARE PINNED ELSEWHERE, DELIBERATELY
# ---------------------------------------------
#   * pnpm  — the SBOM generator for the npm face (`pnpm sbom`). Pinned by
#             package.json's `packageManager: pnpm@12.4.1`, which is the
#             lockfile-adjacent pin the whole workspace already runs under;
#             duplicating it here would create a second source of truth.
#   * uv    — the SBOM generator for the Python face (`uv export --format
#             cyclonedx1.5`). It is already a prerequisite of the
#             vuln-python row, and the gate records the exact generator
#             version it ran in the SBOM's own metadata.tools, which the
#             sbom-python row prints as evidence.
#   * sqlc  — pinned in sqlc.yaml (v1.31.1), not a scanner.
#
# Sourced by tests/security/install-security-tools.sh and by
# tests/security/sast.sh. Keep the values shell-quoted and one per line: the
# install script and the row assertions parse this file.

# Go SAST (tests/security/sast.sh go). gosec's own `-version` prints "dev"
# for a `go install`ed binary, so the row reads the module version out of the
# binary with `go version -m` instead — see version_of_go_tool in
# tests/security/sast.sh.
GOSEC_VERSION=v2.29.0

# Python SAST (tests/security/sast.sh python). Installed with `uv tool install`
# into ~/.local/bin.
BANDIT_VERSION=1.8.6

# Node SAST (tests/security/sast.sh node). ESLint's version is the one
# apps/web itself declares in its devDependencies; the plugin and the
# TypeScript parser are the additions docs/40 asks to be recorded — see
# ops/security/new-dependencies.json for license, purpose and alternatives.
ESLINT_VERSION=9.39.5
ESLINT_PLUGIN_SECURITY_VERSION=4.0.1
TYPESCRIPT_ESLINT_VERSION=8.70.1
TYPESCRIPT_VERSION=5.9.3

# Go SBOM generator (tests/security/sbom.sh go): CycloneDX's own module
# generator, which reads go.mod and emits license evidence per module.
CYCLONEDX_GOMOD_VERSION=v1.9.0

# There is deliberately NO pin for @cyclonedx/cyclonedx-npm. It is the obvious
# candidate for the npm face and it was tried first: it cannot read
# pnpm-lock.yaml, so against this workspace it either fails or renders an
# inventory of a tree the repository does not install. pnpm's own `sbom`
# command reads the lockfile it already owns, which is why the npm face uses it
# and why pnpm's pin (package.json's packageManager) is the one that matters
# there. A variable here that nothing consumes would be a pin somebody
# eventually installs and trusts.

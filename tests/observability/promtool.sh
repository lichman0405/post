#!/usr/bin/env bash
# Fetches the pinned promtool and prints its path on stdout.
#
# WHY THIS IS NOT A REPOSITORY DEPENDENCY
#
# promtool is Prometheus's own rule checker. It is the only honest way to say
# "this rule file is valid" and "this rule fires on these samples" — an
# assertion written by hand in a shell script would be a re-implementation of
# PromQL by someone who is not the PromQL maintainers, and it would agree with
# the rules file for the same reason its author wrote both.
#
# It is deliberately NOT a Go module dependency: the rule files are consumed
# by a Prometheus server, which is not a Go module of this repository, and
# adding a 200-package module graph to go.mod to validate three YAML files
# would put a build dependency on every `go build` in the repository. Same
# reasoning as tests/web-smoke/run.sh installing Chromium: the harness owns
# the tool, the product does not depend on it.
#
# The cache lives outside the repository so that the 120 MB tarball is not a
# thing anybody can accidentally commit. It is machine-local and rebuildable:
# deleting it costs one download.
#
#   Version pinned, archive verified by SHA-256 from the release's own
#   sha256sums.txt. A tool that decides whether alerts work is not something
#   to fetch and run unverified.
#
# Usage: PROMTOOL="$(bash tests/observability/promtool.sh)"
set -euo pipefail

PROMTOOL_VERSION="3.14.0"
PROMTOOL_SHA256="f665c6da19eb7ba399c915d30c7d9793c9b417bf8a749b504bc470678631478d"
CACHE_DIR="${POST_PROMTOOL_DIR:-/tmp/post-promtool}"
ARCHIVE="prometheus-${PROMTOOL_VERSION}.linux-amd64.tar.gz"
URL="https://github.com/prometheus/prometheus/releases/download/v${PROMTOOL_VERSION}/${ARCHIVE}"
BIN="${CACHE_DIR}/promtool-${PROMTOOL_VERSION}"

die() { echo "promtool: $*" >&2; exit 1; }

# Already fetched? The binary must both exist and RUN — a truncated download
# left by an interrupted run would otherwise be used silently on every later
# run, and every rule assertion would then "pass" because promtool never
# answered at all.
if [ -x "$BIN" ] && "$BIN" --version >/dev/null 2>&1; then
  echo "$BIN"
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) ;;
  *) die "promtool ${PROMTOOL_VERSION} is pinned for linux-amd64; this machine is $(uname -m). Add the matching release asset (and its checksum) rather than substituting a different tool." ;;
esac
[ "$(uname -s)" = "Linux" ] || die "promtool fetch is written for Linux; this machine is $(uname -s)"

command -v curl >/dev/null 2>&1 || die "curl is required to fetch promtool"
echo "promtool: fetching ${PROMTOOL_VERSION} into ${CACHE_DIR} (one-time, ~120 MB)" >&2
mkdir -p "$CACHE_DIR"

TMP="$(mktemp -d "${CACHE_DIR}/download.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

curl -sSL --fail --retry 3 -m 600 -o "${TMP}/${ARCHIVE}" "$URL" \
  || die "download failed: $URL"

echo "${PROMTOOL_SHA256}  ${TMP}/${ARCHIVE}" > "${TMP}/expected.sha256"
( cd "$TMP" && sha256sum -c expected.sha256 >/dev/null 2>&1 ) \
  || die "SHA-256 mismatch on ${ARCHIVE}: refusing to run an unverified binary. Expected ${PROMTOOL_SHA256}."

tar -xzf "${TMP}/${ARCHIVE}" -C "$TMP" --strip-components=1 \
  "prometheus-${PROMTOOL_VERSION}.linux-amd64/promtool" \
  || die "could not extract promtool from ${ARCHIVE}"
[ -f "${TMP}/promtool" ] || die "archive did not contain promtool"

# Install atomically: a run killed mid-copy must not leave a half-written
# binary at the path the next run's existence check trusts.
install -m 0755 "${TMP}/promtool" "${BIN}.new"
mv -f "${BIN}.new" "$BIN"
trap - EXIT; rm -rf "$TMP"

"$BIN" --version >/dev/null 2>&1 || die "fetched promtool does not run"
echo "promtool: installed $("$BIN" --version 2>&1 | head -1)" >&2
echo "$BIN"

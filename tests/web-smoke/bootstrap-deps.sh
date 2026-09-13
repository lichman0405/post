#!/usr/bin/env bash
# Browser-dependency bootstrap for the web smoke harness.
#
# Playwright's Chromium needs a handful of system libraries (libatk,
# libatk-bridge, libatspi, libXcomposite, libXdamage, libasound, libXi).
# On a runner without root (a Worker, a stripped CI image) `sudo
# playwright install-deps` is not available, so this script downloads the
# Ubuntu 24.04 packages from the configured apt repo and extracts them
# into ~/.local/share/post-web-smoke-libs — machine-local, outside the
# repository. On a host that already has the libraries (CI ubuntu-latest,
# a developer desktop) this detects that and does nothing.
#
# Prints the LD_LIBRARY_PATH to export (empty when nothing is needed).
set -euo pipefail

ROOT_DIR="$HOME/.local/share/post-web-smoke-libs"
LIBS_DIR="$ROOT_DIR/usr/lib/x86_64-linux-gnu"
# Missing library name -> Ubuntu 24.04 (noble) package that provides it.
declare -A PKG=(
  ["libatk-1.0.so.0"]="libatk1.0-0t64"
  ["libatk-bridge-2.0.so.0"]="libatk-bridge2.0-0t64"
  ["libatspi.so.0"]="libatspi2.0-0t64"
  ["libXcomposite.so.1"]="libxcomposite1"
  ["libXdamage.so.1"]="libxdamage1"
  ["libasound.so.2"]="libasound2t64"
  ["libXi.so.6"]="libxi6"
  ["libcups.so.2"]="libcups2t64"
  ["libavahi-common.so.3"]="libavahi-common3"
  ["libavahi-client.so.3"]="libavahi-client3"
)

# Self-discover every Playwright browser binary in the cache (full chromium
# and/or the headless shell — whichever `playwright install` put there) and
# treat their missing-library union as the closure to satisfy.
CACHE="${PLAYWRIGHT_BROWSERS_PATH:-$HOME/.cache/ms-playwright}"
BINARIES=""
for b in "$CACHE"/chromium-*/chrome-linux/chrome \
         "$CACHE"/chromium_headless_shell-*/chrome-linux/headless_shell; do
  if [ -x "$b" ]; then
    BINARIES="$BINARIES $b"
  fi
done
if [ -z "$BINARIES" ]; then
  echo "bootstrap-deps: no Playwright browser binaries under $CACHE" >&2
  exit 1
fi

missing() { # missing [with-libs] — unresolvable libraries across all binaries.
  # Without the flag: the host's own loader view (system libs only). With
  # the flag: including the locally extracted libs.
  local ldpath=""
  if [ "${1:-}" = "with-libs" ]; then
    ldpath="$LIBS_DIR${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  else
    ldpath="${LD_LIBRARY_PATH:-}"
  fi
  for b in $BINARIES; do
    LD_LIBRARY_PATH="$ldpath" ldd "$b" 2>/dev/null
  done | awk '/not found/ {print $1}' | sort -u
}

# The host already carries everything the browsers link against: nothing
# to extract, nothing to export.
if [ -z "$(missing)" ]; then
  echo ""
  exit 0
fi

mkdir -p "$LIBS_DIR"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Iterate until the whole closure resolves: each round downloads the
# packages that provide the still-missing libraries, whose own
# dependencies may surface the next round (e.g. libcups needs avahi).
for round in 1 2 3 4 5 6; do
  unresolved="$(missing with-libs || true)"
  if [ -z "$unresolved" ]; then
    echo "bootstrap-deps: closure resolved in $((round - 1)) round(s)" >&2
    echo "$LIBS_DIR"
    exit 0
  fi
  for lib in $unresolved; do
    pkg="${PKG[$lib]:-}"
    if [ -z "$pkg" ]; then
      echo "bootstrap-deps: no package mapping for missing library $lib" >&2
      exit 1
    fi
    echo "bootstrap-deps: fetching $pkg (provides $lib)" >&2
    (cd "$WORK" && apt download "$pkg" >/dev/null)
    dpkg -x "$WORK"/"$pkg"_*.deb "$ROOT_DIR"
  done
done

echo "bootstrap-deps: libraries still unresolved after 6 rounds: $(missing with-libs)" 1>&2
exit 1

#!/usr/bin/env bash
set -euo pipefail

# Reference bootstrap for Ubuntu 24.04 LTS. Review before executing on a real host.
if [[ "$(uname -s)" != "Linux" ]]; then
  echo "ERROR: Linux required" >&2; exit 1
fi
if ! grep -q 'Ubuntu 24.04' /etc/os-release 2>/dev/null; then
  echo "WARNING: canonical environment is Ubuntu 24.04 LTS" >&2
fi

sudo apt-get update
sudo apt-get install -y \
  build-essential git git-lfs curl wget ca-certificates gnupg jq unzip zip make pkg-config \
  openssh-client rsync ripgrep fd-find tree lsof tmux htop shellcheck \
  bubblewrap socat postgresql-client redis-tools

git lfs install

# Claude Code sandbox prerequisite for Ubuntu 24.04+.
if [[ -d /etc/apparmor.d ]]; then
  sudo tee /etc/apparmor.d/bwrap >/dev/null <<'APPARMOR'
abi <abi/4.0>,
include <tunables/global>
profile bwrap /usr/bin/bwrap flags=(unconfined) {
  userns,
  include if exists <local/bwrap>
}
APPARMOR
  sudo systemctl reload apparmor || true
fi

cat <<'NEXT'
Base OS packages installed.
Next, install explicitly pinned project toolchains:
  - Docker Engine + Compose v2 from Docker official repository
  - Go 1.27.x (currently 1.27.1 at spec generation)
  - Node.js 24 LTS (currently 24.21.0 at spec generation)
  - pnpm version pinned by package.json
  - Python 3.12+ and uv
  - Claude Code stable channel
Then run: claude --version && claude doctor
Finally run: rddev doctor (after T0009 is implemented)
NEXT

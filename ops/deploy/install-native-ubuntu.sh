#!/usr/bin/env bash
# Install POST's host-native prerequisites on Ubuntu 24.04 amd64.
# This installs software only. It does not deploy POST, create application
# credentials/databases, open public listeners, or obtain a TLS certificate.
set -euo pipefail

GO_VERSION=1.27.1
GO_SHA256=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
NODE_VERSION=24.21.0
PNPM_VERSION=12.4.1
UV_VERSION=0.12.19
PGVECTOR_VERSION=0.8.6
PGVECTOR_COMMIT=8ee86c96f0fd72390f890aa8a336fda6d3ab4c6c
GITEA_VERSION=1.27.3
# The repository's dev image is older. This release fixes a published MinIO
# service-account policy issue; source builds are the upstream-supported path.
MINIO_VERSION=RELEASE.2025-10-15T17-29-55Z
MC_VERSION=RELEASE.2025-08-13T08-35-41Z

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || die 'Ubuntu amd64 is required'
# Fixed OS identity file on Ubuntu.
# shellcheck disable=SC1091
source /etc/os-release
[[ ${ID:-} == ubuntu && ${VERSION_ID:-} == 24.04 ]] || die 'Ubuntu 24.04 is required'

verify() {
  log 'Checking installed components'
  go version
  node --version
  npm --version
  pnpm --version
  uv --version
  python3 --version
  psql --version
  redis-server --version
  gitea --version
  minio --version
  mc --version
  nginx -v
  certbot --version
  git lfs version
  pg_config --version
  local vector_version
  vector_version=$(sudo -n -u postgres psql -Atqc \
    "SELECT default_version FROM pg_available_extensions WHERE name = 'vector'" 2>/dev/null)
  [[ $vector_version == "$PGVECTOR_VERSION" ]] || die "pgvector $PGVECTOR_VERSION is unavailable (found: ${vector_version:-none})"
  [[ $(cat /usr/local/share/post/minio-source-version 2>/dev/null) == "$MINIO_VERSION" ]] || die 'MinIO source version marker is missing'
  [[ $(cat /usr/local/share/post/mc-source-version 2>/dev/null) == "$MC_VERSION" ]] || die 'MinIO Client source version marker is missing'
  systemctl is-active --quiet postgresql || die 'PostgreSQL is not running'
  pg_isready -h 127.0.0.1 >/dev/null || die 'PostgreSQL is not accepting local connections'
  systemctl is-active --quiet redis-server || die 'Redis is not running'
  [[ $(redis-cli --raw CONFIG GET appendonly | tail -n 1) == yes ]] || die 'Redis AOF is not enabled'
  log 'All installable host-native prerequisites are present'
}

case ${1:---install} in
  --verify) verify; exit 0 ;;
  --install) ;;
  *) die 'usage: install-native-ubuntu.sh [--install|--verify]' ;;
esac

if [[ $EUID -ne 0 ]]; then
  exec sudo -n bash "$0" --install
fi
export DEBIAN_FRONTEND=noninteractive
export PATH=/usr/local/bin:/usr/local/go/bin:$PATH
work=$(mktemp -d /tmp/post-native-install.XXXXXX)
trap 'rm -rf "$work"' EXIT

log 'Installing Ubuntu packages'
apt-get update
apt-get install -y --no-install-recommends \
  build-essential git git-lfs curl wget ca-certificates gnupg jq unzip zip \
  make pkg-config rsync ripgrep lsof tmux htop \
  postgresql-16 postgresql-client-16 postgresql-server-dev-16 \
  redis-server nginx certbot python3-certbot-nginx \
  python3-venv python3-pip libssl-dev xz-utils

# Nginx has no POST site configuration yet. Do not publish its default page.
systemctl disable --now nginx >/dev/null 2>&1 || true
systemctl enable --now postgresql redis-server

log "Installing Go $GO_VERSION"
if [[ ! -x /usr/local/go-$GO_VERSION/bin/go ]]; then
  curl -fsSL --retry 3 "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    -o "$work/go.tar.gz"
  printf '%s  %s\n' "$GO_SHA256" "$work/go.tar.gz" | sha256sum -c -
  mkdir -p "/usr/local/go-$GO_VERSION"
  tar -xzf "$work/go.tar.gz" -C "/usr/local/go-$GO_VERSION" --strip-components=1
fi
ln -sfn "/usr/local/go-$GO_VERSION/bin/go" /usr/local/bin/go
ln -sfn "/usr/local/go-$GO_VERSION/bin/gofmt" /usr/local/bin/gofmt

log "Installing Node.js $NODE_VERSION and pnpm $PNPM_VERSION"
node_dir="/opt/node-v${NODE_VERSION}-linux-x64"
if [[ ! -x $node_dir/bin/node ]]; then
  node_file="node-v${NODE_VERSION}-linux-x64.tar.xz"
  curl -fsSL --retry 3 "https://nodejs.org/dist/v${NODE_VERSION}/${node_file}" \
    -o "$work/$node_file"
  curl -fsSL --retry 3 "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" \
    -o "$work/SHASUMS256.txt"
  (cd "$work" && grep "  ${node_file}\$" SHASUMS256.txt | sha256sum -c -)
  tar -xJf "$work/$node_file" -C /opt
fi
for binary in node npm npx corepack; do
  ln -sfn "$node_dir/bin/$binary" "/usr/local/bin/$binary"
done
if [[ ! -x $node_dir/bin/pnpm ]] || [[ $($node_dir/bin/pnpm --version) != "$PNPM_VERSION" ]]; then
  npm install --global --prefix "$node_dir" "pnpm@$PNPM_VERSION"
fi
ln -sfn "$node_dir/bin/pnpm" /usr/local/bin/pnpm
ln -sfn "$node_dir/bin/pnpx" /usr/local/bin/pnpx

log "Installing uv $UV_VERSION"
uv_dir="/opt/post-tools/uv-$UV_VERSION"
if [[ ! -x $uv_dir/bin/uv ]]; then
  python3 -m venv "$uv_dir"
  "$uv_dir/bin/pip" install --disable-pip-version-check --no-cache-dir "uv==$UV_VERSION"
fi
ln -sfn "$uv_dir/bin/uv" /usr/local/bin/uv
ln -sfn "$uv_dir/bin/uvx" /usr/local/bin/uvx

log "Installing pgvector $PGVECTOR_VERSION for PostgreSQL 16"
vector_version=$(sudo -n -u postgres psql -Atqc \
  "SELECT default_version FROM pg_available_extensions WHERE name = 'vector'" 2>/dev/null || true)
if [[ $vector_version != "$PGVECTOR_VERSION" ]]; then
  git clone --quiet --depth 1 --branch "v$PGVECTOR_VERSION" \
    https://github.com/pgvector/pgvector.git "$work/pgvector"
  [[ $(git -C "$work/pgvector" rev-parse HEAD) == "$PGVECTOR_COMMIT" ]] || \
    die 'pgvector tag does not resolve to the expected commit'
  make -C "$work/pgvector" PG_CONFIG=/usr/lib/postgresql/16/bin/pg_config
  make -C "$work/pgvector" PG_CONFIG=/usr/lib/postgresql/16/bin/pg_config install
fi

log 'Enabling Redis append-only persistence'
if ! grep -Eq '^appendonly[[:space:]]+yes([[:space:]]|$)' /etc/redis/redis.conf; then
  if grep -Eq '^[[:space:]#]*appendonly[[:space:]]+' /etc/redis/redis.conf; then
    sed -i -E 's/^[[:space:]#]*appendonly[[:space:]]+.*/appendonly yes/' \
      /etc/redis/redis.conf
  else
    printf '\nappendonly yes\n' >> /etc/redis/redis.conf
  fi
  systemctl restart redis-server
fi

log "Installing Gitea $GITEA_VERSION"
if [[ ! -x /usr/local/bin/gitea ]] || \
   ! /usr/local/bin/gitea --version 2>/dev/null | grep -Fq "version $GITEA_VERSION"; then
  gitea_file="gitea-${GITEA_VERSION}-linux-amd64"
  gitea_url="https://dl.gitea.com/gitea/${GITEA_VERSION}/${gitea_file}"
  curl -fsSL --retry 3 "$gitea_url" -o "$work/$gitea_file"
  curl -fsSL --retry 3 "$gitea_url.sha256" -o "$work/$gitea_file.sha256"
  (cd "$work" && sha256sum -c "$gitea_file.sha256")
  install -m 0755 "$work/$gitea_file" /usr/local/bin/gitea
fi
id -u gitea >/dev/null 2>&1 || \
  useradd --system --create-home --home-dir /var/lib/gitea --shell /usr/sbin/nologin gitea
install -d -o gitea -g gitea -m 0750 /var/lib/gitea /var/lib/gitea/data /var/lib/gitea/log
install -d -o root -g gitea -m 0750 /etc/gitea

log "Building MinIO $MINIO_VERSION from its upstream release"
install -d -m 0755 /usr/local/share/post
if [[ ! -x /usr/local/bin/minio ]] || \
   [[ $(cat /usr/local/share/post/minio-source-version 2>/dev/null) != "$MINIO_VERSION" ]]; then
  mkdir -p "$work/minio-bin"
  GOBIN="$work/minio-bin" GOTOOLCHAIN=local \
    go install "github.com/minio/minio@$MINIO_VERSION"
  install -m 0755 "$work/minio-bin/minio" /usr/local/bin/minio
  printf '%s\n' "$MINIO_VERSION" > /usr/local/share/post/minio-source-version
fi
id -u minio >/dev/null 2>&1 || \
  useradd --system --create-home --home-dir /var/lib/minio --shell /usr/sbin/nologin minio
install -d -o minio -g minio -m 0750 /var/lib/minio /var/lib/minio/data

log "Building MinIO Client $MC_VERSION from its upstream release"
if [[ ! -x /usr/local/bin/mc ]] || \
   [[ $(cat /usr/local/share/post/mc-source-version 2>/dev/null) != "$MC_VERSION" ]]; then
  mkdir -p "$work/mc-bin"
  GOBIN="$work/mc-bin" GOTOOLCHAIN=local \
    go install "github.com/minio/mc@$MC_VERSION"
  install -m 0755 "$work/mc-bin/mc" /usr/local/bin/mc
  printf '%s\n' "$MC_VERSION" > /usr/local/share/post/mc-source-version
fi

log 'Final verification'
verify
printf '\nInstalled software is ready for POST configuration. Gitea, MinIO, and Nginx\n'
printf 'remain stopped until their credentials, domain, and service files are prepared.\n'

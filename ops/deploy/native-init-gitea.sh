#!/usr/bin/env bash
# Create the internal Gitea identities for a native POST trial.
set -euo pipefail
umask 077
[[ $EUID -eq 0 ]] || { echo 'Run with sudo' >&2; exit 1; }
[[ -f /etc/post/secrets.env ]] || { echo 'Run native-init-db.sh first' >&2; exit 1; }
# Generated root-only file.
# shellcheck disable=SC1091
source /etc/post/secrets.env

gitea_cli=(runuser -u gitea -- /usr/local/bin/gitea
  --work-path /var/lib/gitea --config /etc/gitea/app.ini)

if ! "${gitea_cli[@]}" admin user list --admin | awk '$2 == "postadmin" { found=1 } END { exit !found }'; then
  "${gitea_cli[@]}" admin user create --admin --username postadmin \
    --password "$GITEA_ADMIN_PASSWORD" --email postadmin@localhost \
    --must-change-password=false >/dev/null
fi

# Gitea's admin user list omits bot accounts, so check their row directly.
if [[ $(runuser -u postgres -- psql -d gitea -Atqc \
  "SELECT 1 FROM \"user\" WHERE name = 'post-git-svc'") != 1 ]]; then
  "${gitea_cli[@]}" admin user create --username post-git-svc \
    --email post-git-svc@localhost --user-type bot >/dev/null
fi

if [[ ! -s /etc/post/gitea-token ]]; then
  # This internal bot token is scoped broadly for the initial feature trial.
  # Keep it out of terminal output and tighten its scopes after the Git API
  # surface has been exercised end to end.
  "${gitea_cli[@]}" admin user generate-access-token \
    --username post-git-svc --token-name post-native \
    --scopes all --raw > /etc/post/gitea-token
  chmod 0600 /etc/post/gitea-token
fi

echo 'Gitea admin and bot account are ready; bot token is stored on the host.'

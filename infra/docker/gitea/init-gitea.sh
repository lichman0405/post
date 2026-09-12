#!/bin/sh
# Idempotent Gitea init for local dev — runs INSIDE the gitea container:
#
#   docker compose exec -T --user git gitea sh < infra/docker/gitea/init-gitea.sh
#   (or simply: make infra-init)
#
# Creates — without ever deleting or recreating:
#   - the instance admin user (postadmin), via the gitea CLI,
#   - the test organization (post-test), owned by the admin,
#   - the service account (post-git-svc): a token-only bot user. Gitea >= 1.27
#     removed native "service accounts"; bot users are its machine-identity
#     mechanism, so the platform's GitProvider actor is modeled as a bot,
#     - plus an org team (svc, write permission) the bot belongs to, because
#       Gitea 1.27 removed direct org-membership endpoints.
#
# Every step is existence-checked, so re-running is a no-op. All values are
# DEV-ONLY defaults, overridable through environment variables.
set -eu

ADMIN_USER="${GITEA_ADMIN_USER:-postadmin}"
ADMIN_PASSWORD="${GITEA_ADMIN_PASSWORD:-postadmin_dev_pw}"
ADMIN_EMAIL="${GITEA_ADMIN_EMAIL:-postadmin@localhost}"
ORG_NAME="${GITEA_TEST_ORG:-post-test}"
SVC_USER="${GITEA_SERVICE_ACCOUNT:-post-git-svc}"
SVC_EMAIL="${GITEA_SERVICE_ACCOUNT_EMAIL:-post-git-svc@localhost}"
TEAM_NAME="${GITEA_SVC_TEAM:-svc}"
API="http://localhost:3000/api/v1"

# -- admin user -------------------------------------------------------------
echo "== admin user '$ADMIN_USER'"
if gitea admin user list --admin 2>/dev/null | grep -qw "$ADMIN_USER"; then
  echo "   already exists (no-op)"
else
  gitea admin user create --admin \
    --username "$ADMIN_USER" --password "$ADMIN_PASSWORD" \
    --email "$ADMIN_EMAIL" --must-change-password=false
fi

# -- test organization ------------------------------------------------------
echo "== test organization '$ORG_NAME'"
if curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" "$API/orgs/$ORG_NAME" >/dev/null 2>&1; then
  echo "   already exists (no-op)"
else
  curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" -X POST "$API/orgs" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"$ORG_NAME\",\"description\":\"POST local dev test org\"}" >/dev/null
  echo "   created"
fi

# -- service account (bot user) ---------------------------------------------
echo "== service account '$SVC_USER' (token-only bot user)"
if curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" "$API/users/$SVC_USER" >/dev/null 2>&1; then
  echo "   already exists (no-op)"
else
  gitea admin user create --username "$SVC_USER" --email "$SVC_EMAIL" \
    --user-type bot --access-token --access-token-name svc-token
fi

# -- svc team (the bot's org membership) -------------------------------------
echo "== team '$TEAM_NAME' in '$ORG_NAME'"
TEAM_JSON="$(curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" "$API/orgs/$ORG_NAME/teams/search?q=$TEAM_NAME")"
TEAM_ID="$(printf '%s' "$TEAM_JSON" | grep -o '"id":[0-9]*' | head -n1 | cut -d: -f2)"
if [ -n "$TEAM_ID" ]; then
  echo "   already exists (id=$TEAM_ID, no-op)"
else
  TEAM_JSON="$(curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" -X POST "$API/orgs/$ORG_NAME/teams" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$TEAM_NAME\",\"description\":\"POST platform bot team (dev)\",\"permission\":\"write\",\"can_create_org_repo\":true,\"includes_all_repositories\":true,\"units\":[\"repo.code\",\"repo.issues\",\"repo.pulls\",\"repo.releases\",\"repo.wiki\",\"repo.projects\",\"repo.actions\"]}")"
  TEAM_ID="$(printf '%s' "$TEAM_JSON" | grep -o '"id":[0-9]*' | head -n1 | cut -d: -f2)"
  echo "   created (id=$TEAM_ID)"
fi

if curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" "$API/teams/$TEAM_ID/members/$SVC_USER" >/dev/null 2>&1; then
  echo "   '$SVC_USER' already a member (no-op)"
else
  curl -fsS -u "$ADMIN_USER:$ADMIN_PASSWORD" -X PUT "$API/teams/$TEAM_ID/members/$SVC_USER" >/dev/null
  echo "   '$SVC_USER' added to team"
fi

echo "== done"

#!/usr/bin/env bash
# Wait until every service in the compose stack reports healthy.
#
#   bash infra/docker/wait-healthy.sh [timeout-seconds]   # default 300
#
# Exits 0 once `docker compose ps` shows "healthy" for every service
# (services without healthchecks count as unhealthy and will time out).
set -u

TIMEOUT="${1:-300}"
DEADLINE=$(( $(date +%s) + TIMEOUT ))

while true; do
  STATUS="$(docker compose ps --format '{{.Name}}\t{{.Health}}' 2>/dev/null)" || {
    echo "ERROR: stack is not running — run 'docker compose up -d' first" >&2
    exit 1
  }
  if [[ -z "$STATUS" ]] || printf '%s\n' "$STATUS" | grep -qv 'healthy$'; then
    # Still starting/unhealthy (or nothing running): keep waiting.
    if (( $(date +%s) >= DEADLINE )); then
      echo "ERROR: services not healthy after ${TIMEOUT}s:" >&2
      docker compose ps >&2
      exit 1
    fi
    sleep 5
    continue
  fi
  echo "all services healthy:"
  docker compose ps
  exit 0
done

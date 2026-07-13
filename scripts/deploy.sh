#!/usr/bin/env bash
#
# deploy.sh — build + ship the Go API to the VM, the same steps that were being
# typed by hand every release (tar the source → scp → back up the remote copy →
# extract → docker compose build/up → health check). Scripting it removes the
# "skip a step" risk called out in the fix checklist (§2).
#
# Config is read from scripts/deploy.env (gitignored — never commit host/IP), or
# from the environment. Copy scripts/deploy.env.example to scripts/deploy.env and
# fill it in. Required: VM_HOST (user@host), REMOTE_DIR (path to golangserver on
# the VM). Optional: SSH_OPTS, COMPOSE_FILE, HEALTH_URL.
#
# Usage:
#   bash scripts/deploy.sh            # build + deploy
#   DRY_RUN=1 bash scripts/deploy.sh  # tar + print what would run, ship nothing
#
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"
[ -f "$here/deploy.env" ] && . "$here/deploy.env"

: "${VM_HOST:?set VM_HOST (user@host) in scripts/deploy.env}"
: "${REMOTE_DIR:?set REMOTE_DIR (golangserver path on the VM) in scripts/deploy.env}"
SSH_OPTS="${SSH_OPTS:--o StrictHostKeyChecking=no}"
COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.prod.yml}"
COMPOSE_DIR="${COMPOSE_DIR:-$REMOTE_DIR/deployments/docker}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8787/health}"
DRY_RUN="${DRY_RUN:-0}"

# 1. Local gates — never ship code that doesn't build/vet or has the migration bug.
echo "==> local checks"
( cd "$root" && go build ./... && go vet ./... && bash scripts/check_migrations.sh )

# 2. Package exactly the paths the container build needs (no .env, no .git).
stamp="$(date +%Y%m%d-%H%M%S)"
tarball="/tmp/golangserver-${stamp}.tar.gz"
echo "==> packaging $tarball"
( cd "$root" && tar -czf "$tarball" cmd internal pkg migrations go.mod go.sum )

if [ "$DRY_RUN" = "1" ]; then
  echo "DRY_RUN=1 — built + packaged only; nothing shipped."
  echo "would scp $tarball to $VM_HOST:/tmp/ and rebuild in $COMPOSE_DIR"
  exit 0
fi

# 3. Ship + swap on the VM: back up the current source, extract, rebuild, restart.
echo "==> uploading to $VM_HOST"
scp $SSH_OPTS "$tarball" "$VM_HOST:/tmp/"

remote_tar="/tmp/$(basename "$tarball")"
echo "==> remote build + up"
ssh $SSH_OPTS "$VM_HOST" "set -e
  mkdir -p /tmp/deploy-backup-${stamp}
  (cd '$REMOTE_DIR' && tar -czf /tmp/deploy-backup-${stamp}/golangserver-src.tar.gz cmd internal pkg migrations go.mod go.sum)
  tar -xzf '$remote_tar' -C '$REMOTE_DIR'
  cd '$COMPOSE_DIR'
  set -a; . '$REMOTE_DIR/.env'; set +a
  docker compose -f '$COMPOSE_FILE' build api
  docker compose -f '$COMPOSE_FILE' up -d api"

# 4. Verify it came back healthy.
echo "==> health check"
sleep 4
ssh $SSH_OPTS "$VM_HOST" "curl -fsS -o /dev/null -w 'health: %{http_code}\n' '$HEALTH_URL' \
  && docker ps --format '{{.Names}}\t{{.Status}}' | grep -E 'api' || { echo 'HEALTH CHECK FAILED'; exit 1; }"

echo "==> done. remote backup: /tmp/deploy-backup-${stamp}/"

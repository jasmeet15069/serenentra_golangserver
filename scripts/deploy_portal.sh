#!/usr/bin/env bash
#
# deploy_portal.sh — ship an already-built tenant portal to the VM.
#
# The portal is a *prebuilt* deploy: the VM holds only Dockerfile + .output, not
# source. This packages those two, backs up the remote copy, extracts, rebuilds
# the container and waits for it to report healthy.
#
# Expects the build to have happened already (ship.sh does it, with the preset
# guard). Refuses to ship a build without .output/server/index.mjs, because a
# bare `npm run build` auto-detects the Cloudflare preset, produces no server
# entry, and the container crash-loops into a 502.
#
#   bash scripts/deploy_portal.sh
#   DRY_RUN=1 bash scripts/deploy_portal.sh
#
# Reads scripts/deploy.env (gitignored) for VM_HOST / REMOTE_DIR.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
portal="$(cd "$root/../HMS admin portal" && pwd)"

[ -f "$root/scripts/deploy.env" ] && . "$root/scripts/deploy.env"
VM_HOST="${VM_HOST:?VM_HOST not set (scripts/deploy.env)}"
REMOTE_DIR="${REMOTE_DIR:-/opt/hms/mhms_final/golangserver}"
SSH_OPTS="${SSH_OPTS:-}"
DRY_RUN="${DRY_RUN:-0}"

remote_root="$(dirname "$REMOTE_DIR")"
remote_portal="$remote_root/HMS admin portal"
compose_dir="$REMOTE_DIR/deployments/docker"
compose_file="${COMPOSE_FILE:-docker-compose.prod.yml}"

if [ ! -f "$portal/.output/server/index.mjs" ]; then
  echo "refusing to ship: $portal/.output/server/index.mjs is missing." >&2
  echo "Build with:  cd '$portal' && NITRO_PRESET=node-server npm run build" >&2
  exit 1
fi

stamp="$(date +%Y%m%d-%H%M%S)"
tarball="/tmp/portal-${stamp}.tar.gz"
echo "==> packaging $tarball"
( cd "$portal" && tar -czf "$tarball" .output Dockerfile )

if [ "$DRY_RUN" = "1" ]; then
  echo "DRY_RUN=1 — packaged only; nothing shipped."
  exit 0
fi

echo "==> uploading to $VM_HOST"
scp $SSH_OPTS "$tarball" "$VM_HOST:/tmp/"

echo "==> remote extract + rebuild"
ssh $SSH_OPTS "$VM_HOST" "set -e
  mkdir -p /tmp/portal-backup-${stamp}
  if [ -d '$remote_portal/.output' ]; then
    cp -r '$remote_portal/.output' /tmp/portal-backup-${stamp}/
  fi
  rm -rf '$remote_portal/.output'
  tar -xzf '/tmp/$(basename "$tarball")' -C '$remote_portal'
  test -f '$remote_portal/.output/server/index.mjs'
  cd '$compose_dir'
  docker compose --env-file '$REMOTE_DIR/.env' -f '$compose_file' build portal
  docker compose --env-file '$REMOTE_DIR/.env' -f '$compose_file' up -d portal"

echo "==> waiting for healthy"
for i in $(seq 1 20); do
  status="$(ssh $SSH_OPTS "$VM_HOST" "docker ps --filter name=portal --format '{{.Status}}'" || true)"
  case "$status" in
    *healthy*) echo "portal: $status"; echo "==> done. remote backup: /tmp/portal-backup-${stamp}/"; exit 0 ;;
    *) sleep 3 ;;
  esac
done

echo "portal did not report healthy in time; last status: ${status:-unknown}" >&2
echo "roll back with: /tmp/portal-backup-${stamp}/.output" >&2
exit 1

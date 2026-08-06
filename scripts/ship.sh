#!/usr/bin/env bash
#
# ship.sh — the whole release cycle in one command, unattended.
#
# Replaces the hand-run sequence of build, test, lint, deploy, smoke-test,
# commit and push that this project repeats on every change. Each step gates
# the next, so nothing reaches production that has not passed the step before
# it, and nothing is committed that has not been shown working on the VM.
#
#   backend   go build -> go vet -> go test -> migration lint -> deploy -> health
#   frontend  tsc -> build (preset-guarded) -> ship -> health
#   both      smoke test -> commit -> push
#
# Usage:
#   bash scripts/ship.sh -m "commit message"      # backend + frontend
#   bash scripts/ship.sh -m "msg" --backend       # backend only
#   bash scripts/ship.sh -m "msg" --frontend      # frontend only
#   bash scripts/ship.sh --check                  # verify only: no deploy, no push
#   DRY_RUN=1 bash scripts/ship.sh -m "msg"       # everything except deploy/push
#
# Exit codes: 0 all good, 1 a gate failed (nothing was pushed).
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mhms="$(cd "$root/.." && pwd)"
portal="$mhms/HMS admin portal"

DO_BACKEND=1
DO_FRONTEND=1
CHECK_ONLY=0
MSG=""

while [ $# -gt 0 ]; do
  case "$1" in
    -m|--message) MSG="${2:-}"; shift 2 ;;
    --backend)    DO_FRONTEND=0; shift ;;
    --frontend)   DO_BACKEND=0; shift ;;
    --check)      CHECK_ONLY=1; shift ;;
    -h|--help)    sed -n '2,26p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

DRY_RUN="${DRY_RUN:-0}"
fail=0
step()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
ok()    { printf '    \033[32mok\033[0m   %s\n' "$*"; }
bad()   { printf '    \033[31mFAIL\033[0m %s\n' "$*"; fail=1; }
skip()  { printf '    \033[33mskip\033[0m %s\n' "$*"; }

# run <label> <command...> — a failing command records a failure and returns 1,
# so callers can gate on it without set -e killing the summary.
run() {
  local label="$1"; shift
  local out
  if out="$("$@" 2>&1)"; then
    ok "$label"
    return 0
  fi
  bad "$label"
  printf '%s\n' "$out" | tail -20 | sed 's/^/         /'
  return 1
}

# ---------------------------------------------------------------- backend
if [ "$DO_BACKEND" = 1 ]; then
  step "backend: static checks"
  run "go build"        bash -c "cd '$root' && go build ./..."
  run "go vet"          bash -c "cd '$root' && go vet ./..."
  run "go test"         bash -c "cd '$root' && go test ./..."
  run "migration lint"  bash -c "cd '$root' && bash scripts/check_migrations.sh"

  if [ "$fail" = 0 ] && [ "$CHECK_ONLY" = 0 ] && [ "$DRY_RUN" = 0 ]; then
    step "backend: deploy"
    run "deploy.sh" bash -c "cd '$root' && bash scripts/deploy.sh"
  else
    step "backend: deploy"; skip "check-only or dry-run"
  fi
fi

# --------------------------------------------------------------- frontend
if [ "$DO_FRONTEND" = 1 ] && [ -d "$portal" ]; then
  step "frontend: typecheck"
  run "tsc --noEmit" bash -c "cd '$portal' && npx tsc --noEmit"

  if [ "$fail" = 0 ] && [ "$CHECK_ONLY" = 0 ]; then
    step "frontend: build"
    # NITRO_PRESET is not optional. A bare build auto-detects the Cloudflare
    # preset, emits no .output/server/index.mjs, and the container crash-loops
    # — that took the portal down for ~4 minutes on 2026-08-05.
    if run "vite build (node-server preset)" \
         bash -c "cd '$portal' && rm -rf .output && NITRO_PRESET=node-server npm run build"; then
      if [ -f "$portal/.output/server/index.mjs" ]; then
        ok "preset guard: .output/server/index.mjs present"
      else
        bad "preset guard: no .output/server/index.mjs — WRONG PRESET, not shipping"
      fi
    fi

    if [ "$fail" = 0 ] && [ "$DRY_RUN" = 0 ]; then
      step "frontend: ship to VM"
      run "package + upload + rebuild" bash -c "cd '$root' && bash scripts/deploy_portal.sh"
    else
      step "frontend: ship to VM"; skip "dry-run or earlier failure"
    fi
  else
    step "frontend: build"; skip "check-only or earlier failure"
  fi
fi

# ------------------------------------------------------------- smoke test
step "smoke test (live)"
for url in \
  "https://testingxyz.serenentra.com/admin" \
  "https://testingxyz.serenentra.com/reservations/new" \
  "https://superadmin.serenentra.com/login"
do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 "$url" || echo 000)"
  if [ "$code" = "200" ]; then ok "$code $url"; else bad "$code $url"; fi
done

# The API's /health is bound to the VM's loopback and nginx does not expose it
# (the public /api/health is behind auth and answers 401), so check it the same
# way deploy.sh does — from inside the VM — rather than asserting on a 401.
if [ -f "$root/scripts/deploy.env" ]; then
  # shellcheck disable=SC1091
  . "$root/scripts/deploy.env"
  api_code="$(ssh ${SSH_OPTS:-} "$VM_HOST" \
    "curl -s -o /dev/null -w '%{http_code}' --max-time 20 http://127.0.0.1:8787/health" 2>/dev/null || echo 000)"
  if [ "$api_code" = "200" ]; then ok "$api_code api /health (via VM)"; else bad "$api_code api /health (via VM)"; fi
else
  skip "api /health — scripts/deploy.env not present"
fi

# ------------------------------------------------------- commit and push
if [ "$fail" != 0 ]; then
  step "commit + push"; skip "a gate failed — nothing committed or pushed"
elif [ "$CHECK_ONLY" = 1 ] || [ "$DRY_RUN" = 1 ]; then
  step "commit + push"; skip "check-only or dry-run"
elif [ -z "$MSG" ]; then
  step "commit + push"; skip "no -m message given"
else
  step "commit + push"
  for repo in "$root" "$portal"; do
    [ -d "$repo/.git" ] || continue
    name="$(basename "$repo")"
    if [ -z "$(cd "$repo" && git status --porcelain)" ]; then
      skip "$name: nothing to commit"
    else
      ( cd "$repo" && git add -A && git commit -q -m "$MSG" ) \
        && ok "$name: committed" || bad "$name: commit failed"
    fi
    # Push even when this run committed nothing: an earlier commit may be unpushed.
    pushed=0
    for attempt in 1 2 3; do
      if ( cd "$repo" && git push -q origin HEAD 2>/dev/null ); then pushed=1; break; fi
      sleep $((attempt * 5))   # the GitHub DNS lookup here fails intermittently
    done
    [ "$pushed" = 1 ] && ok "$name: pushed" || bad "$name: push failed after 3 attempts"
  done
fi

step "result"
if [ "$fail" = 0 ]; then
  printf '    \033[32mall gates passed\033[0m\n'
else
  printf '    \033[31mone or more gates failed — see above\033[0m\n'
fi
exit "$fail"

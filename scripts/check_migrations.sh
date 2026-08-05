#!/usr/bin/env bash
#
# check_migrations.sh — reject migration SQL that the boot-time runner cannot
# execute safely. Three checks, all of them things that have broken a deploy.
#
# The runner is splitSQLStatements + applyMigrations in
# internal/repository/postgres/schema.go. Two of its properties drive these rules:
# it splits a file on the statement separator with no awareness of comments or
# quoting, and it does not auto-baseline, so it re-runs any migration missing
# from the schema_migrations ledger.
#
#   1. separator inside a '--' comment
#      The splitter cuts on the separator FIRST and only then discards lines
#      starting with '--'. A separator inside a comment cuts a statement at the
#      wrong place and the remainder of the comment line survives as bogus SQL:
#
#          -- drop these<sep> they are stale
#          CREATE TABLE foo (...)<sep>
#
#      splits into "-- drop these" + " they are stale\nCREATE TABLE foo (...)",
#      and the second piece crash-loops the API on boot.
#
#   2. dollar-quoted bodies ($$ ... $$, $tag$ ... $tag$)
#      The splitter has no dollar-quote awareness, so a DO block, function body
#      or trigger body containing separators is shredded into invalid fragments.
#      No migration may use one. Where a guard is needed, use the idempotent
#      drop-then-add form in check 3 instead.
#
#   3. ADD CONSTRAINT without a matching DROP CONSTRAINT IF EXISTS
#      Postgres has no ADD CONSTRAINT IF NOT EXISTS, so a bare ADD fails with
#      SQLSTATE 42P07 when re-run. Because the runner re-applies any unrecorded
#      migration, that aborts start-up on a database whose schema exists but
#      whose ledger is incomplete — exactly the partially-migrated recovery case
#      the no-auto-baseline design was built to survive. Precede every ADD with
#      DROP CONSTRAINT IF EXISTS <same name>. The file runs in one transaction,
#      so the window without the constraint is never observable.
#
# Run locally:  bash scripts/check_migrations.sh
# In CI it prints GitHub Actions ::error annotations pointing at the exact line.
#
# Implementation note: every per-line test uses a bash builtin. An earlier
# version shelled out to grep/sed once per line, which meant thousands of
# process spawns and took ~4 minutes on Git Bash for ~1k lines; builtins bring
# it back to well under a second. Keep it that way — this runs on every CI job
# and every deploy.
#
# Known limitation: these are textual checks. A '--', a dollar-quote marker or
# the words ADD CONSTRAINT inside a single-quoted string literal would be
# flagged as a false positive. Migrations don't currently do that; if one
# legitimately needs to, rephrase or split the literal.
set -euo pipefail

# Case-insensitive [[ =~ ]] and case matching, so 'add constraint' is caught the
# same as 'ADD CONSTRAINT' without converting every line.
shopt -s nocasematch

dir="$(cd "$(dirname "$0")/.." && pwd)/migrations"
if [ ! -d "$dir" ]; then
  echo "migration lint: no migrations dir at $dir" >&2
  exit 1
fi

bad=0
count=0
while IFS= read -r -d '' f; do
  count=$((count + 1))
  base="${f##*/}"

  # Constraint names dropped anywhere in this file, space-delimited, and the
  # ADD sites to resolve against them once the whole file has been read. The
  # guard may legitimately appear after the ADD in source order.
  dropped=" "
  adds=""

  n=0
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))

    # --- 1. statement separator inside a '--' comment ---
    case "$line" in
      *--*)
        comment="${line#*--}"      # everything after the first '--'
        case "$comment" in
          *\;*)
            echo "::error file=migrations/${base},line=${n}:: semicolon inside a '--' comment breaks the SQL splitter"
            printf '  %s:%s: %s\n' "$base" "$n" "$line" >&2
            bad=1
            ;;
        esac
        ;;
    esac

    # Everything before the first '--' is the executable part of the line.
    # Checks 2 and 3 only apply there, so a rule can still be *described* in a
    # comment without tripping it.
    code="${line%%--*}"

    # --- 2. dollar-quoted body ---
    # Matches $$ and named tags such as $func$ — anything the splitter would
    # mishandle.
    if [[ "$code" == *'$$'* || "$code" =~ \$[A-Za-z_][A-Za-z_0-9]*\$ ]]; then
      echo "::error file=migrations/${base},line=${n}:: dollar-quoted body — the SQL splitter has no dollar-quote awareness and will cut it into invalid fragments"
      printf '  %s:%s: %s\n' "$base" "$n" "$line" >&2
      bad=1
    fi

    # --- 3. collect DROP guards and ADD sites for this file ---
    if [[ "$code" =~ DROP[[:space:]]+CONSTRAINT[[:space:]]+IF[[:space:]]+EXISTS[[:space:]]+\"?([A-Za-z0-9_]+) ]]; then
      dropped+="${BASH_REMATCH[1]} "
    fi
    if [[ "$code" =~ ADD[[:space:]]+CONSTRAINT[[:space:]]+\"?([A-Za-z0-9_]+) ]]; then
      adds+="${n} ${BASH_REMATCH[1]}"$'\n'
    fi
  done < "$f"

  # Resolve each ADD against the guards found anywhere in the same file.
  while read -r ln cname; do
    [ -n "$cname" ] || continue
    if [[ "$dropped" != *" $cname "* ]]; then
      echo "::error file=migrations/${base},line=${ln}:: ADD CONSTRAINT ${cname} is not idempotent — add 'ALTER TABLE ... DROP CONSTRAINT IF EXISTS ${cname};' before it"
      printf '  %s:%s: missing DROP CONSTRAINT IF EXISTS %s\n' "$base" "$ln" "$cname" >&2
      bad=1
    fi
  done <<< "$adds"
done < <(find "$dir" -name '*.sql' -print0 | sort -z)

if [ "$bad" -ne 0 ]; then
  echo "migration lint FAILED — see the annotations above." >&2
  exit 1
fi
echo "migration lint OK (${count} files checked)"

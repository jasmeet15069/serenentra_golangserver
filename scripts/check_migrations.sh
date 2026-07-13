#!/usr/bin/env bash
#
# check_migrations.sh — fail the build if any migrations/*.sql contains a ';'
# inside a '--' line comment.
#
# Why this exists: the boot-time migration splitter (splitSQLStatements in
# internal/repository/postgres/schema.go) splits the file on ';' FIRST and only
# then discards lines that start with '--'. A ';' sitting inside a comment
# therefore cuts a statement at the wrong place, and the non-comment remainder of
# the comment line ("... they are stale" below) survives as bogus SQL:
#
#     -- drop these; they are stale
#     CREATE TABLE foo (...);
#
# splits into  "-- drop these"  +  " they are stale\nCREATE TABLE foo (...)"  and
# the second piece is invalid SQL that crash-loops the API on boot. This has bitten
# the deploy more than once and is trivial to catch mechanically, so we do.
#
# Run locally:  bash scripts/check_migrations.sh
# In CI it prints GitHub Actions ::error annotations pointing at the exact line.
#
# Known limitation: a literal '--' followed by ';' inside a single-quoted string
# literal would be flagged as a false positive. Migrations don't currently do that;
# if one legitimately needs to, rephrase or split the literal.
set -euo pipefail

dir="$(cd "$(dirname "$0")/.." && pwd)/migrations"
if [ ! -d "$dir" ]; then
  echo "migration lint: no migrations dir at $dir" >&2
  exit 1
fi

bad=0
count=0
while IFS= read -r -d '' f; do
  count=$((count + 1))
  n=0
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    case "$line" in
      *--*)
        comment="${line#*--}"      # everything after the first '--'
        case "$comment" in
          *\;*)
            echo "::error file=migrations/$(basename "$f"),line=${n}:: semicolon inside a '--' comment breaks the SQL splitter"
            printf '  %s:%s: %s\n' "$(basename "$f")" "$n" "$line" >&2
            bad=1
            ;;
        esac
        ;;
    esac
  done < "$f"
done < <(find "$dir" -name '*.sql' -print0 | sort -z)

if [ "$bad" -ne 0 ]; then
  echo "migration lint FAILED — move the ';' out of the comment (or rephrase it)." >&2
  exit 1
fi
echo "migration lint OK (${count} files checked)"

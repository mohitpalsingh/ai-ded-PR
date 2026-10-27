#!/usr/bin/env bash
# Builds a deterministic git repo that reproduces the "large PR" rendering case:
#   - one file with 1k+ changed lines (a 1,200-line rewrite = ~2,400 diff lines)
#   - 120 additional changed files (enough to overflow any sane sidebar)
#
# Two branches are created so the fixture can be reviewed like a real PR:
#   base   -> before the change
#   large-pr -> after the change (checked out)
#
# Usage: scripts/make-large-pr-fixture.sh [out-dir] [big-file-lines] [other-files]
set -euo pipefail

OUT="${1:-testdata/large-pr-repo}"
LOC="${2:-1200}"
OTHERS="${3:-120}"

rm -rf "$OUT"
mkdir -p "$OUT"
cd "$OUT"

git init -q -b main
git config user.email "fixture@example.com"
git config user.name "large-pr fixture"
git config commit.gpgsign false

mkdir -p internal/service pkg/util

# ---- base ----
{
  echo "package service"
  echo
  i=0
  while [ "$i" -lt "$LOC" ]; do
    echo "func handler$i(x int) int { return x + $i }"
    i=$((i + 1))
  done
} > internal/service/pipeline.go

i=0
while [ "$i" -lt "$OTHERS" ]; do
  printf 'package util\n\nfunc Util%d() int { return %d }\n' "$i" "$i" > "pkg/util/util$i.go"
  i=$((i + 1))
done

git add -A
GIT_AUTHOR_DATE="2026-01-01T00:00:00Z" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z" \
  git commit -qm "base"
git branch base

# ---- head: rewrite every line of the big file, touch every other file ----
{
  echo "package service"
  echo
  i=0
  while [ "$i" -lt "$LOC" ]; do
    if [ "$i" -eq $((LOC - 1)) ]; then
      echo "func handler$i(x int) int { return x*2 + $i } // EOF-MARKER-$LOC"
    else
      echo "func handler$i(x int) int { return x*2 + $i } // changed line $i"
    fi
    i=$((i + 1))
  done
} > internal/service/pipeline.go

i=0
while [ "$i" -lt "$OTHERS" ]; do
  printf 'package util\n\nfunc Util%d() int { return %d }\n' "$i" "$((i + 1))" > "pkg/util/util$i.go"
  i=$((i + 1))
done

git add -A
GIT_AUTHOR_DATE="2026-01-01T00:00:01Z" GIT_COMMITTER_DATE="2026-01-01T00:00:01Z" \
  git commit -qm "head: rewrite pipeline.go and touch $OTHERS files"
git branch large-pr
git checkout -q large-pr

echo "fixture: $(pwd)"
echo "  base...large-pr: $(git diff --shortstat base...large-pr)"
echo "  big-file diff lines: $(git diff base...large-pr -- internal/service/pipeline.go | wc -l | tr -d ' ')"

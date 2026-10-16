#!/usr/bin/env bash
# Regression suite for ai-ded-pr. Verifies:
#   1. unit tests (rationale formatting, tolerance to model JSON quirks,
#      persisted-summary override, state keying, download resume) with -race
#   2. heuristic dump: every file gets a canonical 4-bullet rationale
#   3. AI dump (default model): every file gets an AI-authored 4-bullet rationale
#   4. TUI: model ready, ask rename, summaries for every file, no loading stalls
#   5. SIGINT: graceful shutdown, no llama-server residue, memory watermark
#   6. summaries persist once and are never recalculated (--no-ai run reuses them)
#   7. ask subcommand + clean shutdown (no stray processes, ports free)
#
# Usage: scripts/regression.sh [repo-path]
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REAL_HOME="$HOME"
TARGET_REPO="${1:-$REAL_HOME/Documents/workspace/ap-comms-dds}"
BIN="/tmp/ai-ded-pr-regression"
DEFAULT_MODEL="$REAL_HOME/.cache/ai-pr-review/models/gemma-3-4b-it-Q4_K_M.gguf"
FALLBACK_MODEL="${AI_DED_PR_TEST_MODEL:-/tmp/gemma1b.gguf}"

pass=0
fail=0
ok()  { echo "  PASS: $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL: $1"; fail=$((fail + 1)); }

check() { # check <description> <expected> <actual>
  if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (expected $2, got $3)"; fi
}
has() { # has <file> <pattern> -> at least one match
  [ "$(grep -c -- "$2" "$1")" -ge 1 ]
}

strip_ansi() { sed 's/\x1b\[[0-9;?]*[a-zA-Z]//g' "$1"; }

# Isolate persisted state + config so regression runs never touch real review
# progress. The model cache is symlinked so the real default model is used.
GOCACHE_REAL="$(go env GOCACHE 2>/dev/null || true)"
TEST_HOME="$(mktemp -d)"
mkdir -p "$TEST_HOME/.cache/ai-pr-review"
ln -sfn "$REAL_HOME/.cache/ai-pr-review/models" "$TEST_HOME/.cache/ai-pr-review/models"
export HOME="$TEST_HOME"
[ -n "$GOCACHE_REAL" ] && export GOCACHE="$GOCACHE_REAL"
trap 'rm -rf "$TEST_HOME"' EXIT
STATE_DIR="$HOME/.cache/ai-pr-review/state"

echo "== 0. build & unit tests =="
(cd "$ROOT_DIR" && go build -o "$BIN" ./cmd/review && go vet ./... && go test -race ./...) || { echo "build/test failure"; exit 1; }
ok "go build + vet + test (race)"
V="$("$BIN" --version 2>/dev/null)"
case "$V" in ai-ded-pr*) ok "version flag ($V)";; *) bad "version flag broken ($V)";; esac

echo "== 1. repo =="
if ! git -C "$TARGET_REPO" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "not a git repo: $TARGET_REPO"; exit 1
fi
REPO_ABS="$(cd "$TARGET_REPO" && pwd)"
ok "target repo $REPO_ABS"

FILES="$("$BIN" --repo "$REPO_ABS" --base HEAD~1 --head HEAD --no-ai --dump 2>/dev/null | sed -n 's/.*files=\([0-9]*\).*/\1/p' | head -1)"
[ -n "$FILES" ] && [ "$FILES" -gt 0 ] || { echo "could not determine file count"; exit 1; }
ok "file count detected ($FILES)"

echo "== 2. heuristic dump: canonical 4-bullet for every file =="
"$BIN" --repo "$REPO_ABS" --base HEAD~1 --head HEAD --no-ai --dump > /tmp/regr-noai.txt 2>&1
check "heuristic Intent bullets"    "$FILES" "$(grep -c -- 'rationale: - Intent:' /tmp/regr-noai.txt)"
check "heuristic Dependencies"      "$FILES" "$(grep -c -- '^- Dependencies:' /tmp/regr-noai.txt)"
check "heuristic Verify"            "$FILES" "$(grep -c -- '^- Verify:' /tmp/regr-noai.txt)"
check "heuristic Risk"              "$FILES" "$(grep -c -- '^- Risk:' /tmp/regr-noai.txt)"

echo "== 3. AI dump (default model preferred) =="
MODEL_ARGS=()
USE_DEFAULT=0
if [ -f "$DEFAULT_MODEL" ]; then
  USE_DEFAULT=1; echo "  using default model: $DEFAULT_MODEL"
elif [ -f "$FALLBACK_MODEL" ]; then
  MODEL_ARGS=(--model "$FALLBACK_MODEL"); echo "  using fallback model: $FALLBACK_MODEL"
else
  bad "no model available (install one or set AI_DED_PR_TEST_MODEL)"; MODEL_ARGS=(--no-ai)
fi
"$BIN" --repo "$REPO_ABS" --base HEAD~1 --head HEAD --ai-dump --dump ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"} > /tmp/regr-ai.txt 2>&1
AI_AUTHORED="$(grep -c -- 'rationale: - Intent: modifies ' /tmp/regr-ai.txt || true)"
AI_AUTHORED=$((FILES - AI_AUTHORED))
check "AI Intent bullets"        "$FILES" "$(grep -c -- 'rationale: - Intent:' /tmp/regr-ai.txt)"
check "AI Dependencies"          "$FILES" "$(grep -c -- '^- Dependencies:' /tmp/regr-ai.txt)"
check "AI Verify"                "$FILES" "$(grep -c -- '^- Verify:' /tmp/regr-ai.txt)"
check "AI Risk"                  "$FILES" "$(grep -c -- '^- Risk:' /tmp/regr-ai.txt)"
check "AI errors"                "0"      "$(grep -c 'ERROR' /tmp/regr-ai.txt || true)"
if [ "$USE_DEFAULT" = "1" ]; then
  check "AI-authored rationales (all files)" "$FILES" "$AI_AUTHORED"
else
  [ "$AI_AUTHORED" -ge 1 ] && ok "AI-authored rationales ($AI_AUTHORED)" || bad "no AI-authored rationales"
fi
DEPS_FILES="$(grep -c -- 'deps:' /tmp/regr-ai.txt || true)"
[ "$DEPS_FILES" -ge 1 ] && ok "dependency jump links ($DEPS_FILES files)" || bad "no dependency links in AI plan"

echo "== 4. TUI with default model =="
pkill -f "llama-server" 2>/dev/null; sleep 1
( sleep 120; printf '?'; sleep 2; printf '\003' ) | script -q /dev/null "$BIN" --repo "$REPO_ABS" --base HEAD~1 --head HEAD ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"} > /tmp/regr-tui.txt 2>&1
strip_ansi /tmp/regr-tui.txt > /tmp/regr-tui.clean.txt
has /tmp/regr-tui.clean.txt "model ready" && ok "model ready shown" || bad "model ready not shown"
has /tmp/regr-tui.clean.txt "/ ask" && ok "ask footer (not search)" || bad "ask footer missing"
has /tmp/regr-tui.clean.txt "ask changed files" && ok "ask help text" || bad "ask help text missing"
has /tmp/regr-tui.clean.txt "jump to a changed dependency" && ok "dep jump help" || bad "dep jump help missing"
has /tmp/regr-tui.clean.txt "return after a dependency jump" && ok "dep back help" || bad "dep back help missing"
check "no loading stalls"     "0" "$(grep -c '(loading...)' /tmp/regr-tui.clean.txt || true)"
has /tmp/regr-tui.clean.txt "progress: 0/$FILES" && ok "progress line" || bad "progress line missing"

SLUG="$(printf '%s' "$REPO_ABS" | sed 's#[/:,?#*\\ ]#_#g')"
STATEFILE="$(ls -t "$STATE_DIR"/${SLUG}-*.json 2>/dev/null | head -1)"
if [ -n "$STATEFILE" ]; then
  read -r ENTRIES OKS < <(python3 - "$STATEFILE" <<'PY'
import json, sys
st = json.load(open(sys.argv[1]))
sums = st.get("summaries") or {}
labels = ("- Intent:", "- Dependencies:", "- Verify:", "- Risk:")
bad = sum(1 for s in sums.values() if any(l not in s for l in labels))
print(len(sums), len(sums) - bad)
PY
)
  check "persisted summaries"          "$FILES" "$ENTRIES"
  check "persisted canonical summaries" "$FILES" "$OKS"
  if [ "$USE_DEFAULT" = "1" ]; then
    HEUR="$(python3 -c "import json,sys;print(sum(1 for s in (json.load(open(sys.argv[1])).get('summaries') or {}).values() if s.startswith('- Intent: modifies ')))" "$STATEFILE")"
    check "persisted AI-authored" "$FILES" "$((FILES - HEUR))"
  fi
else
  bad "state file for $SLUG not found"
fi

echo "== 4b. SIGINT graceful shutdown (and memory watermark) =="
pkill -x llama-server 2>/dev/null; sleep 1
( sleep 120; printf '\003' ) | script -q /dev/null "$BIN" --repo "$REPO_ABS" --base HEAD~1 --head HEAD ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"} > /tmp/regr-sigint.txt 2>&1 &
STARTER_PID=$!
READY=0
for _ in $(seq 1 40); do
  if grep -q "model ready" /tmp/regr-sigint.txt 2>/dev/null; then READY=1; break; fi
  sleep 2
done
[ "$READY" = "1" ] && ok "sigint run reached model ready" || bad "sigint run never reached model ready"
LSPID="$(pgrep -x llama-server | head -1)"
if [ -n "$LSPID" ]; then
  RSS_KB="$(ps -o rss= -p "$LSPID" 2>/dev/null | tr -d ' ')"
  [ -n "$RSS_KB" ] && echo "  INFO: llama-server RSS ~$((RSS_KB / 1024)) MB (resident incl. shared file pages)"
fi
pkill -INT -f "$BIN --repo" 2>/dev/null
sleep 8
pgrep -f "$BIN --repo" >/dev/null 2>&1 && bad "TUI survived SIGINT" || ok "TUI exited on SIGINT"
pgrep -x llama-server >/dev/null 2>&1 && bad "llama-server survived SIGINT" || ok "llama-server stopped on SIGINT"
wait "$STARTER_PID" 2>/dev/null

echo "== 5. persisted summaries reused with --no-ai (never recalculated) =="
BEFORE="$(shasum "$STATEFILE" 2>/dev/null | awk '{print $1}')"
( sleep 10; printf '\003' ) | script -q /dev/null "$BIN" --repo "$REPO_ABS" --base HEAD~1 --head HEAD --no-ai > /tmp/regr-noai-tui.txt 2>&1
strip_ansi /tmp/regr-noai-tui.txt > /tmp/regr-noai-tui.clean.txt
has /tmp/regr-noai-tui.clean.txt "progress: 0/$FILES" && ok "no-ai auto-loads review screen" || bad "no-ai did not auto-load review screen"
[ "$(grep -c -- '- Intent:' /tmp/regr-noai-tui.clean.txt)" -ge 1 ] && ok "persisted summaries rendered without model" || bad "no persisted summary rendered in --no-ai run"
AFTER="$(shasum "$STATEFILE" 2>/dev/null | awk '{print $1}')"
check "state unchanged by --no-ai run" "$BEFORE" "$AFTER"

echo "== 6. ask subcommand =="
"$BIN" ask "graphql rendering" --repo "$REPO_ABS" --base HEAD~1 --head HEAD ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"} > /tmp/regr-ask.txt 2>&1
ASK_RC=$?
check "ask exits 0" "0" "$ASK_RC"
[ "$(grep -c 'Relevant files' /tmp/regr-ask.txt)" -ge 1 ] && ok "ask returns relevant files" || bad "ask returned no results"

echo "== 7. clean shutdown =="
sleep 1
LEFT="$(pgrep -f 'llama-server' | wc -l | tr -d ' ')"
check "no llama-server residue" "0" "$LEFT"
PORTS_BUSY=0
for p in 18973 18974 18993 18994; do
  if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then PORTS_BUSY=$((PORTS_BUSY + 1)); fi
done
check "ai-ded-pr ports free" "0" "$PORTS_BUSY"

echo
echo "======================================"
echo "  regression: $pass passed, $fail failed"
echo "======================================"
[ "$fail" -eq 0 ]

#!/usr/bin/env bash
# Push the committable reports under bench/reports/<sweep>/ to a dedicated
# branch on GitHub. Designed to run from the benchmarking host where you
# don't want to fiddle with SSH keys or push permissions on the working
# branch.
#
# Usage:
#   bench/scripts/push-report.sh                  # prompts for token, latest sweep
#   bench/scripts/push-report.sh <sweep>
#   bench/scripts/push-report.sh <sweep> <branch>
#
# Examples:
#   bench/scripts/push-report.sh
#   bench/scripts/push-report.sh v0.0.41-2026-04-27-baseline
#   bench/scripts/push-report.sh v0.0.41-2026-04-27-baseline custom-branch
#
# Token sources, in order of precedence:
#   1. $GH_TOKEN env var (highest)
#   2. Interactive prompt via `read -rsp` (silent, never shown on screen)
#
# Behaviour:
#   - Default branch name is `bench/<sweep>` so each sweep gets its own
#     isolated branch (review-friendly, easy to PR independently).
#   - Token is used inline for one push only — never written to .git/config.
#   - Uses a temporary worktree, so the current checkout is not touched.
#   - Idempotent: re-running with no new files emits "no changes" and exits 0.
#
set -euo pipefail

SWEEP=${1:-}
BRANCH=${2:-}

# Resolve token — prefer env, fall back to silent prompt
if [[ -n "${GH_TOKEN:-}" ]]; then
  TOKEN=$GH_TOKEN
else
  if [[ ! -t 0 ]]; then
    echo "[push-report] no GH_TOKEN env and stdin is not a TTY — cannot prompt"
    exit 1
  fi
  read -rsp 'GitHub token (will not echo): ' TOKEN
  echo
  [[ -z "$TOKEN" ]] && { echo "[push-report] empty token, abort"; exit 1; }
fi

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

# Auto-detect latest sweep if not given
if [[ -z "$SWEEP" ]]; then
  SWEEP=$(ls -t bench/reports 2>/dev/null | grep -v '^\.' | head -1 || true)
  [[ -z "$SWEEP" ]] && { echo "[push-report] no sweep dir under bench/reports/"; exit 1; }
fi
SRC="$REPO_ROOT/bench/reports/$SWEEP"
[[ ! -d "$SRC" ]] && { echo "[push-report] sweep dir not found: $SRC"; exit 1; }

# Default branch follows the sweep name → bench/<sweep>
BRANCH=${BRANCH:-bench/$SWEEP}

# Resolve owner/repo from the existing origin URL (works with both ssh + https)
ORIGIN=$(git remote get-url origin)
case "$ORIGIN" in
  git@github.com:*)
    REPO_PATH=${ORIGIN#git@github.com:}
    ;;
  https://*github.com/*)
    REPO_PATH=$(echo "$ORIGIN" | sed -E 's|https://[^@]*@?github.com/||')
    ;;
  *)
    echo "[push-report] unsupported origin URL: $ORIGIN"
    exit 1
    ;;
esac
REPO_PATH=${REPO_PATH%.git}

PUSH_URL="https://x-access-token:${TOKEN}@github.com/${REPO_PATH}.git"
SAFE_URL="https://x-access-token:***@github.com/${REPO_PATH}.git"

echo "[push-report] sweep:  $SWEEP"
echo "[push-report] branch: $BRANCH"
echo "[push-report] remote: $SAFE_URL"

# Worktree keeps us isolated from the current checkout
WORKTREE=$(mktemp -d -t bench-push-XXXXXX)
cleanup() {
  git worktree remove --force "$WORKTREE" >/dev/null 2>&1 || true
  rm -rf "$WORKTREE"
}
trap cleanup EXIT

# Base the worktree on the existing remote branch if it exists, else on HEAD
if git fetch --quiet "$PUSH_URL" "$BRANCH" 2>/dev/null; then
  echo "[push-report] basing on existing remote $BRANCH"
  git worktree add --quiet -B "$BRANCH" "$WORKTREE" FETCH_HEAD
else
  echo "[push-report] $BRANCH not on remote — creating from HEAD"
  git worktree add --quiet -B "$BRANCH" "$WORKTREE" HEAD
fi

# Copy reports into the worktree at the same path
mkdir -p "$WORKTREE/bench/reports/"
cp -a "$SRC" "$WORKTREE/bench/reports/"

cd "$WORKTREE"
git add "bench/reports/$SWEEP"

if git diff --cached --quiet; then
  echo "[push-report] no changes — branch already up to date for sweep $SWEEP"
  exit 0
fi

# Inline author so we don't mutate global git config
git -c user.email="bench@$(hostname)" -c user.name="bench-bot" \
  commit --quiet -m "bench: $SWEEP

Generated: $(date -Iseconds 2>/dev/null || date)
Host:      $(hostname)
Report:    bench/reports/$SWEEP/report.md"

echo "[push-report] pushing $BRANCH..."
git push --quiet "$PUSH_URL" "$BRANCH"

echo
echo "[push-report] ✓ done"
echo "  Branch:  https://github.com/${REPO_PATH}/tree/${BRANCH}"
echo "  Report:  https://github.com/${REPO_PATH}/blob/${BRANCH}/bench/reports/${SWEEP}/report.md"

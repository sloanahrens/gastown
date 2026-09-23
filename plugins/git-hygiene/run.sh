#!/usr/bin/env bash
# git-hygiene/run.sh — Clean stale branches, stashes, and loose objects.
#
# Runs across all rig repos. Covers:
# - Merged local branches
# - Orphan local branches (polecat/*, dog/*, fix/*, etc. with no remote)
# - Merged remote branches on GitHub
# - Stale stashes
# - Git garbage collection

set -euo pipefail

log() { echo "[git-hygiene] $*"; }

# --- Enumerate rig repos -----------------------------------------------------

# A broken or empty rig list is the exact condition this plugin guards
# against (gt-chqi): exiting 0 serializes as a success receipt on top of
# nothing. FAIL LOUD — nonzero exit, a failure record, a dog from the daemon.
RIG_JSON=$(gt rig list --json 2>/dev/null) || {
  log "FAIL: could not get rig list (gt rig list --json)"
  gt plugin record-run --plugin git-hygiene --result failure \
    --title "git-hygiene: FAILED (rig list unavailable)" \
    --description "gt rig list --json failed; run aborted (gt-chqi, gt-xxwx)" >/dev/null 2>&1 || true
  exit 1
}

# repo_path comes from Rig.RepoPath(): the first of the rig root,
# <rig>/mayor/rig, <rig>/refinery/rig that is a git worktree root.
RIG_PATHS=$(echo "$RIG_JSON" | python3 -c "
import json, sys
rigs = json.load(sys.stdin)
for r in rigs:
    p = r.get('repo_path') or ''
    if p: print(p)
" 2>/dev/null)

if [ -z "$RIG_PATHS" ]; then
  log "FAIL: no rigs with repo paths (gt rig list --json emitted no repo_path field — gt-chqi)"
  gt plugin record-run --plugin git-hygiene --result failure \
    --title "git-hygiene: FAILED (no rig repo paths)" \
    --description "gt rig list --json returned rigs without repo_path; run aborted (gt-chqi, gt-xxwx)" >/dev/null 2>&1 || true
  exit 1
fi

RIG_COUNT=$(echo "$RIG_PATHS" | wc -l | tr -d ' ')
log "Found $RIG_COUNT rig repo(s) to clean"

# --- Process each rig repo ----------------------------------------------------

TOTAL_LOCAL_MERGED=0
TOTAL_LOCAL_ORPHAN=0
TOTAL_REMOTE=0
TOTAL_STASHES=0
TOTAL_GC=0

while IFS= read -r REPO_PATH; do
  [ -z "$REPO_PATH" ] && continue

  if ! git -C "$REPO_PATH" rev-parse --git-dir >/dev/null 2>&1; then
    log "SKIP: $REPO_PATH is not a git repo"
    continue
  fi

  log ""
  log "=== Cleaning: $REPO_PATH ==="

  # Detect default branch. Survive a repo with no origin under set -e +
  # pipefail: the pipeline exits 128 on the missing ref.
  DEFAULT_BRANCH=$(git -C "$REPO_PATH" symbolic-ref refs/remotes/origin/HEAD 2>/dev/null \
    | sed 's|refs/remotes/origin/||' || true)
  if [ -z "$DEFAULT_BRANCH" ]; then
    DEFAULT_BRANCH="main"
  fi
  CURRENT_BRANCH=$(git -C "$REPO_PATH" branch --show-current 2>/dev/null)

  # Step 1: Prune remote tracking refs
  log "  Pruning remote tracking refs..."
  git -C "$REPO_PATH" fetch --prune --all 2>/dev/null || true

  # Step 2: Delete merged local branches
  log "  Deleting merged local branches..."
  MERGED_BRANCHES=$(git -C "$REPO_PATH" branch --merged "$DEFAULT_BRANCH" 2>/dev/null \
    | grep -v "^\*" \
    | grep -v "^+" \
    | grep -v -E "^\s*(main|master)$" \
    | sed 's/^[[:space:]]*//' || true)

  LOCAL_MERGED=0
  while IFS= read -r BRANCH; do
    [ -z "$BRANCH" ] && continue
    if [ "$BRANCH" = "$CURRENT_BRANCH" ] || [ "$BRANCH" = "$DEFAULT_BRANCH" ]; then
      continue
    fi
    case "$BRANCH" in
      refinery-patrol|merge/*) continue ;;
    esac
    log "    Deleting merged: $BRANCH"
    git -C "$REPO_PATH" branch -d "$BRANCH" 2>/dev/null && LOCAL_MERGED=$((LOCAL_MERGED + 1))
  done <<< "$MERGED_BRANCHES"
  TOTAL_LOCAL_MERGED=$((TOTAL_LOCAL_MERGED + LOCAL_MERGED))

  # Step 3: Delete stale unmerged orphan branches
  log "  Deleting stale orphan branches..."
  STALE_PATTERNS="polecat/|dog/|fix/|pr-|integration/|worktree-agent-"
  ALL_BRANCHES=$(git -C "$REPO_PATH" branch 2>/dev/null \
    | grep -v "^\*" \
    | grep -v "^+" \
    | sed 's/^[[:space:]]*//' || true)

  LOCAL_ORPHAN=0
  while IFS= read -r BRANCH; do
    [ -z "$BRANCH" ] && continue
    if ! echo "$BRANCH" | grep -qE "^($STALE_PATTERNS)"; then
      continue
    fi
    if [ "$BRANCH" = "$CURRENT_BRANCH" ] || [ "$BRANCH" = "$DEFAULT_BRANCH" ]; then
      continue
    fi
    case "$BRANCH" in
      main|master|refinery-patrol|merge/*) continue ;;
    esac
    if git -C "$REPO_PATH" rev-parse --verify "refs/remotes/origin/$BRANCH" >/dev/null 2>&1; then
      continue
    fi
    log "    Deleting orphan: $BRANCH"
    git -C "$REPO_PATH" branch -D "$BRANCH" 2>/dev/null && LOCAL_ORPHAN=$((LOCAL_ORPHAN + 1))
  done <<< "$ALL_BRANCHES"
  TOTAL_LOCAL_ORPHAN=$((TOTAL_LOCAL_ORPHAN + LOCAL_ORPHAN))

  # Step 4: Delete merged remote branches on GitHub
  log "  Deleting merged remote branches..."
  REMOTE_DELETED=0

  GH_REPO=$(git -C "$REPO_PATH" remote get-url origin 2>/dev/null \
    | sed -E 's|.*github\.com[:/]||; s|\.git$||' || true)

  if [ -n "$GH_REPO" ]; then
    REMOTE_BRANCHES=$(git -C "$REPO_PATH" branch -r 2>/dev/null \
      | grep -v HEAD \
      | grep -v "origin/$DEFAULT_BRANCH" \
      | grep -v "origin/dependabot/" \
      | grep -v "origin/refinery-patrol" \
      | grep -vE "origin/merge/" \
      | sed 's|^[[:space:]]*origin/||' || true)

    REMOTE_PATTERNS="polecat/|fix/|pr-|integration/|worktree-agent-"

    while IFS= read -r RBRANCH; do
      [ -z "$RBRANCH" ] && continue
      if ! echo "$RBRANCH" | grep -qE "^($REMOTE_PATTERNS)"; then
        continue
      fi
      if git -C "$REPO_PATH" merge-base --is-ancestor "origin/$RBRANCH" "origin/$DEFAULT_BRANCH" 2>/dev/null; then
        log "    Deleting remote: origin/$RBRANCH"
        gh api "repos/$GH_REPO/git/refs/heads/$RBRANCH" -X DELETE 2>/dev/null && REMOTE_DELETED=$((REMOTE_DELETED + 1))
      fi
    done <<< "$REMOTE_BRANCHES"
  fi
  TOTAL_REMOTE=$((TOTAL_REMOTE + REMOTE_DELETED))

  # Step 5: Clear stale stashes
  log "  Clearing stashes..."
  STASH_COUNT=$(git -C "$REPO_PATH" stash list 2>/dev/null | wc -l | tr -d ' ')
  if [ "$STASH_COUNT" -gt 0 ]; then
    log "    Clearing $STASH_COUNT stash(es)"
    git -C "$REPO_PATH" stash clear 2>/dev/null
    TOTAL_STASHES=$((TOTAL_STASHES + STASH_COUNT))
  fi

  # Step 6: Garbage collect
  log "  Running git gc..."
  git -C "$REPO_PATH" gc --prune=now --quiet 2>/dev/null && TOTAL_GC=$((TOTAL_GC + 1))

  log "  Done: $LOCAL_MERGED merged, $LOCAL_ORPHAN orphan, $REMOTE_DELETED remote, $STASH_COUNT stash(es)"
done <<< "$RIG_PATHS"

# --- Report -------------------------------------------------------------------

SUMMARY="$RIG_COUNT rig(s): $TOTAL_LOCAL_MERGED merged, $TOTAL_LOCAL_ORPHAN orphan, $TOTAL_REMOTE remote, $TOTAL_STASHES stash(es), $TOTAL_GC gc"
log ""
log "=== Git Hygiene Summary ==="
log "$SUMMARY"

# A run that deleted nothing and cleared nothing accomplished nothing: print
# the daemon skip marker (scriptSkippedMarker) so the receipt says skipped
# instead of success (gt-chqi). A real no-op must not green-check the history.
#
# TOTAL_GC is deliberately excluded from the condition: git gc --prune=now
# exits 0 on an already-clean repo (it succeeded, it just had nothing to
# prune), so counting it as "did work" would defeat the skip for every clean
# rig. The other four counters are the real work signals.
if [ "$TOTAL_LOCAL_MERGED" -eq 0 ] && [ "$TOTAL_LOCAL_ORPHAN" -eq 0 ] &&
   [ "$TOTAL_REMOTE" -eq 0 ] && [ "$TOTAL_STASHES" -eq 0 ]; then
  log "Nothing to do — all rig repos were already clean"
  echo "[plugin-result skipped]"
  gt plugin record-run --plugin git-hygiene --result skipped \
    --title "git-hygiene: $SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true
  exit 0
fi

gt plugin record-run --plugin git-hygiene --result success \
  --title "git-hygiene: $SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true

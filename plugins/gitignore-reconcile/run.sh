#!/usr/bin/env bash
# gitignore-reconcile/run.sh — Auto-untrack files matched by .gitignore
#
# Scans all rig repos for tracked files that now match an active .gitignore
# rule. On clean main branches: git rm --cached + commit. On dirty branches
# or active polecat worktrees: creates a chore bead instead.

set -euo pipefail

log() { echo "[gitignore-reconcile] $*"; }

# --- Step 1: Enumerate rig repos ---------------------------------------------

# A broken or empty rig list is the exact condition the plugin is meant to
# guard against (gt-chqi): exiting 0 here serializes as a success receipt and
# a 12-hour cooldown on top of nothing, so the failure is invisible. Exit
# nonzero instead — the daemon records it and dispatches a dog.
RIG_JSON=$(gt rig list --json 2>/dev/null) || {
  log "FAIL: could not get rig list (gt rig list --json)"
  gt plugin record-run --plugin gitignore-reconcile --result failure \
    --title "gitignore-reconcile: FAILED (rig list unavailable)" \
    --description "gt rig list --json failed; run aborted (gt-chqi)" >/dev/null 2>&1 || true
  exit 1
}

# repo_path comes from Rig.RepoPath(): the first of the rig root, <rig>/mayor/rig,
# <rig>/refinery/rig that is a git worktree root. An all-empty list means no rig
# has a resolvable repo — loud failure, same as above (gt-chqi, gt-xxwx).
RIG_ENTRIES=$(echo "$RIG_JSON" | jq -r '.[] | select(.repo_path != null and .repo_path != "") | [(.name // "unknown"), .repo_path] | @tsv' 2>/dev/null)
if [ -z "$RIG_ENTRIES" ]; then
  log "FAIL: no rigs with repo paths (gt rig list --json emitted no repo_path field — gt-chqi)"
  gt plugin record-run --plugin gitignore-reconcile --result failure \
    --title "gitignore-reconcile: FAILED (no rig repo paths)" \
    --description "gt rig list --json returned rigs without repo_path; run aborted (gt-chqi, gt-xxwx)" >/dev/null 2>&1 || true
  exit 1
fi

RIG_COUNT=$(echo "$RIG_ENTRIES" | wc -l | tr -d ' ')
log "Checking $RIG_COUNT rig repo(s) for tracked+ignored files"

# --- Step 2: Process each rig -----------------------------------------------

TOTAL_UNTRACKED=0
TOTAL_BEADS=0

while IFS=$'\t' read -r RIG_NAME REPO_PATH; do
  [ -z "$REPO_PATH" ] && continue

  if ! git -C "$REPO_PATH" rev-parse --git-dir >/dev/null 2>&1; then
    log "SKIP: $REPO_PATH — not a git repo"
    continue
  fi

  log ""
  log "=== $REPO_PATH ==="

  IGNORED_TRACKED=$(git -C "$REPO_PATH" ls-files --ignored --exclude-standard --cached 2>/dev/null || true)
  if [ -z "$IGNORED_TRACKED" ]; then
    log "  Clean"
    continue
  fi

  FILE_COUNT=$(echo "$IGNORED_TRACKED" | wc -l | tr -d ' ')
  log "  Found $FILE_COUNT tracked+ignored file(s)"

  CURRENT_BRANCH=$(git -C "$REPO_PATH" branch --show-current 2>/dev/null || true)
  IS_DIRTY=$(git -C "$REPO_PATH" status --porcelain 2>/dev/null | grep -v "^??" | head -1 || true)
  HAS_POLECATS=$(git -C "$REPO_PATH" branch 2>/dev/null | grep -E "^\+?\s+polecat/" | head -1 || true)

  if [ -n "$IS_DIRTY" ] || [ -n "$HAS_POLECATS" ] || [ "$CURRENT_BRANCH" != "main" ]; then
    REASON=""
    [ -n "$IS_DIRTY" ]       && REASON="dirty working tree"
    [ -n "$HAS_POLECATS" ]   && REASON="${REASON:+$REASON, }active polecat worktrees"
    [ "$CURRENT_BRANCH" != "main" ] && REASON="${REASON:+$REASON, }not on main ($CURRENT_BRANCH)"

    EXISTING_ID=$(bd list --status open -l "plugin:gitignore-reconcile" \
      --desc-contains "Repo: $REPO_PATH" --json 2>/dev/null \
      | jq -r 'if type == "array" then (.[0].id // empty) else empty end' 2>/dev/null || true)

    if [ -n "$EXISTING_ID" ]; then
      log "  SKIP: $REASON — updating existing bead $EXISTING_ID"
      bd comments add "$EXISTING_ID" \
        "$(printf "Still %s tracked+ignored file(s) as of %s.\nSkipped: %s" "$FILE_COUNT" "$(date -u +%Y-%m-%d)" "$REASON")" \
        >/dev/null 2>&1 || true
    else
      log "  SKIP: $REASON — creating chore bead"
      bd create "gitignore-reconcile: $RIG_NAME has $FILE_COUNT tracked+ignored file(s)" \
        -t chore \
        -l "plugin:gitignore-reconcile,category:git-hygiene" \
        -d "$(printf "Repo: %s\nSkipped: %s\nFiles:\n%s" "$REPO_PATH" "$REASON" "$(echo "$IGNORED_TRACKED" | head -20)")" \
        --silent 2>/dev/null || true
      TOTAL_BEADS=$((TOTAL_BEADS + 1))
    fi
    continue
  fi

  # Untrack files
  UNTRACKED_THIS=0
  while IFS= read -r FILE; do
    [ -z "$FILE" ] && continue
    log "  Untracking: $FILE"
    git -C "$REPO_PATH" rm --cached "$FILE" 2>/dev/null && UNTRACKED_THIS=$((UNTRACKED_THIS + 1)) || true
  done <<< "$IGNORED_TRACKED"

  STAGED=$(git -C "$REPO_PATH" diff --cached --name-only 2>/dev/null || true)
  if [ -n "$STAGED" ]; then
    COUNT=$(echo "$STAGED" | wc -l | tr -d ' ')
    COMMIT_MSG="chore: untrack $COUNT file(s) now matched by .gitignore

Auto-committed by gitignore-reconcile plugin."
    git -C "$REPO_PATH" commit -m "$COMMIT_MSG" \
      --author="Gas Town <gastown@local>" 2>/dev/null && \
      log "  Committed untracking of $COUNT file(s)" || \
      log "  WARN: commit failed"
    TOTAL_UNTRACKED=$((TOTAL_UNTRACKED + COUNT))
    git -C "$REPO_PATH" push origin main 2>/dev/null || log "  WARN: push failed (committed locally)"
  fi
done <<< "$RIG_ENTRIES"

# --- Report ------------------------------------------------------------------

log ""
log "=== Summary ==="
SUMMARY="gitignore-reconcile: $TOTAL_UNTRACKED file(s) untracked, $TOTAL_BEADS chore bead(s) created"
log "$SUMMARY"

# A run that untracked nothing and filed no bead accomplished nothing: print
# the daemon skip marker (scriptSkippedMarker) so the receipt says skipped
# instead of success (gt-chqi). A real no-op must not green-check the history.
if [ "$TOTAL_UNTRACKED" -eq 0 ] && [ "$TOTAL_BEADS" -eq 0 ]; then
  log "Nothing to do — no tracked+ignored files found in any rig repo"
  echo "[plugin-result skipped]"
  gt plugin record-run --plugin gitignore-reconcile --result skipped \
    --title "$SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true
  log "Done."
  exit 0
fi

gt plugin record-run --plugin gitignore-reconcile --result success \
  --title "$SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true

log "Done."

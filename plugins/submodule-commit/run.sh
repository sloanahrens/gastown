#!/usr/bin/env bash
# submodule-commit/run.sh — Auto-commit accumulated changes in git submodules.
#
# Reads .gitmodules in opt-in rigs, commits any accumulated changes in each
# submodule on a known branch, pushes with || true (local commit is priority),
# then updates the parent repo's submodule pointer on main.
#
# Opt-in per rig: set plugins.submodule-commit.enabled=true in rig config.

set -euo pipefail

log() { echo "[submodule-commit] $*"; }

# --- Step 1: Find opt-in rigs with submodules --------------------------------

# A broken or empty rig list is the exact condition this plugin guards
# against (gt-chqi): exiting 0 serializes as a success receipt on top of
# nothing. FAIL LOUD — nonzero exit, a failure record, a dog from the daemon.
RIG_JSON=$(gt rig list --json 2>/dev/null) || {
  log "FAIL: could not get rig list (gt rig list --json)"
  gt plugin record-run --plugin submodule-commit --result failure \
    --title "submodule-commit: FAILED (rig list unavailable)" \
    --description "gt rig list --json failed; run aborted (gt-chqi, gt-xxwx)" >/dev/null 2>&1 || true
  exit 1
}

# repo_path comes from Rig.RepoPath(): the first of the rig root,
# <rig>/mayor/rig, <rig>/refinery/rig that is a git worktree root. With no
# repo_path at all the opt-in check reads every rig as unenabled — the
# 2026-09-18 no-op (gt-xxwx). Detect it explicitly.
declare -a ENABLED_RIGS=()
declare -a RESOLVED=()
while IFS= read -r REPO_PATH; do
  [ -z "$REPO_PATH" ] && continue
  RESOLVED+=("$REPO_PATH")
  [ ! -f "$REPO_PATH/.gitmodules" ] && continue
  RIG_NAME=$(basename "$REPO_PATH")
  PLUGIN_ENABLED=$(gt rig show "$RIG_NAME" --json 2>/dev/null \
    | jq -r '.plugins["submodule-commit"].enabled // false' 2>/dev/null || echo "false")
  if [ "$PLUGIN_ENABLED" = "true" ]; then
    ENABLED_RIGS+=("$REPO_PATH")
    log "Opt-in rig: $REPO_PATH"
  fi
done < <(echo "$RIG_JSON" | jq -r '.[] | select(.repo_path != null and .repo_path != "") | .repo_path' 2>/dev/null)

if [ ${#RESOLVED[@]} -eq 0 ]; then
  log "FAIL: no rigs with repo paths (gt rig list --json emitted no repo_path field — gt-chqi)"
  gt plugin record-run --plugin submodule-commit --result failure \
    --title "submodule-commit: FAILED (no rig repo paths)" \
    --description "gt rig list --json returned rigs without repo_path; run aborted (gt-chqi, gt-xxwx)" >/dev/null 2>&1 || true
  exit 1
fi

if [ ${#ENABLED_RIGS[@]} -eq 0 ]; then
  log "SKIP: no opt-in rigs with submodules"
  echo "[plugin-result skipped]"
  gt plugin record-run --plugin submodule-commit --result skipped \
    --title "submodule-commit: no opt-in rigs" --description "gt rig list --json returned ${#RESOLVED[@]} rig(s) with repo paths; none opt in to submodule-commit" >/dev/null 2>&1 || true
  exit 0
fi

log "Processing ${#ENABLED_RIGS[@]} opt-in rig(s)"

# --- Step 2: Process each rig ------------------------------------------------

TOTAL_COMMITTED=0
TOTAL_PUSHED=0
TOTAL_PARENT_UPDATED=0

for REPO_PATH in "${ENABLED_RIGS[@]}"; do
  log ""
  log "=== $REPO_PATH ==="

  RIG_NAME=$(basename "$REPO_PATH")

  # Get plugin config
  RIG_CONFIG=$(gt rig show "$RIG_NAME" --json 2>/dev/null \
    | jq -r '.plugins["submodule-commit"] // {}' 2>/dev/null || echo "{}")
  PUSH_ENABLED=$(echo "$RIG_CONFIG" | jq -r '.push_enabled // false')
  ALLOWLIST=$(echo "$RIG_CONFIG" | jq -r '.allowlist // [] | .[]' 2>/dev/null || true)

  SUBMODULE_PATHS=$(git -C "$REPO_PATH" config --file .gitmodules --get-regexp 'submodule\..*\.path' 2>/dev/null \
    | awk '{print $2}' || true)

  if [ -z "$SUBMODULE_PATHS" ]; then
    log "  No submodules found in .gitmodules"
    continue
  fi

  PARENT_CHANGED=false

  while IFS= read -r SUB_PATH; do
    [ -z "$SUB_PATH" ] && continue

    # Apply allowlist filter
    if [ -n "$ALLOWLIST" ]; then
      MATCH=false
      while IFS= read -r ALLOWED; do
        [ "$SUB_PATH" = "$ALLOWED" ] && { MATCH=true; break; }
      done <<< "$ALLOWLIST"
      $MATCH || { log "  SKIP: $SUB_PATH — not in allowlist"; continue; }
    fi

    FULL_SUB="$REPO_PATH/$SUB_PATH"
    if [ ! -d "$FULL_SUB" ]; then
      log "  SKIP: $SUB_PATH — directory not found"
      continue
    fi
    if [ ! -d "$FULL_SUB/.git" ] && [ ! -f "$FULL_SUB/.git" ]; then
      log "  SKIP: $SUB_PATH — not initialized (run git submodule update --init)"
      continue
    fi

    SUB_DIRTY=$(git -C "$FULL_SUB" status --porcelain 2>/dev/null | head -1 || true)
    if [ -z "$SUB_DIRTY" ]; then
      log "  $SUB_PATH: clean"
      continue
    fi

    SUB_BRANCH=$(git -C "$FULL_SUB" branch --show-current 2>/dev/null || true)
    if [ -z "$SUB_BRANCH" ]; then
      log "  SKIP: $SUB_PATH — detached HEAD"
      continue
    fi

    log "  $SUB_PATH: has changes (branch=$SUB_BRANCH)"

    git -C "$FULL_SUB" add -A 2>/dev/null || true
    STAGED_COUNT=$(git -C "$FULL_SUB" diff --cached --name-only 2>/dev/null | wc -l | tr -d ' ')

    if [ "$STAGED_COUNT" -gt 0 ]; then
      git -C "$FULL_SUB" commit \
        -m "chore: accumulated changes [skip ci]

Auto-committed by submodule-commit plugin ($STAGED_COUNT file(s))." \
        --author="Gas Town <gastown@local>" 2>/dev/null && {
          log "    Committed $STAGED_COUNT file(s)"
          TOTAL_COMMITTED=$((TOTAL_COMMITTED + 1))
          PARENT_CHANGED=true
        } || { log "    WARN: commit failed"; continue; }

      if [ "$PUSH_ENABLED" = "true" ]; then
        git -C "$FULL_SUB" push origin "$SUB_BRANCH" 2>/dev/null && \
          { log "    Pushed to origin/$SUB_BRANCH"; TOTAL_PUSHED=$((TOTAL_PUSHED + 1)); } || \
          log "    WARN: push failed (local commit preserved)"
      fi
    fi
  done <<< "$SUBMODULE_PATHS"

  # Update parent pointer on main if submodules changed
  if $PARENT_CHANGED; then
    PARENT_BRANCH=$(git -C "$REPO_PATH" branch --show-current 2>/dev/null || true)
    if [ "$PARENT_BRANCH" != "main" ]; then
      log "  SKIP parent pointer update: on $PARENT_BRANCH (not main)"
      continue
    fi

    PARENT_UNSTAGED=$(git -C "$REPO_PATH" status --porcelain 2>/dev/null | grep -v "^??" | head -1 || true)
    if [ -n "$PARENT_UNSTAGED" ]; then
      log "  SKIP parent pointer update: parent has other uncommitted changes"
      continue
    fi

    # Stage only submodule pointer changes
    git -C "$REPO_PATH" add -u 2>/dev/null || true
    PARENT_STAGED=$(git -C "$REPO_PATH" diff --cached --name-only 2>/dev/null || true)
    if [ -n "$PARENT_STAGED" ]; then
      git -C "$REPO_PATH" commit \
        -m "chore: update submodule pointers [skip ci]

Auto-committed by submodule-commit plugin." \
        --author="Gas Town <gastown@local>" 2>/dev/null && {
          log "  Parent pointer updated"
          TOTAL_PARENT_UPDATED=$((TOTAL_PARENT_UPDATED + 1))
        } || log "  WARN: parent pointer commit failed"
      git -C "$REPO_PATH" push origin main 2>/dev/null || log "  WARN: parent push failed (local commit preserved)"
    fi
  fi
done

# --- Report ------------------------------------------------------------------

log ""
log "=== Summary ==="
SUMMARY="submodule-commit: $TOTAL_COMMITTED submodule(s) committed, $TOTAL_PUSHED pushed, $TOTAL_PARENT_UPDATED parent pointer(s) updated"
log "$SUMMARY"

# A run that committed nothing accomplished nothing: print the daemon skip
# marker (scriptSkippedMarker) so the receipt says skipped instead of success
# (gt-chqi). A real no-op must not green-check the history.
if [ "$TOTAL_COMMITTED" -eq 0 ] && [ "$TOTAL_PARENT_UPDATED" -eq 0 ]; then
  log "Nothing to do — all submodules were clean"
  echo "[plugin-result skipped]"
  gt plugin record-run --plugin submodule-commit --result skipped \
    --title "$SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true
  log "Done."
  exit 0
fi

gt plugin record-run --plugin submodule-commit --result success \
  --title "$SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true

log "Done."

#!/usr/bin/env bash
# rebuild-gt/run.sh — Rebuild gt binary from gastown source if stale.
#
# SAFETY: Only rebuilds forward (binary is ancestor of HEAD) and only
# from main branch. A bad rebuild caused a crash loop (every session's
# startup hook failed, witness respawned, loop repeated every 1-2 min).

set -euo pipefail

TOWN_ROOT="${GT_TOWN_ROOT:-$(gt town root 2>/dev/null)}"
RIG_ROOT="${TOWN_ROOT}/gastown/mayor/rig"

log() { echo "[rebuild-gt] $*"; }

# --- Detection ---------------------------------------------------------------

log "Checking binary staleness..."
STALE_JSON=$(gt stale --json 2>/dev/null) || {
  log "gt stale --json failed, skipping"
  exit 0
}

IS_STALE=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('stale', False))" 2>/dev/null || echo "False")
SAFE=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('safe_to_rebuild', False))" 2>/dev/null || echo "False")

if [ "$IS_STALE" != "True" ]; then
  log "Binary is fresh. Nothing to do."
  gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
    --title "rebuild-gt: binary is fresh" >/dev/null 2>&1 || true
  exit 0
fi

if [ "$SAFE" != "True" ]; then
  log "Not safe to rebuild (not on main or would be a downgrade). Skipping."
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: not safe to rebuild" >/dev/null 2>&1 || true
  exit 0
fi

# --- Pre-flight checks -------------------------------------------------------

log "Pre-flight checks..."

if [ ! -d "$RIG_ROOT" ]; then
  log "Rig root $RIG_ROOT does not exist. Skipping."
  exit 0
fi

# Only TRACKED modifications outside .beads/ can change what 'make build'
# produces. Untracked entries (.agents/, .codex/, .worktrees/) and bd's own
# rewriting of .beads/config.yaml must not trip this guard: a plain
# --porcelain check skipped every rebuild for hours while exiting 0 (gt-50k).
DIRTY=$(git -C "$RIG_ROOT" status --porcelain --untracked-files=no -- . ':(exclude).beads' 2>/dev/null)
if [ -n "$DIRTY" ]; then
  log "Repo is dirty, skipping rebuild."
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: repo has uncommitted changes" >/dev/null 2>&1 || true
  exit 0
fi

BRANCH=$(git -C "$RIG_ROOT" branch --show-current 2>/dev/null)
if [ "$BRANCH" != "main" ]; then
  log "Not on main branch (on $BRANCH), skipping rebuild."
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: not on main branch (on $BRANCH)" >/dev/null 2>&1 || true
  exit 0
fi

# RIG_ROOT has no self-serve pull otherwise: without this, the build uses
# whatever commit a human last checked out, and 'make safe-install' fails its
# check-up-to-date gate against origin/main on every run until a human pulls
# (gt-4g1m). ff-only only: a real divergence must never be reset --hard away.
log "Syncing $RIG_ROOT with origin/main..."
git -C "$RIG_ROOT" fetch origin --quiet 2>/dev/null || true
if ! git -C "$RIG_ROOT" merge --ff-only origin/main --quiet 2>/dev/null; then
  log "Local main diverged from origin/main, skipping rebuild."
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: local main diverged from origin/main" >/dev/null 2>&1 || true
  exit 0
fi

# --- Build -------------------------------------------------------------------

OLD_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
log "Rebuilding gt from $RIG_ROOT..."

if (cd "$RIG_ROOT" && make build && make safe-install) 2>&1; then
  NEW_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
  log "Rebuilt: $OLD_VER -> $NEW_VER"
  gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
    --title "rebuild-gt: $OLD_VER -> $NEW_VER" >/dev/null 2>&1 || true
else
  ERROR="make build/safe-install failed"
  log "FAILED: $ERROR"
  gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
    --title "Plugin: rebuild-gt [failure]" \
    --description "Build failed: $ERROR" >/dev/null 2>&1 || true
  gt escalate "Plugin FAILED: rebuild-gt" -s medium 2>/dev/null || true
  exit 1
fi

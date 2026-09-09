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

# --- Pre-flight checks -------------------------------------------------------

log "Pre-flight checks..."

if [ ! -d "$RIG_ROOT" ]; then
  log "Rig root $RIG_ROOT does not exist. Skipping."
  exit 0
fi

# --- Drift escalation ---------------------------------------------------------
#
# Every bail below (dirty repo, wrong branch, diverged local main, an
# unreadable staleness check, "not safe to rebuild") exits 0 and records a
# healthy-looking "skipped" receipt. None of them escalate, so any one of
# them can persist for hours — or indefinitely — with nothing in the system
# ever raising an alarm (gt-bce). This check runs unconditionally, before any
# of those bails, and is keyed to the OUTCOME THAT MATTERS — the binary
# drifting from origin/main — rather than to an enumeration of bail reasons.
# Enumerating failure modes is the same brittleness this town has hit
# repeatedly (gt-r8o, gt-50k); this catches every bail path above, including
# ones not yet written, because it doesn't ask why the rebuild didn't happen.
#
# 'gt stale --json' refreshes its own origin/main remote-tracking ref and
# only inspects git history, never RIG_ROOT's working-tree state, so it is
# meaningful here even though the checks below may bail on that same
# worktree being dirty or on the wrong branch.
MAX_COMMITS_BEHIND=${REBUILD_GT_MAX_COMMITS_BEHIND:-20}
if DRIFT_JSON=$(gt stale --json 2>/dev/null); then
  DRIFT_BEHIND=$(echo "$DRIFT_JSON" | python3 -c "import json,sys; print(int(json.load(sys.stdin).get('commits_behind') or 0))" 2>/dev/null || echo 0)
  if [ "$DRIFT_BEHIND" -gt "$MAX_COMMITS_BEHIND" ]; then
    log "Binary is $DRIFT_BEHIND commits behind origin/main (over threshold $MAX_COMMITS_BEHIND). Escalating."
    gt escalate "rebuild-gt: binary is $DRIFT_BEHIND commits behind origin/main and has not been rebuilt" \
      -s medium \
      --source "plugin:rebuild-gt" \
      --fingerprint "rebuild-gt:drift" 2>/dev/null || true
  fi
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
#
# This MUST run before the staleness check below (gt-h8s8): 'gt stale'
# compares the binary against RIG_ROOT's local main ref. If RIG_ROOT's own
# checkout is what's stale — and the binary was last built from that same
# stale local tip — checking staleness first lets it read as "fresh" and the
# fetch/pull never runs. Syncing first means the staleness check that
# follows sees a caught-up local main.
log "Syncing $RIG_ROOT with origin/main..."
git -C "$RIG_ROOT" fetch origin --quiet 2>/dev/null || true
if ! git -C "$RIG_ROOT" merge --ff-only origin/main --quiet 2>/dev/null; then
  log "Local main diverged from origin/main, skipping rebuild."
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: local main diverged from origin/main" >/dev/null 2>&1 || true
  exit 0
fi

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

# --- Build -------------------------------------------------------------------

OLD_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
log "Rebuilding gt from $RIG_ROOT..."

if (cd "$RIG_ROOT" && make build && make safe-install) 2>&1; then
  NEW_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
  log "Rebuilt: $OLD_VER -> $NEW_VER"

  # A binary install only carries new formula content — nothing copies it
  # out to $GT_ROOT/.beads/formulas/ on its own (gt-n6c). Sync delivers it.
  # Non-fatal: formulas are convenience content, not required for the binary
  # to work, so a sync failure must not fail the whole rebuild.
  if SYNC_OUT=$(gt formula sync 2>&1); then
    log "$SYNC_OUT"
  else
    log "formula sync failed (non-fatal): $SYNC_OUT"
  fi

  # Same problem, one directory over: $TOWN_ROOT/plugins is a deployed copy
  # of $RIG_ROOT/plugins, and nothing else keeps it current after a merge
  # (gt-reek). Non-fatal for the same reason as formula sync above.
  if PLUGIN_SYNC_OUT=$(gt plugin sync 2>&1); then
    log "$PLUGIN_SYNC_OUT"
  else
    log "plugin sync failed (non-fatal): $PLUGIN_SYNC_OUT"
  fi

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

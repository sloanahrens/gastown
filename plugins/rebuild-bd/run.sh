#!/usr/bin/env bash
# rebuild-bd/run.sh — Rebuild the town-wide bd binary from the beads fork
# source if stale, then prove it works before leaving it in place.
#
# SAFETY: bd is the DATA PLANE for every agent in the town. A bad gt binary
# already caused a crash loop (rebuild-gt/plugin.md); a bad bd binary is
# strictly worse, because every agent loses its issue tracker at once and
# the recovery agents are impaired too. This script is therefore MORE
# conservative than rebuild-gt/run.sh:
#   - forward-only / main-only / dirty-guard / ff-only-sync rails, reused
#     verbatim in spirit from rebuild-gt
#   - the ancestor check itself is NOT reimplemented here — it is delegated
#     to the beads Makefile's check-forward-only/safe-install targets
#   - a post-install smoke test with automatic rollback, which rebuild-gt
#     has no equivalent of

set -euo pipefail

TOWN_ROOT="${GT_TOWN_ROOT:-$HOME/gt}"
RIG_ROOT="${TOWN_ROOT}/beads/mayor/rig"
INSTALL_DIR="$HOME/.local/bin"
MAX_COMMITS_BEHIND="${REBUILD_BD_MAX_COMMITS_BEHIND:-20}"

log() { echo "[rebuild-bd] $*"; }

record() {
  # record <result> <title> [description]
  local args=(--plugin rebuild-bd --result "$1" --rig beads --title "$2")
  if [ -n "${3:-}" ]; then
    args+=(--description "$3")
  fi
  gt plugin record-run "${args[@]}" >/dev/null 2>&1 || true
}

# --- Pre-flight --------------------------------------------------------------

log "Pre-flight checks..."

if [ ! -d "$RIG_ROOT" ]; then
  log "Rig root $RIG_ROOT does not exist. Skipping."
  exit 0
fi

# --- Drift escalation ---------------------------------------------------------
#
# Every bail below (dirty repo, wrong branch, diverged local main, forward
# check refusing) is a normal, expected outcome on its own — but any one of
# them can persist indefinitely with nothing raising an alarm (the same class
# of gap as gt-bce for rebuild-gt). There is no bd-equivalent of `gt stale
# --json`, so drift is computed directly against origin/main here, keyed to
# the outcome that matters (commits behind), before any bail path can fire.
log "Checking drift against origin/main..."
git -C "$RIG_ROOT" fetch origin main --quiet 2>/dev/null || true
DRIFT_BEHIND=$(git -C "$RIG_ROOT" rev-list --count HEAD..origin/main 2>/dev/null || echo 0)
if [ "$DRIFT_BEHIND" -gt "$MAX_COMMITS_BEHIND" ]; then
  log "Binary source is $DRIFT_BEHIND commits behind origin/main (over threshold $MAX_COMMITS_BEHIND). Escalating."
  gt escalate "rebuild-bd: binary is $DRIFT_BEHIND commits behind origin/main and has not been rebuilt" \
    -s medium \
    --source "plugin:rebuild-bd" \
    --fingerprint "rebuild-bd:drift" 2>/dev/null || true
fi

# Only TRACKED modifications outside .beads/ can change what 'make build'
# produces (mirrors rebuild-gt's guard / gt-50k).
DIRTY=$(git -C "$RIG_ROOT" status --porcelain --untracked-files=no -- . ':(exclude).beads' 2>/dev/null)
if [ -n "$DIRTY" ]; then
  log "Repo is dirty, skipping rebuild."
  record skipped "Plugin: rebuild-bd [skipped]" "Skipped: repo has uncommitted changes"
  exit 0
fi

BRANCH=$(git -C "$RIG_ROOT" branch --show-current 2>/dev/null)
if [ "$BRANCH" != "main" ]; then
  log "Not on main branch (on $BRANCH), skipping rebuild."
  record skipped "Plugin: rebuild-bd [skipped]" "Skipped: not on main branch (on $BRANCH)"
  exit 0
fi

# --- Sync with origin/main ----------------------------------------------------
#
# ff-only only: a real divergence must never be reset --hard away.
log "Syncing $RIG_ROOT with origin/main..."
if ! git -C "$RIG_ROOT" merge --ff-only origin/main --quiet 2>/dev/null; then
  log "Local main diverged from origin/main, skipping rebuild."
  record skipped "Plugin: rebuild-bd [skipped]" "Skipped: local main diverged from origin/main"
  exit 0
fi

# --- Detection (delegated to the Makefile's forward-only check) --------------
#
# check-forward-only (added by be-suy) already does exactly what gt stale's
# safe_to_rebuild field does for gt. Do not reimplement the ancestor
# comparison here.
log "Checking bd binary staleness..."
FWD_RC=0
FWD_OUT=$(cd "$RIG_ROOT" && make check-forward-only 2>&1) || FWD_RC=$?

if [ $FWD_RC -ne 0 ]; then
  if echo "$FWD_OUT" | grep -q "already at HEAD"; then
    log "Binary is fresh. Nothing to do."
    record success "rebuild-bd: binary is fresh"
    exit 0
  fi
  log "Not safe to rebuild:"
  log "$FWD_OUT"
  record skipped "Plugin: rebuild-bd [skipped]" "Skipped: not safe to rebuild ($FWD_OUT)"
  exit 0
fi

OLD_VER=$("$INSTALL_DIR/bd" version 2>/dev/null || bd version 2>/dev/null || echo "unknown")

# --- Backup before overwrite --------------------------------------------------
#
# Rollback must never depend on rebuilding from source or on Homebrew still
# being reachable. Keep exactly one rollback copy — the immediately-prior
# binary — so this doesn't accumulate unbounded cruft across cycles.
BACKUP=""
if [ -f "$INSTALL_DIR/bd" ]; then
  rm -f "$INSTALL_DIR"/bd.prev-* 2>/dev/null || true
  BACKUP="$INSTALL_DIR/bd.prev-$(date +%Y%m%d%H%M%S)"
  cp -p "$INSTALL_DIR/bd" "$BACKUP"
  log "Backed up outgoing binary to $BACKUP"
else
  log "No existing $INSTALL_DIR/bd — first fork install, nothing to back up."
fi

# --- Action --------------------------------------------------------------------

log "Rebuilding bd from $RIG_ROOT..."
if ! (cd "$RIG_ROOT" && make safe-install) 2>&1; then
  ERROR="make safe-install failed"
  log "FAILED: $ERROR"
  record failure "Plugin: rebuild-bd [failure]" "Build/install failed: $ERROR"
  gt escalate "rebuild-bd: $ERROR" -s high --source "plugin:rebuild-bd" 2>/dev/null || true
  exit 1
fi

# --- Smoke test (REQUIRED) ----------------------------------------------------
#
# This is what makes rebuild-bd safe to run unattended. A binary that starts
# but can't talk to Dolt is exactly the failure mode this plugin exists to
# catch before it reaches the rest of the town.
log "Running smoke test..."
SMOKE_OK=true
SMOKE_REASON=""
if ! "$INSTALL_DIR/bd" version >/dev/null 2>&1; then
  SMOKE_OK=false
  SMOKE_REASON="bd version failed to exit 0"
elif ! (cd "$RIG_ROOT" && "$INSTALL_DIR/bd" list --limit 1 >/dev/null 2>&1); then
  SMOKE_OK=false
  SMOKE_REASON="bd list against live rig DB ($RIG_ROOT) failed"
fi

if [ "$SMOKE_OK" != "true" ]; then
  log "SMOKE TEST FAILED: $SMOKE_REASON"
  if [ -n "$BACKUP" ] && [ -f "$BACKUP" ]; then
    cp -p "$BACKUP" "$INSTALL_DIR/.bd.rollback.tmp.$$" && mv -f "$INSTALL_DIR/.bd.rollback.tmp.$$" "$INSTALL_DIR/bd"
    ROLLBACK_DESC="restored previous binary from $BACKUP"
  else
    rm -f "$INSTALL_DIR/bd" "$INSTALL_DIR/beads"
    ROLLBACK_DESC="removed broken first-install binary (Homebrew bd now unshadowed)"
  fi
  log "$ROLLBACK_DESC"

  if bd version >/dev/null 2>&1; then
    log "Rollback verified: bd is working again."
    record failure "Plugin: rebuild-bd [smoke test failed, rolled back]" \
      "Smoke test failed ($SMOKE_REASON). $ROLLBACK_DESC. Rollback verified working."
    gt escalate "rebuild-bd: smoke test failed after install, rolled back ($SMOKE_REASON)" \
      -s high --source "plugin:rebuild-bd" 2>/dev/null || true
  else
    log "CRITICAL: rollback verification failed — bd is still broken after rollback!"
    record failure "Plugin: rebuild-bd [CRITICAL: rollback failed]" \
      "Smoke test failed ($SMOKE_REASON). $ROLLBACK_DESC. ROLLBACK VERIFICATION ALSO FAILED — bd is broken town-wide."
    gt escalate "rebuild-bd: CRITICAL — smoke test failed AND rollback verification failed, bd is broken town-wide" \
      -s critical --source "plugin:rebuild-bd" 2>/dev/null || true
  fi
  exit 1
fi

NEW_VER=$("$INSTALL_DIR/bd" version 2>/dev/null || echo "unknown")
log "Rebuilt and verified: $OLD_VER -> $NEW_VER"
record success "Plugin: rebuild-bd [success]" \
  "Rebuilt bd: $OLD_VER -> $NEW_VER (smoke test passed; previous binary at ${BACKUP:-none, first install})"

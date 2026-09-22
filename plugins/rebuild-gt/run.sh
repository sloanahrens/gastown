#!/usr/bin/env bash
# rebuild-gt/run.sh — Rebuild gt binary from gastown source if stale.
#
# SAFETY: Only rebuilds forward (binary is ancestor of HEAD) and only
# from main branch. A bad rebuild caused a crash loop (every session's
# startup hook failed, witness respawned, loop repeated every 1-2 min).
#
# Merged is not in force until something installs: the daemon and every
# session it spawns keep executing the binary at their install path, so a
# stale binary leaves merged fixes inert for as long as detection goes
# unrepaired (gt-oqbw). So this plugin installs, then verifies what came into
# force.
#
# Exit codes are this script's contract with the daemon: 0 did the work (a run
# record is written), 3 DEFERRED — nothing accomplished, so deliberately no
# run record, because a record satisfies the cooldown gate and a run that
# accomplished nothing must not buy an hour before the retry; 1 refused or
# failed (recorded and escalated). Before changing that contract, read
# plugins/rebuild-gt/plugin.md.

set -euo pipefail

DEFERRED=3

TOWN_ROOT="${GT_TOWN_ROOT:-$(gt town root 2>/dev/null)}"
RIG_ROOT="${TOWN_ROOT}/gastown/mayor/rig"

log() { echo "[rebuild-gt] $*"; }

# defer MSG — nothing accomplished, retry on the next heartbeat.
defer() {
  log "Deferred: $*"
  exit "$DEFERRED"
}

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
  DRIFT_STALE=$(echo "$DRIFT_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('stale', False))" 2>/dev/null || echo "False")
  # commits_behind is only meaningful when present: 'or 0' would read a
  # missing/null count the same as "0 behind" and silently retire the alarm
  # this check exists for. An unknown count while the binary IS stale means
  # the drift could be 1 commit or 1000 — this cannot be ruled safe, so it
  # escalates too (gt-oqbw MAJOR #3: "unknown commits_behind parks the
  # install forever with no alarm").
  DRIFT_BEHIND=$(echo "$DRIFT_JSON" | python3 -c "
import json, sys
v = json.load(sys.stdin).get('commits_behind')
print(v if v is not None else 'unknown')
" 2>/dev/null || echo "unknown")
  if [ "$DRIFT_BEHIND" = "unknown" ]; then
    if [ "$DRIFT_STALE" = "True" ]; then
      log "Binary is stale but commits_behind is unknown (cannot rule out being over threshold $MAX_COMMITS_BEHIND). Escalating."
      gt escalate "rebuild-gt: binary is stale but commits_behind could not be determined" \
        -s medium \
        --source "plugin:rebuild-gt" \
        --fingerprint "rebuild-gt:drift-unknown" 2>/dev/null || true
    fi
  elif [ "$DRIFT_BEHIND" -gt "$MAX_COMMITS_BEHIND" ]; then
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
  # Deferred, not recorded: an unreadable staleness check accomplished
  # nothing, and a receipt would hold the retry off for a whole cooldown.
  defer "gt stale --json failed"
}

IS_STALE=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('stale', False))" 2>/dev/null || echo "False")
SAFE=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('safe_to_rebuild', False))" 2>/dev/null || echo "False")
# Same 'unknown is not 0' distinction as the drift check above.
BEHIND=$(echo "$STALE_JSON" | python3 -c "
import json, sys
v = json.load(sys.stdin).get('commits_behind')
print(v if v is not None else 'unknown')
" 2>/dev/null || echo "unknown")
# repo_commit is deliberately not read here: it would only be used to verify
# what came into force after the build, and that comparison needs EXPECTED_COMMIT
# instead (read fresh from RIG_ROOT right before the build, below) — this
# field can move past what was actually built (gt-oqbw MAJOR #2).
BINARY_COMMIT=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('binary_commit') or '')" 2>/dev/null || echo "")

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

# A merged commit that is not in force is a live defect, not a rounding error:
# gt-ww20 (a batch path that bypassed the editorial gate, so 4 MRs landed
# unreviewed) and gt-rbfj (composer-stall recovery typing the literal text
# 'C-x C-s' into the composer, the cause of both refinery stalls that day)
# were each merged while the town kept running a binary without them
# (gt-oqbw). So the install does not wait for a large delta: past this many
# commits, install at the first quiet moment. Raise it to trade inert fixes
# for fewer daemon restarts.
THRESHOLD=${REBUILD_GT_INSTALL_THRESHOLD:-5}
# An unknown count must not read as "0 behind, safely under threshold" —
# that reading is exactly what let a stale, quiet, safe-to-rebuild binary
# sit deferred every heartbeat with the install threshold never satisfied
# and no alarm anywhere (gt-oqbw MAJOR #3). The drift check above already
# escalates this state; here, since we're already stale + safe + quiet,
# proceed with the install rather than withhold it pending a count that
# 'gt stale' could not produce.
if [ "$BEHIND" = "unknown" ]; then
  log "commits_behind is unknown; proceeding with install rather than parking it on an unmeasurable threshold"
elif [ "$BEHIND" -lt "$THRESHOLD" ]; then
  defer "binary is $BEHIND behind (under the install threshold $THRESHOLD)"
fi

# --- Build -------------------------------------------------------------------

# Yield to a running gate (gt-htx3): make build competes for CPU with a gate
# suite whose tests are load-sensitive, so a rebuild while a refinery, batch,
# main-branch-test or om-review role holds a container-gate slot is deferred to
# the next cooldown. A stub or missing gt slot status reads as "free": the
# guard fails open on purpose so a broken status command cannot park rebuilds
# forever (the drift escalation above still fires if that happens).
#
# The town must also have nothing in flight (gt-oqbw): an MR a refinery is
# mid-merge on is work this restart would interrupt. An MR merely ready in the
# queue is NOT a reason to wait — it consumes nothing, and at this town's
# merge rate the queue is never empty, so requiring that would leave the
# install waiting forever. An unreadable Docker status is not a reason either:
# it means the cross-check could not tell, and a VM that is down runs no suite
# for a build to compete with.
GATE_BUSY=$(gt slot status --json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
held = []
for s in d.get("slots") or []:
    role = ((s.get("owner") or {}).get("role") or "")
    if role.endswith(("/refinery", "/refinery-batch", "/main-branch-test", "/om-review")):
        held.append(role)
if held:
    print("a gate suite holds a slot (%s)" % ", ".join(held))
elif d.get("unwrapped_containers"):
    print("container(s) are running outside the gate: %s" % ", ".join(d["unwrapped_containers"]))
elif d.get("saturated"):
    print("every container-gate slot is held")
' 2>/dev/null || true)
if [ -n "$GATE_BUSY" ]; then
  defer "not quiet: $GATE_BUSY"
fi

IN_FLIGHT=$(gt mq list gastown --status=in_progress --json 2>/dev/null | python3 -c '
import json, sys
try:
    print(len(json.load(sys.stdin)))
except Exception:
    print(0)
' 2>/dev/null | tail -1)
case "${IN_FLIGHT:-}" in
  ''|*[!0-9]*) IN_FLIGHT=0 ;;
esac
if [ "$IN_FLIGHT" -gt 0 ]; then
  defer "not quiet: $IN_FLIGHT merge(s) in flight in gastown"
fi

log "Rebuilding gt from $RIG_ROOT ($BEHIND commits behind)..."

# What was actually built is RIG_ROOT's HEAD at build time — not the
# repo_commit read earlier from 'gt stale --json'. That call does its own
# independent fetch (CheckStaleBinaryFresh) and can therefore report a
# repo_commit that has already moved past what this checkout's HEAD (and thus
# this build) contains (gt-oqbw MAJOR #2). Reading it fresh from RIG_ROOT
# immediately before the build ties the expectation to the tree actually
# compiled.
EXPECTED_COMMIT=$(git -C "$RIG_ROOT" rev-parse HEAD)

if (cd "$RIG_ROOT" && make build && make safe-install) 2>&1; then
  # What came into force is read from the gt the town will execute — resolved
  # through PATH, which is how the daemon and every session resolve it — not
  # from the build's stdout. An install that does not take (a shadowing gt
  # earlier in PATH, an INSTALL_DIR that is not the one assumed, a replacement
  # that silently went elsewhere) leaves every log line saying "installed"
  # while the town runs old code, and recording the wrong commit is worse than
  # recording none (gt-oqbw).
  GT_PATH=$(command -v gt 2>/dev/null || true)
  GOT_COMMIT_SHORT=$(gt version 2>/dev/null | grep -o '@[a-f0-9]*' | head -1 | tr -d '@' || true)
  if [ -z "$GOT_COMMIT_SHORT" ]; then
    log "FAILED: cannot read the installed binary's commit ($GT_PATH)"
    gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
      --title "Plugin: rebuild-gt [unverified]" \
      --description "Installed but the commit in force could not be read from $GT_PATH" >/dev/null 2>&1 || true
    gt escalate "rebuild-gt: installed a build but cannot verify what came into force" -s medium \
      --source "plugin:rebuild-gt" --fingerprint "rebuild-gt:unverified" 2>/dev/null || true
    exit 1
  fi
  # CRITICAL FIX (gt-oqbw): 'gt version' prints the binary's build-time commit
  # ABBREVIATED (internal/version.ShortCommit, <=12 chars — the Makefile
  # embeds it via 'git rev-parse --short HEAD'). EXPECTED_COMMIT above, like
  # 'gt stale --json's repo_commit, is a full 40-character hash. Comparing
  # those two strings directly never matches — a short string and a full one
  # are never string-equal — so every real install was being recorded as a
  # failure while the shell test's fixture hid it by using --short for both
  # sides. Resolve the abbreviated form to a full hash inside RIG_ROOT (the
  # one repo it's guaranteed unambiguous in) before comparing like with like.
  GOT_COMMIT=$(git -C "$RIG_ROOT" rev-parse --verify --quiet "$GOT_COMMIT_SHORT" 2>/dev/null || true)
  if [ -z "$GOT_COMMIT" ]; then
    log "FAILED: installed commit $GOT_COMMIT_SHORT does not resolve inside $RIG_ROOT"
    gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
      --title "Plugin: rebuild-gt [unverified]" \
      --description "Installed but $GOT_COMMIT_SHORT (from $GT_PATH) does not resolve to a commit in $RIG_ROOT" >/dev/null 2>&1 || true
    gt escalate "rebuild-gt: installed a build but cannot verify what came into force" -s medium \
      --source "plugin:rebuild-gt" --fingerprint "rebuild-gt:unverified" 2>/dev/null || true
    exit 1
  fi
  if [ "$GOT_COMMIT" != "$EXPECTED_COMMIT" ]; then
    log "FAILED: in force is $GOT_COMMIT, built $EXPECTED_COMMIT"
    gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
      --title "Plugin: rebuild-gt [install did not take]" \
      --description "Built $EXPECTED_COMMIT but the gt on PATH ($GT_PATH) reports $GOT_COMMIT" >/dev/null 2>&1 || true
    gt escalate "rebuild-gt: the install did not take — gt on PATH reports $GOT_COMMIT, built $EXPECTED_COMMIT" \
      -s medium --source "plugin:rebuild-gt" --fingerprint "rebuild-gt:not-in-force" 2>/dev/null || true
    exit 1
  fi
  log "Rebuilt: $BINARY_COMMIT -> $GOT_COMMIT"

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

  # The commits in this range were merged and not running until now: that
  # list is the inert window, recorded so "was gt-ww20 ever in force, and
  # when" is answered by the receipt instead of reconstructed from merge
  # timestamps.
  SUBJECTS=$(git -C "$RIG_ROOT" log --no-decorate --oneline "$BINARY_COMMIT..$GOT_COMMIT" 2>/dev/null | head -20 || true)
  gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
    --title "rebuild-gt: in force $BINARY_COMMIT -> $GOT_COMMIT ($BEHIND commits)" \
    --description "Brought into force $BINARY_COMMIT..$GOT_COMMIT ($BEHIND commits):
$SUBJECTS" >/dev/null 2>&1 || true

  # The restart is last because it terminates the process running this script;
  # the receipt above is already written. 'gt daemon restart' is launchctl
  # kickstart -k, and it is what puts the new binary in force for the daemon
  # itself, which runs this and every other script plugin in-process. A stop
  # followed by a start is not the pair to use: stop unloads the launchd job,
  # so a failure in between leaves the town with no daemon and no loaded job
  # (gt-sq9e).
  if gt daemon restart >/dev/null 2>&1; then
    log "Daemon restarted on $GOT_COMMIT."
  else
    log "WARNING: daemon restart failed; it is still running the previous binary."
    gt escalate "rebuild-gt: installed $GOT_COMMIT but the daemon did not restart" \
      -s medium --source "plugin:rebuild-gt" --fingerprint "rebuild-gt:restart-failed" 2>/dev/null || true
  fi
else
  ERROR="make build/safe-install failed"
  log "FAILED: $ERROR"
  gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
    --title "Plugin: rebuild-gt [failure]" \
    --description "Build failed: $ERROR" >/dev/null 2>&1 || true
  gt escalate "Plugin FAILED: rebuild-gt" -s medium 2>/dev/null || true
  exit 1
fi

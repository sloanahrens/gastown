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
# unrepaired (gt-oqbw). So this plugin installs — through scripts/install-gt.sh,
# the same locked path the refinery's post-merge hook uses (claude-7fc) — and
# leaves the daemon restart to the daemon: install-gt.sh writes
# restart-pending.json and the daemon exits for launchd at its idle point, so
# no install kills in-flight plugins. It is the backstop: the post-merge hook
# installs most merges first, and this plugin also escalates when the daemon
# has not come into force (rebuild-gt:daemon-not-in-force).
#
# Exit codes are this script's contract with the daemon (plugin.md sets
# [execution] allow_deferred_exit = true, without which the daemon reads exit
# 3 as an ordinary failure): 0 did the work OR refused safely — dirty
# checkout, wrong branch, diverged main, not safe to rebuild, no rig root
# (recorded as success or skipped); 3 DEFERRED — nothing accomplished, so
# deliberately no run record, because a record satisfies the cooldown gate and
# a run that accomplished nothing must not buy an hour before the retry;
# 1 FAILED — install-gt.sh failed (it escalates its own failure under
# install-gt:*), or the rig has no install-gt.sh (recorded as failure). Before
# changing that contract, read plugins/rebuild-gt/plugin.md and
# internal/daemon/plugin_script.go.
#
# Before changing the block counting, the starvation alarm, or the reserve
# path, read plugins/rebuild-gt/plugin.md (Starvation): the design lives there,
# and this file carries the code.

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

# --- Blocked-install state ----------------------------------------------------
#
# A run that installs nothing records nothing (see the header), so the fact
# that matters here — a due binary that the town has kept out of force for
# an hour — exists only in this file. It lives in daemon/ beside the rest of
# the daemon's runtime state (gt-kox0).
STARVE_STATE="${TOWN_ROOT}/daemon/rebuild-gt-state.json"
# Under this, a block is the town working as designed: gate holds measured
# 2m-20m20s on 2026-09-22, so a single busy gate is not an alarm. Past it the
# run escalates loudly and, when the gate is what is blocking, waits for the
# gate to release the slot and builds inside it rather than racing it
# (gt-kox0).
STARVE_MINUTES=${REBUILD_GT_STARVE_MINUTES:-30}
# The whole reserve budget — the wait for the gate to release the slot and the
# acquire that follows it share this one clock. plugin.md's [execution] timeout
# has to cover it plus the build and the install that follow.
RESERVE_WAIT=${REBUILD_GT_RESERVE_WAIT:-10m}

# seconds DURATION — a Go-style duration (90s, 10m, 1h30m) as whole seconds.
seconds() {
  python3 - "$1" <<'PY' 2>/dev/null || echo 0
import re, sys
s = sys.argv[1].strip()
if re.fullmatch(r"(\d+(?:\.\d+)?[hms])+", s):
    print(int(sum(float(v) * {"h": 3600, "m": 60, "s": 1}[u]
                  for v, u in re.findall(r"(\d+(?:\.\d+)?)([hms])", s))))
else:
    print(0)
PY
}

# starve_age — minutes since the current block opened; 0 when none is open.
starve_age() {
  python3 - "$STARVE_STATE" <<'PY' 2>/dev/null || echo 0
import json, sys, time
try:
    with open(sys.argv[1]) as f:
        d = json.load(f)
except Exception:
    d = {}
# A cleared block stores epoch 0, which is not "started in 1970" — reading it
# as a start time would make the next block arrive pre-aged and escalate and
# reserve on its first run (gt-kox0).
try:
    since = int(d.get("blocked_since_epoch") or 0)
except Exception:
    since = 0
print(max(0, (int(time.time()) - since) // 60) if since else 0)
PY
}

# starve_bump REASON — count one more blocked run and print the total. An open
# block keeps its start time: the age is what the alarm reads. It prints
# nothing and exits non-zero when the state file cannot be written, so the
# caller can say so: a write that fails and then reads back as "no block, age
# 0" is indistinguishable from a healthy run, which is the silence gt-kox0 is
# about.
starve_bump() {
  python3 - "$STARVE_STATE" "$1" <<'PY'
import datetime, json, os, sys, time

def stamp(t):
    return datetime.datetime.fromtimestamp(t, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

path, reason = sys.argv[1], sys.argv[2]
try:
    with open(path) as f:
        d = json.load(f)
except Exception:
    d = {}
now = time.time()
if not d.get("blocked_since_epoch"):
    d["blocked_since_epoch"] = int(now)
    d["blocked_since"] = stamp(now)
d["consecutive_defers"] = int(d.get("consecutive_defers") or 0) + 1
d["last_reason"] = reason
d["last_blocked_at"] = stamp(now)
os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path + ".tmp", "w") as f:
    json.dump(d, f, indent=2, sort_keys=True)
os.replace(path + ".tmp", path)
print(d["consecutive_defers"])
PY
}

# starve_clear REASON — close the block. Prints 1 when one was open, so a
# caller that should also close the alarm can tell the difference.
starve_clear() {
  python3 - "$STARVE_STATE" "$1" <<'PY' 2>/dev/null || echo 0
import datetime, json, os, sys, time
path, reason = sys.argv[1], sys.argv[2]
try:
    with open(path) as f:
        d = json.load(f)
except Exception:
    d = {}
if not d.get("blocked_since_epoch"):
    print(0)
    raise SystemExit(0)
d["blocked_since"] = None
d["blocked_since_epoch"] = 0
d["consecutive_defers"] = 0
# The next block is a new one and gets its own escalation.
d["escalated_epoch"] = 0
d["escalated_at"] = None
d["cleared_at"] = datetime.datetime.fromtimestamp(time.time(), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
d["cleared_reason"] = reason
with open(path + ".tmp", "w") as f:
    json.dump(d, f, indent=2, sort_keys=True)
os.replace(path + ".tmp", path)
print(1)
PY
}

# starve_escalated — prints 1 when this block has already been escalated.
# One firing per block: the escalation is what makes the block visible, an
# unacked one is re-escalated by the town's own cadence
# (settings/escalation.json), and a firing per heartbeat would be Dolt commits
# that buy nothing (gt-kox0, gt-vwry).
starve_escalated() {
  python3 - "$STARVE_STATE" <<'PY' 2>/dev/null || echo 0
import json, sys
try:
    with open(sys.argv[1]) as f:
        d = json.load(f)
except Exception:
    d = {}
print(1 if d.get("escalated_epoch") else 0)
PY
}

# starve_mark_escalated — record that this block's escalation went out. Read
# back by starve_escalated; kept separate from starve_escalated so the caller
# can run the escalation first and mark only what actually reached the town.
starve_mark_escalated() {
  python3 - "$STARVE_STATE" <<'PY' 2>/dev/null || echo 0
import datetime, json, os, sys, time
path = sys.argv[1]
try:
    with open(path) as f:
        d = json.load(f)
except Exception:
    d = {}
now = time.time()
d["escalated_epoch"] = int(now)
d["escalated_at"] = datetime.datetime.fromtimestamp(now, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path + ".tmp", "w") as f:
    json.dump(d, f, indent=2, sort_keys=True)
os.replace(path + ".tmp", path)
print(1)
PY
}

# note_blocked REASON — the binary was due and this run did not install it:
# count it, and escalate once the block outlasts $STARVE_MINUTES. One stable
# fingerprint, so a night-long block reads as one live escalation instead of
# one per heartbeat (gt-vwry). The escalation runs before the block is marked:
# a call that never reached the town must be retried on the next run, not
# recorded as delivered, or the one alarm this file exists for is lost in
# silence (gt-kox0). Returns without exiting: each caller keeps its own
# contract.
#
# A run can be blocked more than once — the reserve path notes the gate before
# its wait, then the deferral that ends the run notes what the wait did — and
# the count is runs, so only the first call counts and escalates. The reason is
# logged either way: it is the only place the run says which block it hit.
NOTE_BLOCKED=0
note_blocked() {
  if [ "$NOTE_BLOCKED" = "1" ]; then
    log "Still blocked: $1"
    return 0
  fi
  NOTE_BLOCKED=1

  local defers age
  if ! defers=$(starve_bump "$1"); then
    log "WARNING: could not record this blocked run in $STARVE_STATE"
    defers=0
  fi
  age=$(starve_age)
  log "Not installed (due, blocked ${age}m over $defers run(s)): $1"
  if [ "$age" -lt "$STARVE_MINUTES" ] || [ "$(starve_escalated)" = "1" ]; then
    return 0
  fi
  if ESCALATE_OUT=$(gt escalate "rebuild-gt: the binary has been due for install and blocked for ${age}m over $defers run(s); last: $1" \
    -s high --source "plugin:rebuild-gt" --fingerprint "rebuild-gt:starved" 2>&1); then
    if [ "$(starve_mark_escalated)" != "1" ]; then
      log "WARNING: the escalation went out but could not be recorded in $STARVE_STATE; it may fire again on the next run"
    fi
  else
    log "WARNING: the starvation escalation did not reach the town; retrying on the next run: $ESCALATE_OUT"
  fi
}

# blocked_defer REASON — note_blocked, then the deferral exit: nothing
# accomplished, so no run record, and the cooldown is not spent.
blocked_defer() {
  note_blocked "$1"
  exit "$DEFERRED"
}

# clear_alarms — every fingerprint this plugin owns asserts the binary is out
# of force somewhere; once it is in force those assertions are false and the
# producer closes them (gt-vwry). Clearing a key that is not open writes
# nothing. 'rebuild-gt:unverified' belongs here too: a binary that has just
# verified in force refutes it, and without the key the last false positive
# outlives its cause — it would stay open forever, and the town clears it by
# hand (gt-b5mpe).
clear_alarms() {
  gt escalate clear --fingerprint "rebuild-gt:starved" \
    --fingerprint "rebuild-gt:drift" --fingerprint "rebuild-gt:drift-unknown" \
    --fingerprint "rebuild-gt:unverified" --fingerprint "rebuild-gt:no-installer" \
    --fingerprint "rebuild-gt:install-failed" \
    --reason "rebuild-gt: the binary is in force" >/dev/null 2>&1 || true
}

# --- Pre-flight checks -------------------------------------------------------

log "Pre-flight checks..."

# The plugin runs from $TOWN_ROOT/plugins (a deployed copy, plugin.md), which
# is not a repo, so no git command may use the plugin's cwd. Every git call in
# this file therefore carries an explicit -C.
# No record-run call here (nothing to attribute the run to yet), so the
# daemon's own record is the only receipt this path produces; the skip
# marker (scriptSkippedMarker, gt-chqi) is what keeps that receipt honest —
# without it the daemon's automatic exit-0 record reads "success".
if [ ! -d "$RIG_ROOT" ]; then
  log "Rig root $RIG_ROOT does not exist. Skipping."
  echo "[plugin-result skipped]"
  exit 0
fi

# --- Drift escalation ---------------------------------------------------------
#
# Every bail below (dirty repo, wrong branch, diverged local main, an
# unreadable staleness check, "not safe to rebuild") exits 0 and records a
# healthy-looking "skipped" receipt. A bail that leaves a DUE binary out of
# force is counted by the starvation clock below, but that clock only runs on
# paths where DUE was read; this check runs unconditionally, before any of
# them, and is keyed to the OUTCOME THAT MATTERS — the binary drifting from
# origin/main — rather than to an enumeration of bail reasons. Enumerating
# failure modes is the same brittleness this town has hit repeatedly (gt-r8o,
# gt-50k); this catches every bail path above, including ones not yet written,
# because it doesn't ask why the rebuild didn't happen (gt-bce).
#
# 'gt stale --json' refreshes its own origin/main remote-tracking ref and
# only inspects git history, never RIG_ROOT's working-tree state, so it is
# meaningful here even though the checks below may bail on that same
# worktree being dirty or on the wrong branch.
MAX_COMMITS_BEHIND=${REBUILD_GT_MAX_COMMITS_BEHIND:-20}
# A merged commit that is not in force is a live defect, not a rounding error
# (gt-oqbw, gt-ww20, gt-rbfj): one commit behind is due. The post-merge hook
# (scripts/install-after-merge.sh) normally installs first; this plugin is the
# backstop for merges that bypass it (direct pushes, orphan-path merges, a
# failed hook), so waiting for a batch of commits only lengthens the inert
# window (claude-7fc).
THRESHOLD=${REBUILD_GT_INSTALL_THRESHOLD:-1}

# DUE is read from this same unconditional staleness call, before any bail
# below can leave the binary out of force. Every path that then declines to
# install calls note_blocked, so a due binary has one clock running against it
# no matter which reason kept it out of force (gt-kox0).
DRIFT_STALE=""
DRIFT_BEHIND=""
DRIFT_READ=0
DUE=""
if DRIFT_JSON=$(gt stale --json 2>/dev/null); then
  DRIFT_READ=1
  DRIFT_STALE=$(echo "$DRIFT_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('stale', False))" 2>/dev/null || echo "False")
  # commits_behind is only meaningful when present: 'or 0' would read a
  # missing/null count the same as "0 behind" and silently retire the alarm
  # this check exists for. An unknown count while the binary IS stale means
  # the drift could be 1 commit or 1000 — this cannot be ruled safe, so it
  # escalates too (gt-oqbw).
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

# --- Daemon in force (backstop, claude-7fc) -------------------------------------
#
# Installs no longer restart the daemon: install-gt.sh writes
# daemon/restart-pending.json and the daemon exits for a launchd restart at its
# own idle point. Two readings say that did not happen: a marker that has
# waited past the limit (the daemon never went idle, or never read it), or a
# daemon whose recorded commit (state.json "commit", possibly short) is not a
# descendant of the installed binary's while that binary has been in place past
# the limit (a crash loop, or an install by some path that wrote no marker).
# Each run that sees the lag re-sends the HIGH escalation under one
# fingerprint, which the town dedupes into one bead per episode; the flag file
# is what lets a later healthy run close it, so a healthy run with no episode
# open writes nothing.
DAEMON_LAG_MINUTES=${REBUILD_GT_DAEMON_LAG_MINUTES:-30}
LAG_FLAG="${TOWN_ROOT}/daemon/rebuild-gt-daemon-lag"

marker_age_minutes() {
  python3 - "${TOWN_ROOT}/daemon/restart-pending.json" <<'PY' 2>/dev/null || true
import datetime, json, sys, time
m = json.load(open(sys.argv[1]))
t = datetime.datetime.strptime(m["requested_at"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
print(int((time.time() - t.timestamp()) // 60))
PY
}
daemon_state_commit() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("commit") or "")' "${TOWN_ROOT}/daemon/state.json" 2>/dev/null || true
}
file_age_minutes() {
  python3 -c 'import os,sys,time; print(int((time.time() - os.path.getmtime(sys.argv[1])) // 60))' "$1" 2>/dev/null || echo 0
}

LAG=""
# IN_FORCE is set only by a positive healthy reading: no marker pending, and
# condition 2 actually evaluated with the daemon's commit a descendant of the
# installed binary's. A reading that could not be taken (gt stale failed,
# state.json has no commit, a commit does not resolve) is neither lag nor
# health, so it leaves an open escalation alone instead of flapping it.
IN_FORCE=""
MARKER_AGE=$(marker_age_minutes)
if [ -n "$MARKER_AGE" ] && [ "$MARKER_AGE" -ge "$DAEMON_LAG_MINUTES" ]; then
  LAG="a restart-pending marker has waited ${MARKER_AGE}m for the daemon to restart"
elif [ "$DRIFT_READ" = "1" ]; then
  DC=$(daemon_state_commit)
  BC=$(echo "$DRIFT_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('binary_commit') or '')" 2>/dev/null || true)
  DCF=$( [ -n "$DC" ] && git -C "$RIG_ROOT" rev-parse --verify --quiet "$DC^{commit}" 2>/dev/null || true )
  BCF=$( [ -n "$BC" ] && git -C "$RIG_ROOT" rev-parse --verify --quiet "$BC^{commit}" 2>/dev/null || true )
  GT_BIN=$(command -v gt 2>/dev/null || true)
  if [ -z "$DCF" ] || [ -z "$BCF" ] || [ -z "$GT_BIN" ]; then
    log "Daemon-in-force check skipped: cannot resolve the daemon's commit ('$DC') or the binary's ('$BC')."
  elif git -C "$RIG_ROOT" merge-base --is-ancestor "$BCF" "$DCF" 2>/dev/null; then
    [ -e "${TOWN_ROOT}/daemon/restart-pending.json" ] || IN_FORCE=1
  else
    BIN_AGE=$(file_age_minutes "$GT_BIN")
    if [ "$BIN_AGE" -ge "$DAEMON_LAG_MINUTES" ]; then
      LAG="the daemon runs $DC but $BC has been installed for ${BIN_AGE}m"
    fi
  fi
fi
if [ -n "$LAG" ]; then
  log "Daemon not in force: $LAG."
  if gt escalate "rebuild-gt: $LAG" -s high --source "plugin:rebuild-gt" \
    --fingerprint "rebuild-gt:daemon-not-in-force" >/dev/null 2>&1; then
    mkdir -p "$(dirname "$LAG_FLAG")" 2>/dev/null || true
    touch "$LAG_FLAG" 2>/dev/null || true
  fi
elif [ -n "$IN_FORCE" ] && [ -e "$LAG_FLAG" ]; then
  # The flag goes only once the clear landed: a failed clear keeps it, so the
  # next healthy run retries instead of leaving the alert with no closer.
  if gt escalate clear --fingerprint "rebuild-gt:daemon-not-in-force" \
    --reason "rebuild-gt: the daemon runs the installed binary" >/dev/null 2>&1; then
    rm -f "$LAG_FLAG"
  else
    log "WARNING: could not clear rebuild-gt:daemon-not-in-force; will retry next run."
  fi
fi

if [ "$DRIFT_STALE" = "True" ]; then
  # An unknown count is due, matching the threshold gate below: it cannot be
  # ruled under a threshold it does not have (gt-oqbw).
  if [ "$DRIFT_BEHIND" = "unknown" ] || [ "$DRIFT_BEHIND" -ge "$THRESHOLD" ]; then
    DUE=1
  fi
fi

# A reading that says the binary is NOT due ends any block on the books (DUE is
# this same reading taken the other way round). Nothing starves a binary that
# needs no installing, and a block left open past the condition it measured
# outlives it: a hand-install during a dirty checkout, say, then main moving
# past the threshold again — the next block inherits the old start time and
# escalates and reserves on its very first run (gt-kox0). Only a reading that
# succeeded clears, because an unreadable staleness check has not shown
# anything about the binary.
if [ "$DRIFT_READ" = "1" ] && [ -z "$DUE" ]; then
  if [ "$(starve_clear "binary is not due")" = "1" ]; then
    gt escalate clear --fingerprint "rebuild-gt:starved" \
      --reason "rebuild-gt: the binary is not due for install" >/dev/null 2>&1 || true
  fi
fi

# Only TRACKED modifications outside .beads/ can change what 'make build'
# produces. Untracked entries (.agents/, .codex/, .worktrees/) and bd's own
# rewriting of .beads/config.yaml must not trip this guard: a plain
# --porcelain check skipped every rebuild for hours while exiting 0 (gt-50k).
DIRTY=$(git -C "$RIG_ROOT" status --porcelain --untracked-files=no -- . ':(exclude).beads' 2>/dev/null)
if [ -n "$DIRTY" ]; then
  log "Repo is dirty, skipping rebuild."
  if [ -n "$DUE" ]; then note_blocked "repo has uncommitted changes"; fi
  echo "[plugin-result skipped]"
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: repo has uncommitted changes" >/dev/null 2>&1 || true
  exit 0
fi

BRANCH=$(git -C "$RIG_ROOT" branch --show-current 2>/dev/null)
if [ "$BRANCH" != "main" ]; then
  log "Not on main branch (on $BRANCH), skipping rebuild."
  if [ -n "$DUE" ]; then note_blocked "not on main branch (on $BRANCH)"; fi
  echo "[plugin-result skipped]"
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
#
# The sync is a write to mayor/rig, and every write to mayor/rig happens under
# install-gt.sh's flock (claude-7fc), so it cannot move the tree under a build
# the post-merge hook is running. The lock is released right after the sync,
# before install-gt.sh runs and takes it again itself. A lock that stays busy
# means an install is running right now: defer to the next heartbeat, as
# install-gt's own exit 3 does.
INSTALL_LOCK="${INSTALL_GT_DAEMON_DIR:-${TOWN_ROOT}/daemon}/install-gt.lock"
LOCK_WAIT=${REBUILD_GT_LOCK_WAIT:-30}
# What install-gt.sh itself waits for the same lock (seconds); see the call.
INSTALL_LOCK_WAIT=${REBUILD_GT_INSTALL_LOCK_WAIT:-60}

# install_lock_take — flock the install lock on fd 9, waiting up to
# $LOCK_WAIT seconds; false when it stays busy. perl locks the shell's own open
# file description (macOS has no flock(1)), so the lock outlives perl and is
# held until fd 9 closes: install_lock_release, or this process exiting.
install_lock_take() {
  mkdir -p "$(dirname "$INSTALL_LOCK")" 2>/dev/null || true
  exec 9>>"$INSTALL_LOCK" || return 1
  if ! perl -e '
    use Fcntl qw(:flock);
    open(my $fh, ">&=", 9) or exit 1;
    my $deadline = time + $ARGV[0];
    until (flock($fh, LOCK_EX | LOCK_NB)) {
      exit 1 if time >= $deadline;
      select(undef, undef, undef, 0.5);
    }
    exit 0;
  ' "$LOCK_WAIT"; then
    exec 9>&-
    return 1
  fi
}
install_lock_release() { exec 9>&-; }

if ! install_lock_take; then
  if [ -n "$DUE" ]; then note_blocked "another install holds the install lock"; fi
  defer "another install holds the install lock ($INSTALL_LOCK) past ${LOCK_WAIT}s; not syncing $RIG_ROOT"
fi
# Every git call under the lock closes fd 9 (9>&-): git can fork a detached
# 'gc --auto' that would otherwise inherit the lock and hold it past this run.
log "Syncing $RIG_ROOT with origin/main..."
git -C "$RIG_ROOT" fetch origin --quiet 9>&- 2>/dev/null || true
if ! git -C "$RIG_ROOT" merge --ff-only origin/main --quiet 9>&- 2>/dev/null; then
  # rev-list --count always prints a number, including "0", so testing it
  # with -n is always true; that bug let this branch treat every ff-only
  # failure as a real divergence and skipped the retry below unconditionally
  # (gt-9jax). COUNT is local-only commits: >0 means HEAD has something
  # origin/main lacks (a real divergence); 0 means the ff-only failed for
  # some other reason (e.g. an untracked file the merge would overwrite),
  # which a re-fetch+re-merge can still resolve if that reason has cleared.
  COUNT=$(git -C "$RIG_ROOT" rev-list origin/main..HEAD --count 9>&- 2>/dev/null || echo 0)
  if [ "$COUNT" -gt 0 ]; then
    install_lock_release
    log "Local main diverged from origin/main, skipping rebuild."
    if [ -n "$DUE" ]; then note_blocked "local main diverged from origin/main"; fi
    echo "[plugin-result skipped]"
    gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
      --title "Plugin: rebuild-gt [skipped]" \
      --description "Skipped: local main diverged from origin/main" >/dev/null 2>&1 || true
    exit 0
  fi
  # COUNT is 0: local HEAD has nothing origin/main lacks, so the ff-only
  # failure is not a divergence (e.g. an untracked file the fast-forward
  # would overwrite). Re-fetch and re-merge exactly once; if it still fails
  # the retry didn't clear whatever blocked it and the original bail stands.
  log "origin/main moved during sync; re-fetching and re-merging once..."
  git -C "$RIG_ROOT" fetch origin --quiet 9>&- 2>/dev/null || true
  if ! git -C "$RIG_ROOT" merge --ff-only origin/main --quiet 9>&- 2>/dev/null; then
    install_lock_release
    log "Local main diverged from origin/main, skipping rebuild."
    if [ -n "$DUE" ]; then note_blocked "local main diverged from origin/main"; fi
    echo "[plugin-result skipped]"
    gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
      --title "Plugin: rebuild-gt [skipped]" \
      --description "Skipped: local main diverged from origin/main" >/dev/null 2>&1 || true
    exit 0
  fi
fi
install_lock_release

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
# repo_commit is deliberately not read here: it can move past what was
# actually built (gt-oqbw), so install-gt.sh verifies against the rig's HEAD
# at build time instead. BINARY_COMMIT is the receipt's fallback "from".
BINARY_COMMIT=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('binary_commit') or '')" 2>/dev/null || echo "")

if [ "$IS_STALE" != "True" ]; then
  log "Binary is fresh. Nothing to do."
  starve_clear "binary is fresh" >/dev/null
  clear_alarms
  gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
    --title "rebuild-gt: binary is fresh" >/dev/null 2>&1 || true
  exit 0
fi

if [ "$SAFE" != "True" ]; then
  log "Not safe to rebuild (not on main or would be a downgrade). Skipping."
  if [ -n "$DUE" ]; then note_blocked "not safe to rebuild"; fi
  echo "[plugin-result skipped]"
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: not safe to rebuild" >/dev/null 2>&1 || true
  exit 0
fi

# An unknown count must not read as "0 behind, safely under threshold" —
# that reading is exactly what let a stale, quiet, safe-to-rebuild binary
# sit deferred every heartbeat with the install threshold never satisfied
# and no alarm anywhere (gt-oqbw). The drift check above already
# escalates this state; here, since we're already stale + safe + quiet,
# proceed with the install rather than withhold it pending a count that
# 'gt stale' could not produce.
if [ "$BEHIND" = "unknown" ]; then
  log "commits_behind is unknown; proceeding with install rather than parking it on an unmeasurable threshold"
elif [ "$BEHIND" -lt "$THRESHOLD" ]; then
  # Not due, so any block from an earlier run is over. The alarm follows the
  # state: closed here only when it was open, so an ordinary sub-threshold run
  # costs one read and no write (gt-vwry).
  if [ "$(starve_clear "binary is under the install threshold")" = "1" ]; then
    gt escalate clear --fingerprint "rebuild-gt:starved" \
      --reason "rebuild-gt: the binary is under the install threshold" >/dev/null 2>&1 || true
  fi
  defer "binary is $BEHIND behind (under the install threshold $THRESHOLD)"
fi

# --- Build -------------------------------------------------------------------

# in_flight_count — MRs a refinery is mid-merge on, or "" when the list could
# not be read at all. Both readings fail open, but only one is worth a warning,
# so the caller decides (gt-oqbw).
in_flight_count() {
  gt mq list gastown --status=in_progress --json 2>/dev/null | python3 -c '
import json, sys
try:
    print(len(json.load(sys.stdin)))
except Exception:
    pass
' 2>/dev/null | tail -1
}

# install_requires_quiet [WARN] — return when no merge is in flight, defer when
# one is. Called before the build.
install_requires_quiet() {
  local n
  n=$(in_flight_count)
  if [ -z "$n" ] || ! [[ "$n" =~ ^[0-9]+$ ]]; then
    if [ "${1:-}" = "warn" ]; then
      log "WARNING: could not read in-flight MR count from 'gt mq list'; failing open (treating as quiet) rather than parking the rebuild"
    fi
    return 0
  fi
  if [ "$n" -gt 0 ]; then
    blocked_defer "not quiet: $n merge(s) in flight in gastown"
  fi
}

# Yield to a running gate (gt-htx3): make build competes for CPU with a gate
# suite whose tests are load-sensitive, so a rebuild while a refinery, batch,
# main-branch-test or om-review role holds a container-gate slot is deferred,
# and past the starvation threshold it waits for that slot instead.
#
# slot_status_lines — the container-gate reading, three lines: the gate-class
# roles holding a slot right now (comma-joined, empty when none hold one), the
# reason the town is not quiet that no slot serializes (empty when there is
# none), and "reservable" when the blocker is one the slot does serialize. A
# stub or missing gt slot status prints nothing, and the guard fails open on
# purpose so a broken status command cannot park rebuilds forever (the drift
# escalation above still fires if that happens).
slot_status_lines() {
  gt slot status --json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
roles = []
for s in d.get("slots") or []:
    role = ((s.get("owner") or {}).get("role") or "")
    if role.endswith(("/refinery", "/refinery-batch", "/main-branch-test", "/om-review")):
        roles.append(role)
print(", ".join(roles))
c = d.get("unwrapped_containers") or []
if c:
    print("container(s) are running outside the gate: %s" % ", ".join(c))
elif d.get("saturated"):
    print("every container-gate slot is held")
else:
    print("")
print("reservable" if roles or d.get("saturated") else "")
' 2>/dev/null || true
}

# gate_holders — just the roles from that reading, one line.
gate_holders() { slot_status_lines | sed -n 1p; }

# wait_for_gate_free — block until no gate-class role holds a container-gate
# slot, or the reserve budget runs out; false on the budget. The poll is what
# the acquire alone cannot do: the pool hands a free slot to a non-gate role
# immediately, so on a town whose pool has more than one slot the build would
# start beside the very suite the yield exists to stay out of — the contention
# gt-htx3 added the yield for (gt-kox0).
wait_for_gate_free() {
  local interval=${REBUILD_GT_POLL_SECONDS:-15}
  while :; do
    if [ -z "$(gate_holders)" ]; then
      return 0
    fi
    if [ "$(date +%s)" -ge "$RESERVE_DEADLINE" ]; then
      return 1
    fi
    sleep "$interval"
  done
}

# reserve_remaining — seconds left of the shared reserve budget, with a floor
# so a gate that takes the slot the moment the wait ends still gets one honest
# acquire.
reserve_remaining() {
  local left=$(( RESERVE_DEADLINE - $(date +%s) ))
  if [ "$left" -lt 60 ]; then left=60; fi
  printf '%s' "$left"
}

# The town must also have nothing in flight (gt-oqbw): an MR a refinery is
# mid-merge on is running a gate suite the build would compete with (the
# install itself no longer restarts anything, claude-7fc). An MR merely ready in the
# queue is NOT a reason to wait — it consumes nothing, and at this town's
# merge rate the queue is never empty, so requiring that would leave the
# install waiting forever. An unreadable Docker status is not a reason either:
# it means the cross-check could not tell, and a VM that is down runs no suite
# for a build to compete with.
GATE_READING=$(slot_status_lines)
GATE_HOLDERS=$(printf '%s\n' "$GATE_READING" | sed -n 1p)
GATE_OTHER=$(printf '%s\n' "$GATE_READING" | sed -n 2p)
GATE_RESERVABLE=$(printf '%s\n' "$GATE_READING" | sed -n 3p)
RESERVE=0
RESERVE_DEADLINE=0
GATE_BUSY=""
if [ -n "$GATE_HOLDERS" ]; then
  GATE_BUSY="a gate suite holds a slot ($GATE_HOLDERS)"
elif [ -n "$GATE_OTHER" ]; then
  GATE_BUSY="$GATE_OTHER"
fi
if [ -n "$GATE_BUSY" ]; then
  if [ "$GATE_RESERVABLE" = "reservable" ] && [ "$(starve_age)" -ge "$STARVE_MINUTES" ]; then
    # Past the threshold with the slot as the blocker: wait for it instead of
    # racing it, so this build queues like any other gate consumer (gt-kox0).
    RESERVE=1
  else
    blocked_defer "not quiet: $GATE_BUSY"
  fi
fi

install_requires_quiet warn

if [ "$RESERVE" = "1" ]; then
  # Counted before the wait, not after: the run is blocked from the moment it
  # starts waiting, and the alarm should not be hostage to the wait finishing.
  note_blocked "not quiet: $GATE_BUSY"
  RESERVE_SECONDS=$(seconds "$RESERVE_WAIT")
  if [ "$RESERVE_SECONDS" -le 0 ]; then
    log "WARNING: REBUILD_GT_RESERVE_WAIT='$RESERVE_WAIT' is not a duration; using 10m"
    RESERVE_WAIT=10m
    RESERVE_SECONDS=600
  fi
  RESERVE_DEADLINE=$(( $(date +%s) + RESERVE_SECONDS ))
  log "Blocked $(starve_age)m: waiting up to $RESERVE_WAIT for the gate to release the slot, then building inside it."
  wait_for_gate_free || blocked_defer "waited $RESERVE_WAIT for the container-gate slot and did not get it"
fi

log "Installing gt from $RIG_ROOT ($BEHIND commits behind) through scripts/install-gt.sh..."

# One install path for the town (claude-7fc): the rig's own install-gt.sh builds,
# installs, verifies (rolling back on a failed smoke check), syncs formulas and
# plugins, and writes the restart-pending marker; the daemon restarts itself at
# its idle point, so nothing here kills in-flight plugins — this one included.
# It runs under the install lock, which the post-merge hook takes too, so the two
# can never build into one output. What it builds is RIG_ROOT's HEAD, which the
# sync above fast-forwarded to origin/main (SKIP_UPDATE_CHECK stays inside it,
# gt-9jax).
INSTALLER="$RIG_ROOT/scripts/install-gt.sh"
if [ ! -f "$INSTALLER" ]; then
  log "FAILED: $INSTALLER is missing (the rig checkout is older than this plugin)"
  gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
    --title "Plugin: rebuild-gt [no installer]" \
    --description "$INSTALLER does not exist" >/dev/null 2>&1 || true
  gt escalate "rebuild-gt: $INSTALLER is missing, so nothing can install gt" -s medium \
    --source "plugin:rebuild-gt" --fingerprint "rebuild-gt:no-installer" 2>/dev/null || true
  exit 1
fi
TARGET=$(git -C "$RIG_ROOT" rev-parse HEAD)
INSTALL_ARGS=(--sha "$TARGET" --source rebuild-gt)
if [ "$RESERVE" = "1" ]; then
  INSTALL_ARGS+=(--slot-role gastown/rebuild-gt --slot-timeout "$(reserve_remaining)")
fi

# Teed, not captured: a build waiting on the slot would otherwise print nothing
# to the plugin log until it finished, and read as hung (gt-kox0).
INSTALL_LOG=$(mktemp)
set +e
# INSTALL_GT_LOCK_WAIT: a post-merge install holding the lock means one is
# running right now; wait a minute, not install-gt's default 5, then defer
# (exit 3). Waiting longer only keeps this plugin running, which keeps the
# daemon non-idle and delays its upgrade restart.
INSTALL_GT_LOCK_WAIT="$INSTALL_LOCK_WAIT" INSTALL_GT_RIG_DIR="$RIG_ROOT" \
  bash "$INSTALLER" "${INSTALL_ARGS[@]}" 2>&1 | tee "$INSTALL_LOG"
INSTALL_RC=${PIPESTATUS[0]}
set -e
RESULT=$(grep -a '^install-gt: RESULT ' "$INSTALL_LOG" | tail -1 || true)
rm -f "$INSTALL_LOG"
# install-gt: RESULT <event> <commit> <prev> <reason>
R_EVENT="" R_COMMIT="" R_PREV="" R_REASON=""
read -r _ _ R_EVENT R_COMMIT R_PREV R_REASON <<<"$RESULT" || true
# igt_result writes "-" for an absent field.
[ "$R_COMMIT" = "-" ] && R_COMMIT=""
[ "$R_PREV" = "-" ] && R_PREV=""
[ "$R_REASON" = "-" ] && R_REASON=""

case "$INSTALL_RC" in
  0)
    if [ "$R_EVENT" = "noop" ]; then
      log "Binary already contains $TARGET."
      starve_clear "binary is fresh" >/dev/null
      clear_alarms
      gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
        --title "rebuild-gt: binary is fresh" >/dev/null 2>&1 || true
      exit 0
    fi
    FROM="${R_PREV:-$BINARY_COMMIT}"
    TO="${R_COMMIT:-$TARGET}"
    # The commits in this range were merged and not running until now: that
    # list is the inert window, so "was gt-ww20 ever in force, and when" is
    # answered by the receipt instead of reconstructed from merge timestamps.
    SUBJECTS=$(git -C "$RIG_ROOT" log --no-decorate --oneline "$FROM..$TO" 2>/dev/null | head -20 || true)
    gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
      --title "rebuild-gt: in force $FROM -> $TO ($BEHIND commits)" \
      --description "Brought into force $FROM..$TO ($BEHIND commits); the daemon restarts at its idle point:
$SUBJECTS" >/dev/null 2>&1 || true
    starve_clear "installed $TO" >/dev/null
    clear_alarms
    log "In force: $FROM -> $TO. Restart marker written; the daemon restarts when idle."
    exit 0
    ;;
  2)
    log "install-gt refused: ${R_REASON:-unknown}"
    if [ -n "$DUE" ]; then note_blocked "install-gt refused: ${R_REASON:-unknown}"; fi
    echo "[plugin-result skipped]"
    gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
      --title "Plugin: rebuild-gt [skipped]" \
      --description "Skipped: install-gt refused (${R_REASON:-unknown})" >/dev/null 2>&1 || true
    exit 0
    ;;
  3)
    if [ "$R_REASON" = "slot-busy" ]; then
      blocked_defer "waited for the container-gate slot and did not get it"
    fi
    blocked_defer "another install holds the install lock"
    ;;
  *)
    log "FAILED: install-gt exited $INSTALL_RC (${R_REASON:-no result line})"
    # install-gt escalates these reasons itself (install-gt:build-failed,
    # :smoke-failed, :rollback-failed, :marker-write-failed). Anything else —
    # its 'unexpected' trap, no-rig, or no RESULT line at all — reached nobody,
    # and the failure record starts a cooldown, so escalate it here.
    case "$R_REASON" in
      build-failed|smoke-failed|rollback-failed|marker-write)
        ESCALATED_BY="escalated by install-gt" ;;
      *)
        ESCALATED_BY="escalated by rebuild-gt"
        gt escalate "rebuild-gt: install-gt failed (${R_REASON:-no result line}, exit $INSTALL_RC) installing $TARGET" \
          -s medium --source "plugin:rebuild-gt" \
          --fingerprint "rebuild-gt:install-failed" >/dev/null 2>&1 \
          || { log "WARNING: escalation rebuild-gt:install-failed did not reach the town"; ESCALATED_BY="escalation failed"; }
        ;;
    esac
    gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
      --title "Plugin: rebuild-gt [failure]" \
      --description "install-gt failed: ${R_REASON:-exit $INSTALL_RC} ($ESCALATED_BY)" >/dev/null 2>&1 || true
    exit 1
    ;;
esac

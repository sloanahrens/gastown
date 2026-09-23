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
# Exit codes are this script's contract with the daemon (plugin.md sets
# [execution] allow_deferred_exit = true, without which the daemon reads exit
# 3 as an ordinary failure): 0 did the work OR refused safely — dirty
# checkout, wrong branch, diverged main, not safe to rebuild, no rig root
# (recorded as success or skipped); 3 DEFERRED — nothing accomplished, so
# deliberately no run record, because a record satisfies the cooldown gate and
# a run that accomplished nothing must not buy an hour before the retry;
# 1 FAILED — build failed, or the install could not be verified as in force
# (recorded as failure and escalated under a stable fingerprint). Before
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
# nothing.
clear_alarms() {
  gt escalate clear --fingerprint "rebuild-gt:starved" \
    --fingerprint "rebuild-gt:drift" --fingerprint "rebuild-gt:drift-unknown" \
    --reason "rebuild-gt: the binary is in force" >/dev/null 2>&1 || true
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
# A merged commit that is not in force is a live defect, not a rounding error:
# gt-ww20 (a batch path that bypassed the editorial gate, so 4 MRs landed
# unreviewed) and gt-rbfj (composer-stall recovery typing the literal text
# 'C-x C-s' into the composer, the cause of both refinery stalls that day)
# were each merged while the town kept running a binary without them
# (gt-oqbw). So the install does not wait for a large delta: past this many
# commits, install at the first quiet moment. Raise it to trade inert fixes
# for fewer daemon restarts.
THRESHOLD=${REBUILD_GT_INSTALL_THRESHOLD:-5}

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
  gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
    --title "Plugin: rebuild-gt [skipped]" \
    --description "Skipped: repo has uncommitted changes" >/dev/null 2>&1 || true
  exit 0
fi

BRANCH=$(git -C "$RIG_ROOT" branch --show-current 2>/dev/null)
if [ "$BRANCH" != "main" ]; then
  log "Not on main branch (on $BRANCH), skipping rebuild."
  if [ -n "$DUE" ]; then note_blocked "not on main branch (on $BRANCH)"; fi
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
  if [ -n "$DUE" ]; then note_blocked "local main diverged from origin/main"; fi
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
# field can move past what was actually built (gt-oqbw).
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
# one is. Called twice: before the build, and again immediately before the
# install, because the build takes minutes and a merge that was not in flight
# when the run started can be in flight by the time it finishes (gt-kox0).
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
# mid-merge on is work this restart would interrupt. An MR merely ready in the
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

log "Rebuilding gt from $RIG_ROOT ($BEHIND commits behind)..."

# What was actually built is RIG_ROOT's HEAD at build time — not the
# repo_commit read earlier from 'gt stale --json'. That call does its own
# independent fetch (CheckStaleBinaryFresh) and can therefore report a
# repo_commit that has already moved past what this checkout's HEAD (and thus
# this build) contains (gt-oqbw). Reading it fresh from RIG_ROOT
# immediately before the build ties the expectation to the tree actually
# compiled.
EXPECTED_COMMIT=$(git -C "$RIG_ROOT" rev-parse HEAD)

# build_gt — the CPU-heavy half, run inside the container-gate slot when a held
# slot is what has been blocking this rebuild (gt-kox0). Non-zero means the
# build failed; a wait that never got the slot is a deferral, not a failure.
build_gt() {
  if [ "$RESERVE" != "1" ]; then
    (cd "$RIG_ROOT" && make build) 2>&1 || return $?
    return 0
  fi
  local logf rc=0
  logf=$(mktemp) || return 1
  log "Building inside the container-gate slot (acquire budget $(reserve_remaining)s)."
  # Teed rather than captured: a build that spends the acquire budget waiting
  # would otherwise print nothing to the plugin log until it finished, and read
  # as hung (gt-kox0).
  (cd "$RIG_ROOT" && gt slot run --role gastown/rebuild-gt --timeout "$(reserve_remaining)s" -- make build) 2>&1 \
    | tee "$logf" || rc=$?
  # 'gt slot run' prints this the moment it holds the slot — the literal is
  # slotAcquiredFormat in internal/cmd/slot.go, pinned by
  # TestSlotAcquiredFormat. Without it the build never started, so nothing was
  # built: the retry belongs on the next heartbeat, not in an escalation.
  if ! grep -q "Container-gate slot acquired" "$logf"; then
    rm -f "$logf"
    blocked_defer "waited for the container-gate slot and did not get it"
  fi
  rm -f "$logf"
  return "$rc"
}

# The install stays outside the slot hold: it is a temp-file rename, not a CPU
# consumer, so releasing the slot when the build ends costs the gate nothing.
if build_gt && install_requires_quiet && (cd "$RIG_ROOT" && make safe-install) 2>&1; then
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
  # 'gt version' prints the binary's build-time commit abbreviated
  # (internal/version.ShortCommit, <=12 chars); EXPECTED_COMMIT is a full
  # 40-character hash. Resolve the abbreviated form to a full hash inside
  # RIG_ROOT (the one repo it's guaranteed unambiguous in) before comparing
  # like with like (gt-oqbw).
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

  # Nothing is blocked any more: the same fact that makes the receipt above a
  # success is what closes the block this file was counting and every alarm
  # keyed to the binary being out of force (gt-kox0).
  starve_clear "installed $GOT_COMMIT" >/dev/null
  clear_alarms

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

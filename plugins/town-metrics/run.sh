#!/usr/bin/env bash
# A daemon plugin must be a script, so this wrapper exists; the snapshot itself
# is Go (gt metrics snapshot). What it does, and when it skips: plugin.md beside
# this script (gt-tctk4).

set -euo pipefail

TOWN_ROOT="${GT_TOWN_ROOT:-${GT_ROOT:-$HOME/gt}}"
LOCK_FILE="$TOWN_ROOT/.runtime/town-metrics.lock"

log() { printf '[town-metrics] %s\n' "$*"; }

fail() { printf 'town-metrics: %s\n' "$*" >&2; exit 1; }

# holder_alive PID reports whether PID is a process right now. A run the
# daemon's timeout killed leaves its pid in the lock with nothing behind it,
# and that is a crash leftover rather than a holder: the pid is the only way
# to tell the two apart.
holder_alive() {
  [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null
}

# LOCK_STALE_AFTER_MIN is how long a lock naming no readable pid sits before it
# is debris: the daemon kills a script plugin at its [execution] timeout
# (plugin.md), so a lock older than that plus a margin has no writer behind it.
LOCK_STALE_AFTER_MIN=20

# lock_aged reports whether LOCK_FILE has sat unmodified for
# LOCK_STALE_AFTER_MIN minutes. A lock file is written once, at claim, so its
# mtime is when its run started.
lock_aged() {
  [ -n "$(find "$LOCK_FILE" -mmin "+$LOCK_STALE_AFTER_MIN" 2>/dev/null)" ]
}

# claim_pid PID publishes PID as this run's claim on LOCK_FILE, and reports
# whether it did. The pid is written to a private file and that file is linked
# into place, so the lock never exists empty or half-written: ln fails when the
# path is taken, which is the exclusive create this needs, and a reader sees a
# whole pid or no file at all.
claim_pid() {
  local claim="$LOCK_FILE.claim.$$" rc=0
  if ! printf '%s\n' "$1" >"$claim" 2>/dev/null; then
    rm -f "$claim"
    return 1
  fi
  ln "$claim" "$LOCK_FILE" 2>/dev/null || rc=1
  rm -f "$claim"
  return "$rc"
}

# take_lock claims LOCK_FILE for this run, and reports whether it did. A lock
# naming a live run is that run's. A lock naming a dead pid is a crash
# leftover; the rename that takes it over is checked against the pid found
# dead, because a run that claims the lock between that read and the rename
# must keep it. A lock naming no readable pid is a claim in progress or debris,
# and only its age tells them apart.
take_lock() {
  local dir holder aside
  dir="$(dirname "$LOCK_FILE")"
  mkdir -p "$dir" 2>/dev/null || fail "cannot create $dir"
  claim_pid "$$" && return 0
  holder="$(cat "$LOCK_FILE" 2>/dev/null || true)"
  case "$holder" in
    ''|*[!0-9]*) lock_aged || return 1 ;;
    *) holder_alive "$holder" && return 1 ;;
  esac
  aside="$LOCK_FILE.stale.$$"
  mv "$LOCK_FILE" "$aside" 2>/dev/null || return 1
  if [ "$(cat "$aside" 2>/dev/null || true)" != "$holder" ]; then
    # The lock on the path is not the one judged stale: another run took it in
    # between, so give the path back to that run.
    ln "$aside" "$LOCK_FILE" 2>/dev/null || true
    rm -f "$aside"
    return 1
  fi
  rm -f "$aside"
  claim_pid "$$"
}

# release_lock removes the lock only while it still names this run: after a
# takeover it is the next run's, and deleting it would let a third run in
# beside that one.
release_lock() {
  if [ "$(cat "$LOCK_FILE" 2>/dev/null || true)" = "$$" ]; then
    rm -f "$LOCK_FILE"
  fi
  return 0
}

command -v gt >/dev/null 2>&1 || fail "'gt' is not on PATH; nothing can be snapshotted"
[ -d "$TOWN_ROOT" ] || fail "no town root at $TOWN_ROOT"
# gt resolves the town it appends to from the cwd; the lock is under the town
# named here, and one run must not lock one town and snapshot another.
cd "$TOWN_ROOT"

if ! take_lock; then
  log "another run holds $LOCK_FILE; the next cooldown retries"
  printf '[plugin-result skipped]\n'
  exit 0
fi
trap release_lock EXIT

# Both steps run whichever one fails, and the run reports the first failure: an
# analysis that fails must not cost the hour its row, and a snapshot that fails
# still leaves the analysis the hours already in the log. A source that failed
# inside the snapshot is an errors entry, not a failure here; only a failed
# append is.
rc=0
gt metrics snapshot || rc=$?
snapshot_rc=$rc
rc=0
gt metrics analyse || rc=$?
if [ "$rc" -eq 0 ]; then
  rc=$snapshot_rc
fi
exit "$rc"

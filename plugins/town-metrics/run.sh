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

# take_lock claims LOCK_FILE for this run, and reports whether it did. The
# write is a noclobber create, so the pid is in the file from the moment it
# exists and two claimers cannot both win it. A stale file is renamed aside by
# one takeover and the other's rename then fails, so the takeover is single too.
take_lock() {
  local dir
  dir="$(dirname "$LOCK_FILE")"
  mkdir -p "$dir" 2>/dev/null || fail "cannot create $dir"
  if (set -o noclobber; printf '%s\n' "$$" >"$LOCK_FILE") 2>/dev/null; then
    return 0
  fi
  holder_alive "$(cat "$LOCK_FILE" 2>/dev/null || true)" && return 1
  local aside="$LOCK_FILE.stale.$$"
  mv "$LOCK_FILE" "$aside" 2>/dev/null || return 1
  rm -f "$aside"
  (set -o noclobber; printf '%s\n' "$$" >"$LOCK_FILE") 2>/dev/null
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

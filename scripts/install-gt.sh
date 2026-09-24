#!/usr/bin/env bash
# install-gt.sh — build, install and verify gt at one commit. The single
# install path, shared by the refinery's post-merge hook
# (scripts/install-after-merge.sh) and the rebuild-gt plugin, so there is one
# build under one lock and not two builds racing into one output (claude-7fc;
# design: docs/plans/2026-09-23-install-gt-after-merge-design.md).
#
# Exit codes — callers map these, so change them only together with
# internal/cmd (post-merge hook) and plugins/rebuild-gt/run.sh:
#   0  installed, or nothing to do (the binary already contains the commit)
#   1  failed: build/install failed, or the smoke check failed (rolled back)
#   2  refused: mayor/rig dirty, off main, diverged, not forward, unknown commit
#   3  busy: the install lock or the container-gate slot was not free in time
# The last stdout line is always "install-gt: RESULT <event> <commit> <prev> <reason>".
#
# It never restarts the daemon: it writes daemon/restart-pending.json and the
# daemon exits for a launchd restart once nothing is in flight.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/$(basename "$0")"
# shellcheck source=lib/install-gt-lib.sh
. "$SCRIPT_DIR/lib/install-gt-lib.sh"

ORIG_ARGS=("$@")
SHA="" SOURCE="" SLOT_ROLE="" SLOT_TIMEOUT=600
while [ $# -gt 0 ]; do
  case "$1" in
    --sha) SHA="${2:-}"; shift 2 ;;
    --source) SOURCE="${2:-}"; shift 2 ;;
    --slot-role) SLOT_ROLE="${2:-}"; shift 2 ;;
    --slot-timeout) SLOT_TIMEOUT="${2:-}"; shift 2 ;;
    *) echo "install-gt: unknown argument: $1" >&2; exit 1 ;;
  esac
done
case "$SOURCE" in post-merge|rebuild-gt) ;; *) echo "install-gt: --source must be post-merge or rebuild-gt" >&2; exit 1 ;; esac
[ -n "$SHA" ] || { echo "install-gt: --sha is required" >&2; exit 1; }

TOWN_ROOT="${GT_TOWN_ROOT:-}"
BIN_DIR="${INSTALL_GT_BIN_DIR:-$HOME/.local/bin}"
DAEMON_DIR="${INSTALL_GT_DAEMON_DIR:-${TOWN_ROOT:?install-gt: GT_TOWN_ROOT or INSTALL_GT_DAEMON_DIR must be set}/daemon}"
RIG_DIR="${INSTALL_GT_RIG_DIR:-${TOWN_ROOT:?install-gt: GT_TOWN_ROOT or INSTALL_GT_RIG_DIR must be set}/gastown/mayor/rig}"
LOCK_WAIT="${INSTALL_GT_LOCK_WAIT:-300}"
GT="$BIN_DIR/gt"
START=$(date +%s)
log() { echo "[install-gt] $*"; }

# escalate SEVERITY FINGERPRINT MESSAGE — through whatever gt is installed at
# that moment (after a rollback, the restored one).
escalate() {
  "$GT" escalate "install-gt: $3" -s "$1" --source "script:install-gt" --fingerprint "$2" >/dev/null 2>&1 \
    || log "WARNING: escalation $2 did not reach the town"
}

# --- Lock ---------------------------------------------------------------------
# flock(2) through perl (macOS has no flock(1)). The script re-execs itself with
# the lock fd inherited ($^F keeps it open across exec), so the kernel drops
# the lock if this process dies — a mkdir-lock would outlive a crash.
if [ -z "${INSTALL_GT_LOCKED:-}" ]; then
  mkdir -p "$DAEMON_DIR"
  rc=0
  INSTALL_GT_LOCKED=1 perl -e '
    use Fcntl qw(:flock);
    $^F = 255;
    my ($lock, $wait, @cmd) = @ARGV;
    open(my $fh, ">>", $lock) or die "install-gt: cannot open $lock: $!\n";
    my $deadline = time + $wait;
    until (flock($fh, LOCK_EX | LOCK_NB)) {
      exit 75 if time >= $deadline;
      select(undef, undef, undef, 0.5);
    }
    exec @cmd or die "install-gt: exec failed: $!\n";
  ' "$DAEMON_DIR/install-gt.lock" "$LOCK_WAIT" bash "$SCRIPT_PATH" "${ORIG_ARGS[@]}" || rc=$?
  if [ "$rc" = "75" ]; then
    log "Another install held the lock for ${LOCK_WAIT}s; not installing $SHA."
    igt_receipt refused "$SHA" "" lock-busy "" "$START"
    igt_result refused "$SHA" "" lock-busy
    exit 3
  fi
  exit "$rc"
fi

# refuse REASON MESSAGE — exit 2, binary untouched.
refuse() {
  log "Refused: $2"
  igt_receipt refused "${FULL_SHA:-$SHA}" "${PREV:-}" "$1" "${MERGED_AT:-}" "$START"
  igt_result refused "${FULL_SHA:-$SHA}" "${PREV:-}" "$1"
  exit 2
}

# --- Resolve the target ---------------------------------------------------------
[ -d "$RIG_DIR/.git" ] || [ -f "$RIG_DIR/.git" ] || { log "No build checkout at $RIG_DIR"; igt_result failed "$SHA" "" no-rig; exit 1; }
git -C "$RIG_DIR" fetch origin --quiet 2>/dev/null || log "WARNING: fetch failed; using local refs"
FULL_SHA=$(igt_resolve "$RIG_DIR" "$SHA")
[ -n "$FULL_SHA" ] || refuse unknown-commit "$SHA is not a commit in $RIG_DIR"
MERGED_AT=$(git -C "$RIG_DIR" log -1 --format=%ct "$FULL_SHA")
PREV=$(igt_resolve "$RIG_DIR" "$(igt_binary_commit_raw "$GT" "$RIG_DIR")")

# --- No-op: the installed binary already contains the commit ----------------------
if [ -n "$PREV" ] && git -C "$RIG_DIR" merge-base --is-ancestor "$FULL_SHA" "$PREV" 2>/dev/null; then
  log "Installed $PREV already contains $FULL_SHA; nothing to do."
  igt_receipt noop "$FULL_SHA" "$PREV" already-installed "$MERGED_AT" "$START"
  igt_result noop "$FULL_SHA" "$PREV" already-installed
  exit 0
fi

# --- Refusals (the rebuild-gt checks, plugins/rebuild-gt/run.sh:366-428) ------------
# Only tracked edits outside .beads/ change what 'make build' produces (gt-50k).
if [ -n "$(git -C "$RIG_DIR" status --porcelain --untracked-files=no -- . ':(exclude).beads' 2>/dev/null)" ]; then
  refuse dirty "$RIG_DIR has uncommitted changes"
fi
BRANCH=$(git -C "$RIG_DIR" branch --show-current 2>/dev/null || true)
[ "$BRANCH" = "main" ] || refuse wrong-branch "$RIG_DIR is on '$BRANCH', not main"
# ff-only, never reset: a real divergence is a human's call (gt-4g1m).
git -C "$RIG_DIR" merge --ff-only "$FULL_SHA" --quiet 2>/dev/null || refuse diverged "local main cannot fast-forward to $FULL_SHA"
if [ "$(git -C "$RIG_DIR" rev-list --count origin/main..HEAD 2>/dev/null || echo 0)" -gt 0 ]; then
  refuse diverged "local main has commits origin/main lacks"
fi
# HEAD can be past FULL_SHA when mayor/rig was already on a newer main; that is
# forward and contains the commit, so HEAD is what gets built and verified.
EXPECTED=$(git -C "$RIG_DIR" rev-parse HEAD)
if [ -n "$PREV" ] && ! git -C "$RIG_DIR" merge-base --is-ancestor "$PREV" "$EXPECTED" 2>/dev/null; then
  refuse not-forward "installed $PREV is not an ancestor of $EXPECTED (would be a downgrade)"
fi

# --- Keep the previous binary ------------------------------------------------------
if [ -f "$GT" ]; then
  cp -p "$GT" "$BIN_DIR/.gt.prev.$$"
  mv -f "$BIN_DIR/.gt.prev.$$" "$BIN_DIR/gt.prev"
fi

# rollback — put gt.prev back atomically; 0 on success.
rollback() {
  [ -f "$BIN_DIR/gt.prev" ] || return 1
  cp -p "$BIN_DIR/gt.prev" "$BIN_DIR/.gt.rollback.$$" 2>/dev/null || return 1
  mv -f "$BIN_DIR/.gt.rollback.$$" "$GT" 2>/dev/null || { rm -f "$BIN_DIR/.gt.rollback.$$"; return 1; }
}

# fail_install REASON MESSAGE SEVERITY FINGERPRINT EVENT — exit 1 after putting
# the old binary back when the installed file changed.
fail_install() {
  log "FAILED: $2"
  if [ -f "$BIN_DIR/gt.prev" ] && ! cmp -s "$BIN_DIR/gt.prev" "$GT"; then
    if rollback; then
      log "Rolled back to $PREV."
    else
      escalate critical install-gt:rollback-failed "$2; and restoring $BIN_DIR/gt.prev FAILED — the town may be running a broken gt"
      igt_receipt failed "$EXPECTED" "$PREV" rollback-failed "$MERGED_AT" "$START"
      igt_result failed "$EXPECTED" "$PREV" rollback-failed
      exit 1
    fi
  fi
  escalate "$3" "$4" "$2"
  igt_receipt "$5" "$EXPECTED" "$PREV" "$1" "$MERGED_AT" "$START"
  igt_result "$5" "$EXPECTED" "$PREV" "$1"
  exit 1
}

# --- Build and install -------------------------------------------------------------
log "Building $EXPECTED in $RIG_DIR (source: $SOURCE)"
if [ -n "$SLOT_ROLE" ]; then
  # Inside the container-gate slot, the way rebuild-gt builds past its
  # starvation threshold (gt-kox0). The acquired literal is slotAcquiredFormat
  # in internal/cmd/slot.go; without it the build never started.
  SLOT_LOG=$(mktemp)
  set +e
  (cd "$RIG_DIR" && "$GT" slot run --role "$SLOT_ROLE" --timeout "${SLOT_TIMEOUT}s" -- make SKIP_UPDATE_CHECK=1 build) 2>&1 | tee "$SLOT_LOG"
  BUILD_RC=${PIPESTATUS[0]}
  set -e
  if ! grep -q "Container-gate slot acquired" "$SLOT_LOG"; then
    rm -f "$SLOT_LOG"
    log "Did not get the container-gate slot within ${SLOT_TIMEOUT}s."
    igt_receipt refused "$EXPECTED" "$PREV" slot-busy "$MERGED_AT" "$START"
    igt_result refused "$EXPECTED" "$PREV" slot-busy
    exit 3
  fi
  rm -f "$SLOT_LOG"
else
  BUILD_RC=0
  (cd "$RIG_DIR" && make SKIP_UPDATE_CHECK=1 build) 2>&1 || BUILD_RC=$?
fi
[ "$BUILD_RC" = "0" ] || fail_install build-failed "make build failed for $EXPECTED" medium install-gt:build-failed failed

INSTALL_RC=0
INSTALL_OUT=$( (cd "$RIG_DIR" && make SKIP_UPDATE_CHECK=1 INSTALL_DIR="$BIN_DIR" safe-install) 2>&1 ) || INSTALL_RC=$?
echo "$INSTALL_OUT"
if [ "$INSTALL_RC" != "0" ]; then
  # check-forward-only exits 1 when the binary is already at HEAD — reachable
  # only when the installed commit could not be read above.
  if printf '%s\n' "$INSTALL_OUT" | grep -q "already at HEAD"; then
    igt_receipt noop "$EXPECTED" "$PREV" already-installed "$MERGED_AT" "$START"
    igt_result noop "$EXPECTED" "$PREV" already-installed
    exit 0
  fi
  fail_install build-failed "make safe-install failed for $EXPECTED" medium install-gt:build-failed failed
fi

# --- Smoke: the gt the town will execute is the commit built --------------------------
if ! (cd "$RIG_DIR" && "$GT" stale --json 2>/dev/null) | python3 -c 'import json,sys; json.load(sys.stdin)' >/dev/null 2>&1; then
  fail_install smoke-failed "the new binary cannot answer 'gt stale --json'" high install-gt:smoke-failed rolled_back
fi
GOT_RAW=$(igt_binary_commit_raw "$GT" "$RIG_DIR")
GOT=$(igt_resolve "$RIG_DIR" "$GOT_RAW")
if [ -z "$GOT" ]; then
  fail_install smoke-failed "installed a build but cannot verify what came into force (read '${GOT_RAW:-nothing}')" high install-gt:smoke-failed rolled_back
fi
if [ "$GOT" != "$EXPECTED" ]; then
  fail_install smoke-failed "the install did not take — $GT reports $GOT, built $EXPECTED" high install-gt:smoke-failed rolled_back
fi

# --- Syncs (non-fatal, as in rebuild-gt: convenience, not the install) ---------------
if OUT=$( (cd "$RIG_DIR" && "$GT" formula sync) 2>&1 ); then log "$OUT"; else log "formula sync failed (non-fatal): $OUT"; fi
if OUT=$( (cd "$RIG_DIR" && "$GT" plugin sync) 2>&1 ); then log "$OUT"; else log "plugin sync failed (non-fatal): $OUT"; fi

# --- Restart marker, receipt ---------------------------------------------------------
python3 - "$DAEMON_DIR/restart-pending.json" "$EXPECTED" "$SOURCE" "$(cd "$RIG_DIR" && pwd)" <<'PY'
import datetime, json, os, sys
path, commit, source, repo = sys.argv[1:5]
m = {"commit": commit,
     "requested_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
     "source": source,
     "repo": repo}
tmp = path + ".tmp.%d" % os.getpid()
with open(tmp, "w") as f:
    json.dump(m, f, indent=2, sort_keys=True)
os.replace(tmp, path)
PY
igt_receipt installed "$EXPECTED" "$PREV" "" "$MERGED_AT" "$START"
"$GT" escalate clear --fingerprint install-gt:build-failed --fingerprint install-gt:smoke-failed \
  --reason "install-gt: $EXPECTED is in force" >/dev/null 2>&1 || true
log "In force: ${PREV:-unknown} -> $EXPECTED. The daemon restarts itself when idle."
igt_result installed "$EXPECTED" "$PREV" ""

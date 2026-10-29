#!/usr/bin/env bash
# install-om.sh — build om from the job-owned clone and put it in force, the way
# the gt installer does for gt (gt-3imih). The rebuild_om daemon job runs it; it can
# also be run by hand.
#
#   install-om.sh [--om-path PATH] [--source manual|rebuild-om]
#
# It builds the commit checked out in <town>/.runtime/om-src (the job keeps that
# clone current; this script never fetches or moves it) with
#   go build -ldflags "-X main.version=<sha>" ./cmd/om
# and does not call om's own `make install`, whose deploy.sh drift gate needs
# TOWN and can refuse. PATH is the om gastown resolves: --om-path, else
# INSTALL_OM_PATH, else ~/go/bin/om. The town is GT_TOWN_ROOT, or else the
# outermost directory holding mayor/town.json above this script.
#
# Sequence, under one lock: build to a temp file beside PATH -> sign it ->
# copy PATH to om.prev -> rename the build over PATH -> smoke check (`om version`
# must print the built sha) -> receipt. A failed smoke check renames om.prev back.
# Any earlier failure leaves PATH untouched, and PATH only ever changes by an
# atomic rename: a half-written om never exists at PATH.
#
# Exit codes — the daemon job maps these, as it does for the gt installer:
#   0  installed, or nothing to do (the installed om already is the built sha)
#   1  failed: build or swap failed, or the smoke check failed (rolled back)
#   2  refused: no clone, clone dirty, or HEAD unreadable
#   3  busy: another install held the lock for the whole wait
# The last stdout line is always "install-om: RESULT <event> <commit> <prev> <reason>"
# (empty fields print as "-"), the field order the gt installer prints, and the exit
# code is always 0-3: an EXIT trap reports "failed ... unexpected" and exits 1 for
# any path nothing here anticipated.
#
# It never restarts anything: the landing worker execs om afresh for each review.
#
# Signing: before the swap, the build is signed with the town's stable identity
# (INSTALL_OM_SIGN_DIR, default <town>/.runtime/signing, the material the gt
# installer signs gt with) so a Developer Tools grant survives; without it, or if
# signing fails, the install goes ahead ad-hoc with one WARNING line.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/$(basename "$0")"
# Only igt_town_root_above and igt_resolve are used from the shared lib; the
# receipt and RESULT writers there are install-gt's, so om has its own below.
# shellcheck source=lib/install-gt-lib.sh
. "$SCRIPT_DIR/lib/install-gt-lib.sh"

log() { echo "[install-om] $*"; }

# om_result EVENT COMMIT PREV REASON — the machine-readable last line.
om_result() { echo "install-om: RESULT ${1:--} ${2:--} ${3:--} ${4:--}"; }

# om_receipt EVENT COMMIT PREV REASON — one line appended to
# $DAEMON_DIR/install-om-receipts.jsonl (install-gt's own file stays gt-only).
# Absent fields are JSON null. A failed write is logged, never fatal.
om_receipt() {
  mkdir -p "$DAEMON_DIR" 2>/dev/null || true
  python3 - "$DAEMON_DIR/install-om-receipts.jsonl" "$1" "${2:-}" "${3:-}" "${SOURCE:-}" "${4:-}" "${START:-}" <<'PY' || log "WARNING: could not append a receipt to $DAEMON_DIR/install-om-receipts.jsonl" >&2
import datetime, json, sys, time
path, event, commit, prev, source, reason, start = sys.argv[1:8]
now = time.time()
rec = {
    "ts": datetime.datetime.fromtimestamp(now, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "event": event,
    "commit": commit or None,
    "prev_commit": prev or None,
    "source": source or None,
    "reason": reason or None,
    "duration_s": int(now - int(start)) if start else None,
}
with open(path, "a") as f:
    f.write(json.dumps(rec, sort_keys=True) + "\n")
PY
}

# --- Exit contract -------------------------------------------------------------
# Every deliberate exit goes through result_line, so RESULT_EMITTED marks that the
# last-line contract was met; the EXIT trap covers everything else.
RESULT_EMITTED=0
result_line() { RESULT_EMITTED=1; om_result "$@"; }
on_exit() {
  local ec=$?
  [ "$RESULT_EMITTED" = "1" ] && exit "$ec"
  if [ -n "${DAEMON_DIR:-}" ]; then
    om_receipt failed "${EXPECTED:-}" "${PREV:-}" unexpected || true
  fi
  rm -f "${NEW_BIN:-}" 2>/dev/null || true
  om_result failed "${EXPECTED:-}" "${PREV:-}" unexpected
  exit 1
}
trap on_exit EXIT

ORIG_ARGS=("$@")
OM_PATH="${INSTALL_OM_PATH:-}" SOURCE="manual"
while [ $# -gt 0 ]; do
  case "$1" in
    --om-path) OM_PATH="${2:-}"; shift 2 ;;
    --source) SOURCE="${2:-}"; shift 2 ;;
    *) echo "install-om: unknown argument: $1" >&2; exit 1 ;;
  esac
done
case "$SOURCE" in manual|rebuild-om) ;; *) echo "install-om: --source must be manual or rebuild-om" >&2; exit 1 ;; esac
OM_PATH="${OM_PATH:-$HOME/go/bin/om}"

TOWN_ROOT="${GT_TOWN_ROOT:-$(igt_town_root_above "$SCRIPT_DIR")}"
SRC_DIR="${INSTALL_OM_SRC_DIR:-${TOWN_ROOT:?install-om: no town found (set GT_TOWN_ROOT)}/.runtime/om-src}"
DAEMON_DIR="${INSTALL_OM_DAEMON_DIR:-${TOWN_ROOT:?install-om: no town found (set GT_TOWN_ROOT)}/daemon}"
LOCK_WAIT="${INSTALL_OM_LOCK_WAIT:-300}"
OM_DIR="$(dirname "$OM_PATH")"
SIGN_DIR="${INSTALL_OM_SIGN_DIR:-$TOWN_ROOT/.runtime/signing}"
SIGN_KEYCHAIN="$SIGN_DIR/gastown-signing.keychain-db"
SIGN_PASSWORD_FILE="$SIGN_DIR/keychain.pass"
SIGN_IDENTITY="${INSTALL_OM_SIGN_IDENTITY:-gastown-dev-unattended}"
SIGN_IDENTIFIER="com.gastown.om"
SIGN_TIMEOUT="${INSTALL_OM_SIGN_TIMEOUT:-30}"
SMOKE_TIMEOUT="${INSTALL_OM_SMOKE_TIMEOUT:-30}"
START=$(date +%s)
NEW_BIN=""

# --- Lock ---------------------------------------------------------------------
# flock(2) through perl (macOS has no flock(1)), then the script re-execs itself
# with the lock fd inherited ($^F), so the kernel drops the lock if this process
# dies. The MARKER file tells the outer instance whether perl reached exec: perl
# unlinks it just before, so a surviving MARKER means perl died first and this
# instance's EXIT trap owes the fallback RESULT line. See cmd/gt-installer for
# the long form.
if [ -z "${INSTALL_OM_LOCKED:-}" ]; then
  mkdir -p "$DAEMON_DIR"
  MARKER="$DAEMON_DIR/.install-om.pre-exec.$$"
  : > "$MARKER"
  rc=0
  INSTALL_OM_LOCKED=1 perl -e '
    use Fcntl qw(:flock);
    $^F = 255;
    my ($lock, $wait, $marker, @cmd) = @ARGV;
    open(my $fh, ">>", $lock) or die "install-om: cannot open $lock: $!\n";
    my $deadline = time + $wait;
    until (flock($fh, LOCK_EX | LOCK_NB)) {
      exit 75 if time >= $deadline;
      select(undef, undef, undef, 0.5);
    }
    unlink($marker);
    exec @cmd or die "install-om: exec failed: $!\n";
  ' "$DAEMON_DIR/install-om.lock" "$LOCK_WAIT" "$MARKER" bash "$SCRIPT_PATH" ${ORIG_ARGS[@]+"${ORIG_ARGS[@]}"} || rc=$?
  if [ "$rc" = "75" ]; then
    rm -f "$MARKER"
    log "Another install held the lock for ${LOCK_WAIT}s; not installing."
    om_receipt refused "" "" lock-busy
    result_line refused "" "" lock-busy
    exit 3
  fi
  if [ -e "$MARKER" ]; then
    rm -f "$MARKER"
    exit "$rc"
  fi
  RESULT_EMITTED=1
  exit "$rc"
fi

# refuse REASON MESSAGE — exit 2, om untouched.
refuse() {
  log "Refused: $2"
  om_receipt refused "${EXPECTED:-}" "${PREV:-}" "$1"
  result_line refused "${EXPECTED:-}" "${PREV:-}" "$1"
  exit 2
}

# code_timeout SECONDS CMD... — GNU timeout(1) when on PATH (coreutils, or brew's
# gtimeout); where neither exists the command runs unguarded.
code_timeout() {
  local secs="$1"; shift
  if command -v timeout >/dev/null 2>&1; then timeout "$secs" "$@"
  elif command -v gtimeout >/dev/null 2>&1; then gtimeout "$secs" "$@"
  else "$@"
  fi
}

# om_version_sha OM — the sha on OM's "om version <sha>" line, or empty.
om_version_sha() {
  [ -x "$1" ] || { echo ""; return 0; }
  code_timeout "$SMOKE_TIMEOUT" "$1" version 2>/dev/null | python3 -c '
import re, sys
for line in sys.stdin:
    m = re.match(r"\s*om version ([0-9a-f]{7,40})(\s|$)", line)
    if m:
        print(m.group(1))
        break
' 2>/dev/null || true
}

# --- Resolve the target ---------------------------------------------------------
[ -d "$SRC_DIR/.git" ] || [ -f "$SRC_DIR/.git" ] || refuse no-source "no om clone at $SRC_DIR"
EXPECTED=$(igt_resolve "$SRC_DIR" HEAD)
[ -n "$EXPECTED" ] || refuse no-source "$SRC_DIR has no readable HEAD"
# Only tracked edits change what the build produces; the sha stamped into the
# binary must describe what was compiled.
if [ -n "$(git -C "$SRC_DIR" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
  refuse dirty "$SRC_DIR has uncommitted changes"
fi
PREV_RAW=$(om_version_sha "$OM_PATH")
PREV=$(igt_resolve "$SRC_DIR" "$PREV_RAW")
PREV="${PREV:-$PREV_RAW}"

# --- No-op: the installed om already is this commit ------------------------------
if [ -n "$PREV" ] && [ "$PREV" = "$EXPECTED" ]; then
  log "Installed om is already $EXPECTED; nothing to do."
  om_receipt noop "$EXPECTED" "$PREV" already-installed
  result_line noop "$EXPECTED" "$PREV" already-installed
  exit 0
fi

# fail_install REASON MESSAGE EVENT — exit 1; PATH is left (or put back) as it was.
fail_install() {
  log "FAILED: $2"
  rm -f "$NEW_BIN" 2>/dev/null || true
  om_receipt "$3" "$EXPECTED" "$PREV" "$1"
  result_line "$3" "$EXPECTED" "$PREV" "$1"
  exit 1
}

# --- Build to a temp file beside OM_PATH, same filesystem so the swap is a rename
mkdir -p "$OM_DIR" || fail_install build-failed "cannot create $OM_DIR" failed
NEW_BIN=$(mktemp "$OM_DIR/.om.new.XXXXXX") || fail_install build-failed "cannot create a temp file in $OM_DIR" failed
log "Building $EXPECTED in $SRC_DIR"
BUILD_RC=0
(cd "$SRC_DIR" && go build -ldflags "-X main.version=$EXPECTED" -o "$NEW_BIN" ./cmd/om) 2>&1 || BUILD_RC=$?
[ "$BUILD_RC" = "0" ] || fail_install build-failed "go build failed for $EXPECTED" failed
chmod 0755 "$NEW_BIN" || fail_install build-failed "cannot make $NEW_BIN executable" failed

# --- Signing (best-effort, on the temp file) -------------------------------------
# Signing the build before it is renamed in means the live om is never rewritten
# in place and never half-signed; every failure leaves the unsigned build to go
# in ad-hoc, with one WARNING.
sign_binary() {
  local bin="$1" pw msg rc=0
  [ -f "$SIGN_KEYCHAIN" ] || { log "WARNING: installing ad-hoc: no keychain at $SIGN_KEYCHAIN"; return 0; }
  [ -f "$SIGN_PASSWORD_FILE" ] || { log "WARNING: installing ad-hoc: no password file at $SIGN_PASSWORD_FILE"; return 0; }
  pw=$(cat "$SIGN_PASSWORD_FILE") || { log "WARNING: installing ad-hoc: cannot read $SIGN_PASSWORD_FILE"; return 0; }
  # The password reaches this one call and is never logged or handed to codesign.
  if ! security unlock-keychain -p "$pw" "$SIGN_KEYCHAIN" >/dev/null 2>&1; then
    log "WARNING: installing ad-hoc: cannot unlock $SIGN_KEYCHAIN"
    return 0
  fi
  msg=$(code_timeout "$SIGN_TIMEOUT" codesign --force --keychain "$SIGN_KEYCHAIN" -s "$SIGN_IDENTITY" \
          -i "$SIGN_IDENTIFIER" "$bin" </dev/null 2>&1) || rc=$?
  if [ "$rc" != "0" ]; then
    log "WARNING: installing ad-hoc: codesign failed: $(printf '%s' "${msg:-no output}" | head -1)"
    return 0
  fi
  log "Signed the build as $SIGN_IDENTIFIER"
}
sign_binary "$NEW_BIN"

# --- Keep the previous om, then swap ----------------------------------------------
if [ -f "$OM_PATH" ]; then
  cp -p "$OM_PATH" "$OM_DIR/.om.prev.$$" && mv -f "$OM_DIR/.om.prev.$$" "$OM_DIR/om.prev" \
    || { rm -f "$OM_DIR/.om.prev.$$"; fail_install backup-failed "cannot keep $OM_PATH as om.prev" failed; }
fi
mv -f "$NEW_BIN" "$OM_PATH" || fail_install swap-failed "cannot rename the build over $OM_PATH" failed
NEW_BIN=""

# rollback — put om.prev back atomically (or, on a first install, remove the bad
# om so nothing broken is left in force); 0 on success.
rollback() {
  if [ ! -f "$OM_DIR/om.prev" ]; then
    rm -f "$OM_PATH"
    return 0
  fi
  cp -p "$OM_DIR/om.prev" "$OM_DIR/.om.rollback.$$" 2>/dev/null || return 1
  mv -f "$OM_DIR/.om.rollback.$$" "$OM_PATH" 2>/dev/null || { rm -f "$OM_DIR/.om.rollback.$$"; return 1; }
}

# smoke_failed MESSAGE — the swap already happened, so put the old om back.
smoke_failed() {
  log "FAILED: $1"
  if rollback; then
    log "Rolled back to ${PREV:-no om}."
    om_receipt rolled_back "$EXPECTED" "$PREV" smoke-failed
    result_line rolled_back "$EXPECTED" "$PREV" smoke-failed
  else
    log "CRITICAL: restoring $OM_DIR/om.prev FAILED — $OM_PATH may be a broken om"
    om_receipt failed "$EXPECTED" "$PREV" rollback-failed
    result_line failed "$EXPECTED" "$PREV" rollback-failed
  fi
  exit 1
}

# --- Smoke: the om gastown will exec is the commit built ---------------------------
GOT=$(om_version_sha "$OM_PATH")
[ -n "$GOT" ] || smoke_failed "the new om printed no \"om version <sha>\" line"
# om stamps the full sha; a short one still has to be a prefix of the built commit.
case "$EXPECTED" in
  "$GOT"*) ;;
  *) smoke_failed "the install did not take — $OM_PATH reports $GOT, built $EXPECTED" ;;
esac

om_receipt installed "$EXPECTED" "$PREV" ""
log "In force: ${PREV:-none} -> $EXPECTED."
result_line installed "$EXPECTED" "$PREV" ""

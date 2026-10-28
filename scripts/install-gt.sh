#!/usr/bin/env bash
# install-gt.sh — THE way to install gt into a running town (gt-z0l3s). Run it
# with no arguments (or `make install`) from any gastown clone: it installs
# origin/main. The rebuild-gt plugin calls it with --source rebuild-gt; a
# landing's post_land_command may call it with --source post-land. There is
# one build under one lock, never two builds racing into one output
# (claude-7fc; design: docs/plans/2026-09-23-install-gt-after-merge-design.md).
#
# The sandbox image is built and published for the target commit before the
# binary swap, and a build that does not finish fails the install with the old
# binary still in place (gt-xzo8a). --no-image-first keeps the old order for an
# urgent daemon fix: the binary is swapped without waiting, and the image is
# rebuilt afterwards, detached and best effort (gt-vhn89).
#
#   install-gt.sh [--sha REF] [--source manual|rebuild-gt|post-land]
#                 [--slot-role ROLE] [--slot-timeout SECONDS] [--dry-run]
#                 [--no-image-first]
#
# --dry-run builds and installs nothing and takes no lock: it prints the
# launchd actions an install would take for the OpenShell gateway agent and
# exits 0 (docs/openshell-gateway.md).
#
# --sha defaults to origin/main, resolved after this script's own fetch.
# It always builds in <town>/gastown/mayor/rig (fast-forwarded to the target),
# whichever clone it is run from. The town is GT_TOWN_ROOT, or else the
# outermost directory holding mayor/town.json above this script.
#
# Exit codes — callers map these, so change them only together with
# plugins/rebuild-gt/run.sh:
#   0  installed, or nothing to do (the binary already contains the commit)
#   1  failed: build/install failed, or the smoke check failed (rolled back)
#   2  refused: mayor/rig dirty, off main, diverged, not forward, unknown commit
#   3  busy: the install lock or the container-gate slot was not free in time
# The last stdout line is "install-gt: RESULT <event> <commit> <prev> <reason>",
# and the exit status is one of the four above — an EXIT trap (below) guarantees
# both even on a path nothing here anticipated (a bad flag, a "${VAR:?}" guard,
# the install-lock helper dying for a reason other than the timeout, cp/mv of
# gt.prev failing under `set -e`, ...): it reports "failed ... unexpected" and
# exits 1. An install killed by a signal is the one case no trap covers — bash
# runs no EXIT trap then — so its status is 128+n and no RESULT line is printed.
#
# Besides the binary it installs the OpenShell gateway's launchd agent
# (igt_gateway_agent in lib/install-gt-lib.sh) once the operator has written
# ~/.config/openshell/gateway.toml: bootstrap if launchd has not loaded it,
# never kickstart.
#
# It never restarts the daemon: it writes daemon/restart-pending.json and the
# daemon exits for a launchd restart once nothing is in flight.
#
# After install-local has installed the new binary, and before the smoke check
# runs it, that binary is signed with the town's stable identity (gt-426fo.1)
# so a Developer Tools grant survives every install. SIGN_DIR below holds the
# material it signs with and the ad-hoc fallback when it cannot.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/$(basename "$0")"
# shellcheck source=lib/install-gt-lib.sh
. "$SCRIPT_DIR/lib/install-gt-lib.sh"

# --- Exit contract -------------------------------------------------------------
# Every deliberate exit below goes through result_line (never igt_result
# directly), so RESULT_EMITTED marks that the last-line contract was met. The
# EXIT trap catches everything else — including exit codes bash's own guards
# produce (a `${TOWN_ROOT:?...}` abort, `set -e` killing the script mid cp/mv
# or mid python-write) and an install-lock exit status that was never meant to
# be a contract value (only 75, the lock-wait timeout, is; anything else the
# helper returns — e.g. it could not open the lock file — must not leak through
# as a bare exit code outside the range the header states). It is intentionally
# conservative: it does not call escalate() (GT/BIN_DIR may not be resolved yet
# this early) and only attempts a receipt when DAEMON_DIR is already known.
RESULT_EMITTED=0
HELPER_DIR=""
IMAGE_OWNED=""
result_line() { RESULT_EMITTED=1; igt_result "$@"; }
on_exit() {
  local ec=$?
  # A detached image build is started from HELPER_DIR and removes it itself
  # (--owned), so this exit leaves it alone (gt-p5pjs).
  [ -z "$HELPER_DIR" ] || [ -n "$IMAGE_OWNED" ] || rm -rf "$HELPER_DIR"
  [ "$RESULT_EMITTED" = "1" ] && exit "$ec"
  if [ -n "${DAEMON_DIR:-}" ]; then
    igt_receipt failed "${EXPECTED:-${FULL_SHA:-${SHA:-}}}" "${PREV:-}" unexpected "${MERGED_AT:-}" "${START:-}" || true
  fi
  igt_result failed "${EXPECTED:-${FULL_SHA:-${SHA:-}}}" "${PREV:-}" unexpected
  exit 1
}
trap on_exit EXIT

ORIG_ARGS=("$@")
SHA="origin/main" SOURCE="manual" SLOT_ROLE="" SLOT_TIMEOUT=600 DRY_RUN=0 IMAGE_FIRST=1
while [ $# -gt 0 ]; do
  case "$1" in
    --sha) SHA="${2:-}"; shift 2 ;;
    --source) SOURCE="${2:-}"; shift 2 ;;
    --slot-role) SLOT_ROLE="${2:-}"; shift 2 ;;
    --slot-timeout) SLOT_TIMEOUT="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --no-image-first) IMAGE_FIRST=0; shift ;;
    *) echo "install-gt: unknown argument: $1" >&2; exit 1 ;;
  esac
done
case "$SOURCE" in manual|rebuild-gt|post-land) ;; *) echo "install-gt: --source must be manual, rebuild-gt or post-land" >&2; exit 1 ;; esac
[ -n "$SHA" ] || { echo "install-gt: --sha needs a commit" >&2; exit 1; }

# The town root comes from the shared helper in lib/install-gt-lib.sh, so
# install and uninstall agree on what a town is (gt-acdfp).
TOWN_ROOT="${GT_TOWN_ROOT:-$(igt_town_root_above "$SCRIPT_DIR")}"
# INSTALL_DIR is honored too: the Makefile's name for this directory, and the
# one the dispatcher's host-safety prompt names, so a test that follows the
# prompt installs into its temp dir (gt-tcigd). INSTALL_GT_BIN_DIR wins.
BIN_DIR="${INSTALL_GT_BIN_DIR:-${INSTALL_DIR:-$HOME/.local/bin}}"
DAEMON_DIR="${INSTALL_GT_DAEMON_DIR:-${TOWN_ROOT:?install-gt: no town found (set GT_TOWN_ROOT); on a machine with no town yet use make install-local}/daemon}"
RIG_DIR="${INSTALL_GT_RIG_DIR:-${TOWN_ROOT:?install-gt: no town found (set GT_TOWN_ROOT); on a machine with no town yet use make install-local}/gastown/mayor/rig}"
GT="$BIN_DIR/gt"
# The town's signing material: a keychain and, beside it, the 0600 file holding
# its password. A Developer Tools grant is keyed on the *designated requirement*
# — the identifier below, not the binary's hash — which is why signing with one
# stable identity is what makes the operator's grant survive an install
# (gt-426fo.1). None of it is required: with no material, or if signing fails,
# the install proceeds ad-hoc with one WARN line.
SIGN_DIR="${INSTALL_GT_SIGN_DIR:-$TOWN_ROOT/.runtime/signing}"
SIGN_KEYCHAIN="$SIGN_DIR/gastown-signing.keychain-db"
SIGN_PASSWORD_FILE="$SIGN_DIR/keychain.pass"
SIGN_IDENTITY="${INSTALL_GT_SIGN_IDENTITY:-gastown-dev-unattended}"
SIGN_IDENTIFIER="com.gastown.gt"
SIGN_TIMEOUT="${INSTALL_GT_SIGN_TIMEOUT:-30}"
# Every gt this script runs is the installer's unless the caller already named
# an actor (rebuild-gt runs as the daemon), so the usage log and escalations
# never read "unknown" (gt-kyik6).
export BD_ACTOR="${BD_ACTOR:-installer}"
START=$(date +%s)
log() { echo "[install-gt] $*"; }

# escalate SEVERITY FINGERPRINT MESSAGE — through whatever gt is installed at
# that moment (after a rollback, the restored one).
escalate() {
  "$GT" escalate "install-gt: $3" -s "$1" --source "script:install-gt" --fingerprint "$2" >/dev/null 2>&1 \
    || log "WARNING: escalation $2 did not reach the town"
}

# --- OpenShell gateway agent ---------------------------------------------------
# The template comes from the checkout being installed (RIG_DIR, fast-forwarded
# by the time a build runs), else from this script's own clone. A failure here
# never fails an install: the gateway is independent of the gt binary.
#
# The roots are arguments, not ambient globals: --dry-run exits before the lock
# block, so its call site must already hold paths resolved above it (gt-ky98c).
GATEWAY_HOME="${INSTALL_GT_GATEWAY_HOME:-$HOME}"
gateway_step() {
  local mode="$1" rig="$2" here="$3" line tmpl
  tmpl="$rig/scripts/launchd/com.gastown.openshell-gateway.plist.tmpl"
  [ -f "$tmpl" ] || tmpl="$here/launchd/com.gastown.openshell-gateway.plist.tmpl"
  igt_gateway_agent "$mode" "$tmpl" "$GATEWAY_HOME" | while IFS= read -r line; do log "$line"; done || true
}

# polecat_image_step RUN LOG BINARY COMMIT [before|after] builds and publishes
# the sandbox polecat image at COMMIT. "before" (the default) waits for the
# build and returns non-zero when it did not finish; it runs before install-local
# swaps the binary, so a failed build leaves the old binary in place: a sling is
# refused while the image carries another commit, and the plugin's own cooldown
# can hold that window for 10 minutes after an install (gt-pmxo9, gt-xzo8a).
# "after" is the old order --no-image-first keeps for an urgent daemon fix: the
# build is started detached, the install says where its output goes, and the
# function returns 0, so the already-swapped binary is neither held nor rolled
# back for the image's sake (gt-vhn89); there the plugin's deferrals — docker
# down, a sling running, another run holding its lock — exit 0 and let the
# install through, since blocking on an in-flight sling would starve the install
# path this step exists to keep free.
#
# The pre-swap build gets POLECAT_IMAGE_FAIL_CLOSED=1 (gt-efupr); before changing
# what it makes fail, read plugin.md, "Publishing to a registry", since the
# install stands behind every failure the switch raises. The detached build
# leaves it unset.
#
# BINARY is the build output, never the installed $GT: the plugin reads the
# commit to build from `gt version`, and before the swap $GT still reports the
# old commit.
#
# The plugin is the rig checkout's, which this install just built: its run.sh
# execs plugins/polecat-image/bin/polecat-image, a binary only `make build`
# writes, and the town's runtime copy is not synced until further down. Running
# that copy here would exec a binary it does not have yet on the first install
# after the port (gt-tpg0p.7).
IMAGE_RUN="${INSTALL_GT_IMAGE_RUN:-${RIG_DIR:+$RIG_DIR/plugins/polecat-image/run.sh}}"
# What `make build` leaves in RIG_DIR and install-local copies from.
IMAGE_BIN="${INSTALL_GT_IMAGE_BINARY:-$RIG_DIR/gt}"
IMAGE_LOG="${INSTALL_GT_IMAGE_LOG:-${TOWN_ROOT:+$TOWN_ROOT/.runtime/polecat-image-install.log}}"
IMAGE_LOCK="${INSTALL_GT_IMAGE_LOCK:-${TOWN_ROOT:+$TOWN_ROOT/.runtime/polecat-image.lock}}"
IMAGE_ALARM="${INSTALL_GT_IMAGE_ALARM:-1200}"
# The image build holds the install lock for up to the alarm bound (gt-xzo8a,
# hq-ltx66), so a queued install waits for the whole build: the default wait is
# the bound plus a minute. INSTALL_GT_LOCK_WAIT, when set, is used as given.
lock_floor=1200
if [[ "$IMAGE_ALARM" =~ ^[1-9][0-9]*$ ]]; then lock_floor=$IMAGE_ALARM; fi
LOCK_WAIT="${INSTALL_GT_LOCK_WAIT:-$((lock_floor + 60))}"
# image_daemon_env KEY: sets IMAGE_ENV_VALUE to daemon.env.KEY in
# settings/config.json (the one source the daemon gives its plugin runs, gt-jmd9n)
# and IMAGE_ENV_REASON to why it is empty. Each failure has its own reason, so the
# log says why a build is local-only: no town root, no config file, no jq, an
# unreadable or malformed config, no daemon.env object, or no such key. Nothing here
# is fatal; the build then runs without the registry, as it always has.
image_daemon_env() {
  IMAGE_ENV_VALUE=""
  IMAGE_ENV_REASON=""
  if [ -z "${TOWN_ROOT:-}" ]; then
    IMAGE_ENV_REASON="no town root"
    return 0
  fi
  local cfg="$TOWN_ROOT/settings/config.json"
  if [ ! -f "$cfg" ]; then
    IMAGE_ENV_REASON="$cfg is missing"
    return 0
  fi
  if ! command -v jq >/dev/null 2>&1; then
    IMAGE_ENV_REASON="jq is not installed, so $cfg cannot be read"
    return 0
  fi
  local kind
  if ! kind=$(jq -r '.daemon.env | type' "$cfg" 2>/dev/null); then
    IMAGE_ENV_REASON="$cfg is not readable JSON"
    return 0
  fi
  case "$kind" in
    object) ;;
    null) IMAGE_ENV_REASON="$cfg has no daemon.env"; return 0 ;;
    *) IMAGE_ENV_REASON="daemon.env in $cfg is a $kind, not an object"; return 0 ;;
  esac
  local value
  if ! value=$(jq -r --arg k "$1" '.daemon.env[$k] // empty' "$cfg" 2>/dev/null); then
    IMAGE_ENV_REASON="$cfg is not readable JSON"
    return 0
  fi
  if [ -z "$value" ]; then
    IMAGE_ENV_REASON="no $1 in daemon.env of $cfg"
    return 0
  fi
  IMAGE_ENV_VALUE="$value"
}

# image_build SECS LOCK BINARY RUN [FINGERPRINT] — run.sh under cmd/image-supervisor,
# built by image_supervisor_prepare. What the supervisor does with the alarm, the
# lock and the fingerprint is stated once, in internal/imagebuild. Call this in a
# subshell, and let it exec: the supervisor closes every fd it inherits, the
# install lock among them, and a bash left waiting on it would keep that fd open.
#
# It exports the registry and crane it was given and points run.sh at this
# install's town root, the binary whose commit the image is built at, and its
# lock (POLECAT_IMAGE_TOWN_ROOT, POLECAT_IMAGE_GT, POLECAT_IMAGE_LOCK; case 43
# pins them). HOME and PATH are not used, so the image is built for the install
# that asked for it.
image_build() {
  local secs="$1" lock="$2" bin="$3" run="$4" fingerprint="${5:-}"
  local report=()
  if [ -n "$fingerprint" ]; then report=(--gt "$GT" --fingerprint "$fingerprint"); fi
  export POLECAT_IMAGE_TOWN_ROOT="$TOWN_ROOT" POLECAT_IMAGE_GT="$bin" POLECAT_IMAGE_LOCK="$lock"
  if [ -n "$image_registry" ]; then export POLECAT_IMAGE_REGISTRY="$image_registry"; fi
  if [ -n "$image_crane" ]; then export POLECAT_IMAGE_CRANE="$image_crane"; fi
  exec "$IMAGE_SUPERVISOR" --alarm "$secs" --lock "$lock" ${report[@]+"${report[@]}"} --owned "$IMAGE_OWNED" -- bash "$run"
}

# image_supervisor_prepare — build cmd/image-supervisor from this checkout into a
# temp dir (HELPER_DIR, removed on exit unless a detached build owns it) and set
# IMAGE_SUPERVISOR to it, or set
# IMAGE_SUPERVISOR_WHY to why it could not be. Like the install lock, it comes
# from the checkout because an install can run before any gt exists, and the Go
# toolchain is already required: 'make build' needs it. There is no shell
# fallback.
image_supervisor_prepare() {
  IMAGE_SUPERVISOR=""
  IMAGE_SUPERVISOR_WHY=""
  if [ -z "$HELPER_DIR" ] && ! HELPER_DIR=$(mktemp -d "${TMPDIR:-/tmp}/install-gt-image.XXXXXX"); then
    HELPER_DIR=""
    IMAGE_SUPERVISOR_WHY="cannot make a temp directory"
    return 1
  fi
  local out
  if ! out=$(cd "$SCRIPT_DIR/.." && CGO_ENABLED=0 go build -buildvcs=false -o "$HELPER_DIR/image-supervisor" ./cmd/image-supervisor 2>&1); then
    IMAGE_SUPERVISOR_WHY="go build ./cmd/image-supervisor in $SCRIPT_DIR failed: $(printf '%s' "$out" | tail -n 1)"
    return 1
  fi
  IMAGE_SUPERVISOR="$HELPER_DIR/image-supervisor"
}

polecat_image_step() {
  local run="$1" logfile="$2" bin="$3" commit="$4"
  # "before" fails the install closed; "after" is detached and best effort, so
  # every preflight failure below is a log line and a return 0 on that path.
  local when="${5:-before}" order="before" consequence="the binary was not swapped" fail_rc=1
  if [ "$when" = "after" ]; then
    order="after"; consequence="the binary was already swapped"; fail_rc=0
  fi
  local image_registry image_crane
  image_daemon_env POLECAT_IMAGE_REGISTRY
  image_registry="$IMAGE_ENV_VALUE"
  if [ -z "$image_registry" ]; then
    log "polecat image: build is local only and not published: $IMAGE_ENV_REASON"
  fi
  image_daemon_env POLECAT_IMAGE_CRANE
  image_crane="$IMAGE_ENV_VALUE"
  if [ -z "$run" ] || [ ! -f "$run" ]; then
    log "polecat image: no plugin at '${run:-?}', so the image cannot be built $order the swap; $consequence"
    return "$fail_rc"
  fi
  # Without the build output run.sh would read the old commit from $GT and build
  # an image for the binary this install is about to replace.
  if [ ! -x "$bin" ]; then
    log "polecat image: no build output at $bin, so the image was not built"
    return "$fail_rc"
  fi
  if ! image_supervisor_prepare; then
    log "polecat image: $IMAGE_SUPERVISOR_WHY, so the image cannot be built $order the swap; $consequence"
    return "$fail_rc"
  fi
  # An alarm of 0 or a non-number would leave the build with no bound at all.
  local alarm="$IMAGE_ALARM"
  if ! [[ "$alarm" =~ ^[1-9][0-9]*$ ]]; then
    log "polecat image: INSTALL_GT_IMAGE_ALARM='$alarm' is not a whole number of seconds above zero, so the build gets 1200"
    alarm=1200
  fi
  [ -n "$logfile" ] || logfile=/dev/null
  mkdir -p "$(dirname "$logfile")" 2>/dev/null || true
  log "polecat image: building $commit $order the binary swap (log $logfile)"
  if [ "$when" = "after" ]; then
    # Detached, as before gt-xzo8a: the subshell exits at once, so the install
    # never waits on the build, and image_build's fd closes keep the install
    # lock out of it.
    # The build's supervisor removes HELPER_DIR when its build ends (--owned),
    # so this install's exit must not: removing it here raced the supervisor's
    # exec from HELPER_DIR and could lose (gt-p5pjs). A launch that dies before
    # the exec leaves the directory behind, the one case no supervisor can clean.
    IMAGE_OWNED="$HELPER_DIR"
    ( image_build "$alarm" "$IMAGE_LOCK" "$bin" "$run" install-gt:image-build-failed </dev/null >>"$logfile" 2>&1 & ) 2>/dev/null
    log "polecat image: rebuild for $commit started in the background (log $logfile)"
    return 0
  fi
  local rc=0
  ( export POLECAT_IMAGE_FAIL_CLOSED=1
    image_build "$alarm" "$IMAGE_LOCK" "$bin" "$run" ) </dev/null >>"$logfile" 2>&1 || rc=$?
  if [ "$rc" = "0" ]; then
    log "polecat image: in force at $commit"
    return 0
  fi
  log "polecat image: the build for $commit did not finish (exit $rc); $consequence"
  if [ "$logfile" != /dev/null ]; then
    tail -n 5 "$logfile" 2>/dev/null | while IFS= read -r line; do log "polecat image: $line"; done || true
  fi
  return 1
}

if [ "$DRY_RUN" = "1" ]; then
  gateway_step dry-run "$RIG_DIR" "$SCRIPT_DIR"
  result_line dry-run "$SHA" "" ""
  exit 0
fi

# --- Lock ---------------------------------------------------------------------
# flock(2) through the Go helper in cmd/install-lock (macOS has no flock(1)).
# The helper takes the lock, keeps its fd open across exec, and then execs the
# re-exec'd script itself, so the process holding the lock IS the install and
# the lock lives exactly as long as the install does. The kernel drops it if
# that process dies, which a mkdir-lock would not (gt-1jgci). Nothing runs
# beside the install that could outlive it.
#
# The lock fd is inherited by every descendant of the install, not only the
# install. The ones that can outlive it are handled: git runs with auto
# maintenance off (GIT_NO_AUTO_GC below), and the image-build supervisor closes
# every inherited fd, so neither can hold the lock past the install.
#
# The helper is built from this checkout, because an install can run before any
# gt exists and this clone is then the only source there is. The Go toolchain is
# already required: 'make build' below needs it. There is no shell fallback.
#
# The exec'd install is the same process the helper was, but it is a separate
# bash from this one, so its RESULT_EMITTED is invisible here. A MARKER file
# tells this instance which happened: the helper removes it just before the
# exec, so if it still exists once the helper returns, the exec was never
# reached (the lock file could not be opened, bash could not be started, ...)
# and this instance's own EXIT trap should print the fallback RESULT line below;
# if it is gone, the install ran and already printed its own — RESULT_EMITTED is
# set here too, purely to stop this instance's trap from printing a second one
# on top of it. The status is the install's own (the ranges are in the header),
# so 75 can only be the helper reporting that the lock wait ran out.
if [ -z "${INSTALL_GT_LOCKED:-}" ]; then
  mkdir -p "$DAEMON_DIR"
  HELPER_DIR=$(mktemp -d "${TMPDIR:-/tmp}/install-gt-lock.XXXXXX")
  if ! (cd "$SCRIPT_DIR/.." && CGO_ENABLED=0 go build -buildvcs=false -o "$HELPER_DIR/install-lock" ./cmd/install-lock); then
    log "Cannot build cmd/install-lock in $SCRIPT_DIR; the install lock is unavailable."
    exit 1
  fi
  MARKER="$DAEMON_DIR/.install-gt.pre-exec.$$"
  : > "$MARKER"
  rc=0
  INSTALL_GT_LOCKED=1 "$HELPER_DIR/install-lock" \
    --lock "$DAEMON_DIR/install-gt.lock" --wait "$LOCK_WAIT" --marker "$MARKER" \
    -- bash "$SCRIPT_PATH" ${ORIG_ARGS[@]+"${ORIG_ARGS[@]}"} || rc=$?
  if [ "$rc" = "75" ]; then
    rm -f "$MARKER"
    log "Another install held the lock for ${LOCK_WAIT}s; not installing $SHA."
    igt_receipt refused "$SHA" "" lock-busy "" "$START"
    result_line refused "$SHA" "" lock-busy
    exit 3
  fi
  if [ -e "$MARKER" ]; then
    rm -f "$MARKER"
    exit "$rc"
  fi
  RESULT_EMITTED=1
  exit "$rc"
fi

# refuse REASON MESSAGE — exit 2, binary untouched.
refuse() {
  log "Refused: $2"
  igt_receipt refused "${FULL_SHA:-$SHA}" "${PREV:-}" "$1" "${MERGED_AT:-}" "$START"
  result_line refused "${FULL_SHA:-$SHA}" "${PREV:-}" "$1"
  exit 2
}

# write_restart_marker COMMIT — write daemon/restart-pending.json for COMMIT,
# the marker the daemon restarts into. 0 once the marker is in place, non-zero
# when the write failed; each caller decides what that means on its own path
# (gt-tqxdd).
write_restart_marker() {
  python3 - "$DAEMON_DIR/restart-pending.json" "$1" "$SOURCE" "$(cd "$RIG_DIR" && pwd)" <<'PY'
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
}

# git fetch/merge can fork a detached 'gc --auto' or 'maintenance --auto' that
# inherits the lock fd (the install-lock helper leaves it open across exec) and
# would hold the install lock long after this run ends; rebuild-gt closes fd 9
# for the same reason. Disable auto maintenance on the calls that can trigger it.
GIT_NO_AUTO_GC=(-c gc.auto=0 -c maintenance.auto=false)

# --- Resolve the target ---------------------------------------------------------
[ -d "$RIG_DIR/.git" ] || [ -f "$RIG_DIR/.git" ] || { log "No build checkout at $RIG_DIR"; result_line failed "$SHA" "" no-rig; exit 1; }
git -C "$RIG_DIR" "${GIT_NO_AUTO_GC[@]}" fetch origin --quiet 2>/dev/null || log "WARNING: fetch failed; using local refs"
FULL_SHA=$(igt_resolve "$RIG_DIR" "$SHA")
[ -n "$FULL_SHA" ] || refuse unknown-commit "$SHA is not a commit in $RIG_DIR"
MERGED_AT=$(git -C "$RIG_DIR" log -1 --format=%ct "$FULL_SHA")
PREV=$(igt_resolve "$RIG_DIR" "$(igt_binary_commit_raw "$GT" "$RIG_DIR")")

# --- No-op: the installed binary already contains the commit ----------------------
#
# An install killed between install-local's swap and the marker write lands
# here on its retry: the binary on disk is ahead, the daemon still runs the old
# build, and this path used to exit without telling it to restart. The read
# that tells the two apart is the daemon's own commit (state.json), never the
# binary at $GT, which is the one already swapped (gt-tqxdd).
#
# Whether the daemon needs telling is an ancestry question, not an equality
# one: a daemon already running a build that CONTAINS $FULL_SHA has nothing to
# restart for, and an inequality answered "different, write the marker" for it
# — marker-ing the older commit, which the daemon then read as covered, cleared
# and stayed put, so the newer build it should have restarted into waited for
# the next install (gt-tg5to).
if [ -n "$PREV" ] && git -C "$RIG_DIR" merge-base --is-ancestor "$FULL_SHA" "$PREV" 2>/dev/null; then
  DAEMON_COMMIT=$(igt_daemon_commit "$DAEMON_DIR")
  if [ -n "$DAEMON_COMMIT" ]; then
    DAEMON_COMMIT=$(igt_resolve "$RIG_DIR" "$DAEMON_COMMIT")
  fi
  if [ -n "$DAEMON_COMMIT" ] && ! git -C "$RIG_DIR" merge-base --is-ancestor "$FULL_SHA" "$DAEMON_COMMIT" 2>/dev/null; then
    # A pending marker another install left already names what the daemon will
    # restart into. Replacing a newer marker with this older commit would drop
    # that restart, so leave it alone (gt-tg5to).
    PENDING_COMMIT=$(igt_pending_commit "$DAEMON_DIR")
    if [ -n "$PENDING_COMMIT" ]; then
      PENDING_COMMIT=$(igt_resolve "$RIG_DIR" "$PENDING_COMMIT")
    fi
    if [ -n "$PENDING_COMMIT" ] && git -C "$RIG_DIR" merge-base --is-ancestor "$FULL_SHA" "$PENDING_COMMIT" 2>/dev/null; then
      log "A pending restart marker for $PENDING_COMMIT already contains $FULL_SHA; leaving it in place."
    else
      log "Installed $PREV already contains $FULL_SHA, but the daemon is running $DAEMON_COMMIT; writing the restart marker."
      if ! write_restart_marker "$FULL_SHA"; then
        escalate high install-gt:marker-write-failed "$FULL_SHA is installed at $GT and the running daemon is still on $DAEMON_COMMIT, but writing $DAEMON_DIR/restart-pending.json failed — the binary stays installed; the daemon needs a manual restart to pick it up"
        igt_receipt failed "$FULL_SHA" "$PREV" marker-write "$MERGED_AT" "$START"
        result_line failed "$FULL_SHA" "$PREV" marker-write
        exit 1
      fi
    fi
  else
    log "Installed $PREV already contains $FULL_SHA; nothing to do."
  fi
  gateway_step install "$RIG_DIR" "$SCRIPT_DIR"
  igt_receipt noop "$FULL_SHA" "$PREV" already-installed "$MERGED_AT" "$START"
  result_line noop "$FULL_SHA" "$PREV" already-installed
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
git -C "$RIG_DIR" "${GIT_NO_AUTO_GC[@]}" merge --ff-only "$FULL_SHA" --quiet 2>/dev/null || refuse diverged "local main cannot fast-forward to $FULL_SHA"
if [ "$(git -C "$RIG_DIR" rev-list --count origin/main..HEAD 2>/dev/null || echo 0)" -gt 0 ]; then
  refuse diverged "local main has commits origin/main lacks"
fi
# HEAD can be past FULL_SHA when mayor/rig was already on a newer main; that is
# forward and contains the commit, so HEAD is what gets built and verified.
EXPECTED=$(git -C "$RIG_DIR" rev-parse HEAD)
if [ -n "$PREV" ] && ! git -C "$RIG_DIR" merge-base --is-ancestor "$PREV" "$EXPECTED" 2>/dev/null; then
  refuse not-forward "installed $PREV is not an ancestor of $EXPECTED (would be a downgrade)"
fi

# gt-vya0s can leave $GT OS-immutable (BSD chflags uchg / Linux chattr +i)
# between installs. That flag blocks more than a direct write to $GT itself:
# macOS `cp -p` also copies chflags onto the copy, and an immutable file
# cannot be renamed even under a new name — so a plain `cp -p "$GT" tmp` below
# would leave `tmp` stuck uchg too, and the following `mv` of *that* tmp would
# fail the same way a direct write would. clear_immutable strips the flag
# from any such copy right after it's made (only the live $GT should ever
# carry it), and from $GT itself before rollback's own replace; set_immutable
# re-applies it once $GT holds the restored binary — the same bracket
# install-binary.sh's atomic replace uses. Both are harmless no-ops when the
# flag was never set, or on a platform that can't set it at all (e.g.
# chattr +i without CAP_LINUX_IMMUTABLE).
clear_immutable() {
  local target="$1"
  [ -e "$target" ] || return 0
  chflags nouchg "$target" 2>/dev/null || true
  chattr -i "$target" 2>/dev/null || true
}

set_immutable() {
  local target="$1"
  chflags uchg "$target" 2>/dev/null && return 0
  chattr +i "$target" 2>/dev/null || true
}

# --- Keep the previous binary ------------------------------------------------------
if [ -f "$GT" ]; then
  cp -p "$GT" "$BIN_DIR/.gt.prev.$$"
  clear_immutable "$BIN_DIR/.gt.prev.$$"
  mv -f "$BIN_DIR/.gt.prev.$$" "$BIN_DIR/gt.prev"
fi

# rollback — put gt.prev back atomically; 0 on success.
rollback() {
  [ -f "$BIN_DIR/gt.prev" ] || return 1
  cp -p "$BIN_DIR/gt.prev" "$BIN_DIR/.gt.rollback.$$" 2>/dev/null || return 1
  clear_immutable "$BIN_DIR/.gt.rollback.$$"
  clear_immutable "$GT"
  if ! mv -f "$BIN_DIR/.gt.rollback.$$" "$GT" 2>/dev/null; then
    rm -f "$BIN_DIR/.gt.rollback.$$"
    # The mv failed for some reason other than the flag (disk full, EIO, a
    # directory permission change mid-run): $GT is left holding the bad
    # binary. Re-apply the flag anyway so that binary is at least still
    # protected from a stray write while a human responds to the CRITICAL
    # escalation this failure triggers.
    set_immutable "$GT"
    return 1
  fi
  set_immutable "$GT"
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
      result_line failed "$EXPECTED" "$PREV" rollback-failed
      exit 1
    fi
  fi
  escalate "$3" "$4" "$2"
  igt_receipt "$5" "$EXPECTED" "$PREV" "$1" "$MERGED_AT" "$START"
  result_line "$5" "$EXPECTED" "$PREV" "$1"
  exit 1
}

# --- Signing (best-effort) ----------------------------------------------------------
# Implements the contract described at SIGN_DIR above. Every failure path leaves
# the install exactly as it would have been without any of this: unsigned,
# ad-hoc, exit 0.
#
# The target is the installed binary, never the build output: `make build`
# stamps a fresh $(BUILD_TIME) into every invocation, so install-local's own
# build prerequisite re-links and any signature on $(BUILD_DIR)/$(BINARY) is
# gone before install-binary.sh ever copies it.

# code_timeout CMD... — GNU timeout(1) when it is on PATH (coreutils, or brew's
# gtimeout when the unprefixed name is not linked). Where neither exists,
# codesign runs unguarded: it gets </dev/null and an already-unlocked keychain,
# so it has nothing left to prompt for.
code_timeout() {
  if command -v timeout >/dev/null 2>&1; then timeout "$SIGN_TIMEOUT" "$@"
  elif command -v gtimeout >/dev/null 2>&1; then gtimeout "$SIGN_TIMEOUT" "$@"
  else "$@"
  fi
}

# sign_binary BIN — sign the installed BIN with the town's identity. codesign
# runs against a temp copy in BIN's own directory, renamed over BIN only once it
# has succeeded: signing the live path in place would rewrite the bytes of a
# binary the town may be exec'ing, and a codesign that dies mid-write would leave
# a half-signed file there (the temp+rename install-binary.sh uses, for the same
# reason). The OS-immutable flag install-binary.sh left on BIN comes off for the
# rename and goes back after, exactly as rollback() does it. Best-effort: any
# failure leaves BIN as install-local left it and logs one WARN — never a failed
# install, never a rollback.
sign_binary() {
  local bin="$1" pw tmp msg rc=0
  [ -f "$bin" ] || { log "WARNING: not signing: nothing installed at $bin"; return 0; }
  [ -f "$SIGN_KEYCHAIN" ] || { log "WARNING: installing ad-hoc: no keychain at $SIGN_KEYCHAIN"; return 0; }
  [ -f "$SIGN_PASSWORD_FILE" ] || { log "WARNING: installing ad-hoc: no password file at $SIGN_PASSWORD_FILE"; return 0; }
  pw=$(cat "$SIGN_PASSWORD_FILE") || { log "WARNING: installing ad-hoc: cannot read $SIGN_PASSWORD_FILE"; return 0; }
  # The password reaches this one call (in its argv, for as long as the unlock
  # takes). It is never logged, never echoed, and never handed to codesign.
  if ! security unlock-keychain -p "$pw" "$SIGN_KEYCHAIN" >/dev/null 2>&1; then
    log "WARNING: installing ad-hoc: cannot unlock $SIGN_KEYCHAIN"
    return 0
  fi
  tmp=$(mktemp "$(dirname "$bin")/.$(basename "$bin").signed.XXXXXX") \
    || { log "WARNING: installing ad-hoc: cannot create a temp file beside $bin"; return 0; }
  cp "$bin" "$tmp" || { rm -f "$tmp"; log "WARNING: installing ad-hoc: cannot copy $bin"; return 0; }
  chmod --reference="$bin" "$tmp" 2>/dev/null || chmod 0755 "$tmp"
  clear_immutable "$tmp"
  msg=$(code_timeout codesign --force --keychain "$SIGN_KEYCHAIN" -s "$SIGN_IDENTITY" \
          -i "$SIGN_IDENTIFIER" "$tmp" </dev/null 2>&1) || rc=$?
  if [ "$rc" != "0" ]; then
    rm -f "$tmp"
    log "WARNING: installing ad-hoc: codesign failed: $(printf '%s' "${msg:-no output}" | head -1)"
    return 0
  fi
  clear_immutable "$bin"
  if ! mv -f "$tmp" "$bin"; then
    rm -f "$tmp"
    set_immutable "$bin"
    log "WARNING: installing ad-hoc: cannot rename the signed copy over $bin"
    return 0
  fi
  set_immutable "$bin"
  log "Signed $bin as $SIGN_IDENTIFIER"
}

# --- Build and install -------------------------------------------------------------
log "Building $EXPECTED in $RIG_DIR (source: $SOURCE)"
if [ -n "$SLOT_ROLE" ]; then
  # Inside the container-gate slot, the way rebuild-gt builds past its
  # starvation threshold (gt-kox0). The acquired literal is slotAcquiredFormat
  # in internal/cmd/slot.go; without it the build never started.
  if [ ! -x "$GT" ]; then
    fail_install slot-unavailable "$GT is missing or not executable; cannot take the container-gate slot" \
      medium install-gt:slot-unavailable failed
  fi
  SLOT_LOG=$(mktemp)
  set +e
  (cd "$RIG_DIR" && "$GT" slot run --role "$SLOT_ROLE" --timeout "${SLOT_TIMEOUT}s" -- make SKIP_UPDATE_CHECK=1 build) 2>&1 | tee "$SLOT_LOG"
  BUILD_RC=${PIPESTATUS[0]}
  set -e
  if ! grep -q "Container-gate slot acquired" "$SLOT_LOG"; then
    rm -f "$SLOT_LOG"
    # Exit 126/127 is a gt that could not run at all (missing, not executable,
    # wrong format), which never asked for the slot. Reporting that as busy
    # (exit 3) tells the caller to retry, and a broken gt is what the retry
    # would have to replace (gt-bqzoq).
    if [ "$BUILD_RC" = "126" ] || [ "$BUILD_RC" = "127" ]; then
      fail_install slot-unavailable "cannot run $GT slot run (exit $BUILD_RC); the container-gate slot was never requested" \
        medium install-gt:slot-unavailable failed
    fi
    log "Did not get the container-gate slot within ${SLOT_TIMEOUT}s."
    igt_receipt refused "$EXPECTED" "$PREV" slot-busy "$MERGED_AT" "$START"
    result_line refused "$EXPECTED" "$PREV" slot-busy
    exit 3
  fi
  rm -f "$SLOT_LOG"
else
  BUILD_RC=0
  (cd "$RIG_DIR" && make SKIP_UPDATE_CHECK=1 build) 2>&1 || BUILD_RC=$?
fi
[ "$BUILD_RC" = "0" ] || fail_install build-failed "make build failed for $EXPECTED" medium install-gt:build-failed failed

# --- The sandbox image, before the swap ----------------------------------------------
# IMAGE_BIN is the commit that was just built; install-local has not replaced
# $GT with it yet. build-failed is one reason for both builds: rebuild-gt
# already escalates it, and a new install-gt reason would reach it as a second
# alert (rebuild_gt.go). --no-image-first skips this wait and builds the image
# after the restart marker instead (gt-vhn89). That detached build does have
# a reason of its own, install-gt:image-build-failed: the install stands, so
# rebuild-gt has no failure to report, and only the supervisor sees this one.
if [ "$IMAGE_FIRST" = "1" ] && ! polecat_image_step "$IMAGE_RUN" "$IMAGE_LOG" "$IMAGE_BIN" "$EXPECTED"; then
  fail_install build-failed "the sandbox image for $EXPECTED was not built and published; $GT was left at ${PREV:-the previous build} and the daemon was not told to restart" \
    medium install-gt:build-failed failed
fi

INSTALL_RC=0
INSTALL_OUT=$( (cd "$RIG_DIR" && make SKIP_UPDATE_CHECK=1 INSTALL_DIR="$BIN_DIR" install-local) 2>&1 ) || INSTALL_RC=$?
echo "$INSTALL_OUT"
if [ "$INSTALL_RC" != "0" ]; then
  # check-forward-only exits 1 when the binary is already at HEAD — reachable
  # only when the installed commit could not be read above.
  if printf '%s\n' "$INSTALL_OUT" | grep -q "already at HEAD"; then
    igt_receipt noop "$EXPECTED" "$PREV" already-installed "$MERGED_AT" "$START"
    result_line noop "$EXPECTED" "$PREV" already-installed
    exit 0
  fi
  fail_install build-failed "make install-local failed for $EXPECTED" medium install-gt:build-failed failed
fi

# Sign what install-local just put in place, before the smoke check runs it: the
# smoke check then exercises exactly the artifact the town will execute, and
# nothing is signed that has not already installed successfully.
sign_binary "$GT"

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

gateway_step install "$RIG_DIR" "$SCRIPT_DIR"

# --- Restart marker, receipt ---------------------------------------------------------
# The new binary is already installed and smoke-verified at this point: a
# failure writing the marker is not a build/install/smoke failure, and must
# never trigger fail_install's rollback of a binary that is good. It is its
# own reason (marker-write) so the receipt and escalation say plainly that
# the binary is in force but the daemon was not told to restart into it.
if ! write_restart_marker "$EXPECTED"; then
  escalate high install-gt:marker-write-failed "$EXPECTED is installed and verified at $GT, but writing $DAEMON_DIR/restart-pending.json failed — the binary stays installed; the daemon needs a manual restart to pick it up"
  igt_receipt failed "$EXPECTED" "$PREV" marker-write "$MERGED_AT" "$START"
  result_line failed "$EXPECTED" "$PREV" marker-write
  exit 1
fi
igt_receipt installed "$EXPECTED" "$PREV" "" "$MERGED_AT" "$START"
"$GT" escalate clear --fingerprint install-gt:build-failed --fingerprint install-gt:smoke-failed \
  --reason "install-gt: $EXPECTED is in force" >/dev/null 2>&1 || true
# --no-image-first: the old order. The binary and the restart marker are in
# place and the daemon has been told; the image is rebuilt detached and best
# effort, so an urgent daemon fix never waits on a docker build, and a build
# that fails or overruns its alarm leaves this install standing (gt-vhn89).
if [ "$IMAGE_FIRST" != "1" ]; then
  polecat_image_step "$IMAGE_RUN" "$IMAGE_LOG" "$IMAGE_BIN" "$EXPECTED" after || true
fi
log "In force: ${PREV:-unknown} -> $EXPECTED. The daemon restarts itself when idle."
result_line installed "$EXPECTED" "$PREV" ""

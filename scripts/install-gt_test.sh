#!/usr/bin/env bash
# Tests for scripts/install-gt.sh: the one install path (`make install`,
# rebuild-gt; claude-7fc, gt-z0l3s). A stub make on PATH builds a stub gt that
# reports a chosen commit and "installs" by copying it into place; the stub gt
# answers stale/version/sync/escalate/slot and logs what it was asked.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALLER="$SCRIPT_DIR/install-gt.sh"
FAILURES=0
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { echo "PASS: $*"; }

# gt stub template; __COMMIT__ is the commit this "binary" was built from.
# broken_<commit> makes 'stale --json' print junk; lockout_<commit> makes it
# write-protect the bin dir first (so a rollback cannot rename into it).
write_template() {
  cat > "$1/gt.tmpl" <<'STUB'
#!/usr/bin/env bash
C="__COMMIT__"
[ -z "${T_WORLD:-}" ] || echo "${BD_ACTOR:-}" >> "$T_WORLD/actors.log"
case "$1 ${2:-}" in
  "stale --json")
    [ -e "$T_WORLD/lockout_$C" ] && chmod a-w "$T_WORLD/bin"
    if [ -e "$T_WORLD/broken_$C" ]; then echo "not json"; exit 0; fi
    printf '{"stale": false, "binary_commit": "%s"}\n' "$C" ;;
  "version "*)
    if [ -n "$C" ]; then echo "gt version $C (dev: main@$C)"; else echo "gt version dev"; fi ;;
  "formula sync"|"plugin sync") echo "synced $1"; echo "sync $1" >> "$T_WORLD/gt.log" ;;
  "escalate "*) echo "escalate $*" >> "$T_WORLD/gt.log" ;;
  "slot run")
    echo "slot run $*" >> "$T_WORLD/gt.log"
    shift 2
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
    [ "${1:-}" = "--" ] && shift
    if [ -e "$T_WORLD/slot_gt_unrunnable" ]; then echo "exec format error" >&2; exit 126; fi
    if [ -e "$T_WORLD/slot_refuse" ]; then echo "timed out waiting for slot" >&2; exit 1; fi
    echo "Container-gate slot acquired (role=test, waited 0s, slot 0/2)."
    exec "$@" ;;
  *) echo "stub gt: unhandled: $*" >&2; exit 1 ;;
esac
STUB
}

# install_stub T COMMIT — put a gt reporting COMMIT at $T/bin/gt.
install_stub() {
  mkdir -p "$1/bin"
  sed "s/__COMMIT__/$2/" "$1/gt.tmpl" > "$1/bin/gt"
  chmod +x "$1/bin/gt"
}

# make_world -> dir with: origin.git, rig (main at C2, C1 its parent), bin/gt
# reporting C1 (short), a stub make on PATH, and C1/C2 recorded in files.
#
# Every case gets a copy of WORLD_TEMPLATE (built once, below) rather than a
# fresh world: forty cases each want one of their own, and the twenty-odd
# processes a fresh world costs (mostly git) are a sixth of this script's wall
# time on a loaded host. A case mutates only its own copy; the copy's rig gets
# its origin remote rewritten, since git clone recorded the template's path and
# a case may push to it. The guard after build_world's template is built fails
# the run if a copy ever stops matching a fresh world (gt-m8y0b).
make_world() {
  local t; t=$(mktemp -d)
  cp -Rp "$WORLD_TEMPLATE/." "$t/"
  git -C "$t/rig" remote set-url origin "$t/origin.git"
  echo "$t"
}

build_world() {
  local t; t=$(mktemp -d)
  git init -q --bare -b main "$t/origin.git"
  git clone -q "$t/origin.git" "$t/rig" 2>/dev/null
  git -C "$t/rig" -c user.email=t@t -c user.name=t commit -q --allow-empty -m c1
  git -C "$t/rig" -c user.email=t@t -c user.name=t commit -q --allow-empty -m c2
  echo tracked > "$t/rig/tracked.txt"
  git -C "$t/rig" add tracked.txt
  git -C "$t/rig" -c user.email=t@t -c user.name=t commit -q -m c3
  git -C "$t/rig" push -q origin main
  git -C "$t/rig" rev-parse --short HEAD~1 > "$t/c1"
  git -C "$t/rig" rev-parse --short HEAD > "$t/c2"
  # rig's local main sits at c1 before the run, so the install must fetch+ff.
  git -C "$t/rig" reset -q --hard HEAD~1
  mkdir -p "$t/stubs" "$t/daemon"
  write_template "$t"
  install_stub "$t" "$(cat "$t/c1")"
  cat > "$t/stubs/make" <<'MAKE'
#!/usr/bin/env bash
echo "make $*" >> "$T_WORLD/make.log"
target="" inst=""
for a in "$@"; do
  case "$a" in
    INSTALL_DIR=*) inst="${a#INSTALL_DIR=}" ;;
    *=*) ;;
    *) target="$a" ;;
  esac
done
case "$target" in
  build)
    [ -e "$T_WORLD/fail_build" ] && { echo "build failed" >&2; exit 2; }
    # The build output: a stub gt reporting this build's commit (new_commit when
    # a case pins one, else the rig's HEAD), landing where the real Makefile's
    # $(BUILD_DIR)/$(BINARY) does.
    c=$(cat "$T_WORLD/new_commit" 2>/dev/null || git rev-parse --short HEAD)
    sed "s/__COMMIT__/$c/" "$T_WORLD/gt.tmpl" > "$T_WORLD/rig/gt"
    chmod +x "$T_WORLD/rig/gt"
    exit 0 ;;
  install-local)
    if [ -e "$T_WORLD/already_at_head" ]; then echo "Binary is already at HEAD, nothing to do"; exit 1; fi
    [ -e "$T_WORLD/fail_install" ] && exit 2
    # install-binary.sh's copy of that build output into the install dir, and
    # nothing more: every install-gt step around it belongs to install-gt.sh,
    # signing included, so the test exercises that step and not a stand-in.
    cp "$T_WORLD/rig/gt" "$inst/.gt.new"
    chmod +x "$inst/.gt.new"
    # simulate_immutable_replace marker: stand in for install-binary.sh's own
    # gt-vya0s clear-flag/replace/re-set-flag bracket, so a case can leave the
    # installed binary OS-immutable the way a real gt-vya0s install would.
    if [ -e "$T_WORLD/simulate_immutable_replace" ]; then
      chflags nouchg "$inst/gt" 2>/dev/null || true
      chattr -i "$inst/gt" 2>/dev/null || true
    fi
    mv -f "$inst/.gt.new" "$inst/gt"
    if [ -e "$T_WORLD/simulate_immutable_replace" ]; then
      chflags uchg "$inst/gt" 2>/dev/null || chattr +i "$inst/gt" 2>/dev/null || true
    fi ;;
  *) echo "stub make: unhandled target $target" >&2; exit 2 ;;
esac
MAKE
  chmod +x "$t/stubs/make"
  # launchctl stub: logs its argv; `print` reports "not loaded" unless gw_loaded
  # exists, and `bootstrap` fails when gw_bootstrap_fails exists. No test
  # reaches the real launchd.
  cat > "$t/stubs/launchctl" <<'LCTL'
#!/usr/bin/env bash
echo "launchctl $*" >> "$T_WORLD/launchctl.log"
[ "$1" != "bootstrap" ] || [ ! -e "$T_WORLD/gw_bootstrap_fails" ] || exit 1
[ "$1" != "print" ] || [ -e "$T_WORLD/gw_loaded" ]
LCTL
  chmod +x "$t/stubs/launchctl"
  mkdir -p "$t/home"
  echo "$t"
}

# run_install T ARGS... -> exit code; output in $T/run.out. The run carries
# no identity unless CALLER_ACTOR names one as BD_ACTOR.
run_install() {
  local t="$1" rc=0; shift
  ( unset BD_ACTOR GT_ROLE
    if [ -n "${CALLER_ACTOR:-}" ]; then export BD_ACTOR="$CALLER_ACTOR"; fi
    export T_WORLD="$t" INSTALL_GT_BIN_DIR="$t/bin" INSTALL_GT_DAEMON_DIR="$t/daemon" \
      INSTALL_GT_RIG_DIR="$t/rig" INSTALL_GT_LOCK_WAIT="${LOCK_WAIT:-5}" \
      INSTALL_GT_SIGN_DIR="$t/signing" INSTALL_GT_GATEWAY_HOME="$t/home" \
      INSTALL_GT_IMAGE_RUN="${IMAGE_RUN:-$t/no-image-plugin/run.sh}" INSTALL_GT_IMAGE_LOG="$t/image.log" \
      INSTALL_GT_IMAGE_LOCK="$t/image.lock" \
      PATH="$t/stubs:/usr/bin:/bin:/opt/homebrew/bin:/usr/sbin:/sbin"
    bash "$INSTALLER" "$@" ) > "$t/run.out" 2>&1 || rc=$?
  echo "$rc"
}

# run_install_at T BIN_DIR_ASSIGN ARGS... -> exit code; run_install with
# INSTALL_GT_BIN_DIR unset, BIN_DIR_ASSIGN naming the install dir, and HOME a
# temp dir — so a case can see which env the script honors without either env
# pointing at a real ~/.local/bin. Output in $T/run.out.
run_install_at() {
  local t="$1" assign="$2" rc=0; shift 2
  ( unset BD_ACTOR GT_ROLE INSTALL_GT_BIN_DIR INSTALL_DIR
    export T_WORLD="$t" HOME="$t/home-default" "$assign" \
      INSTALL_GT_DAEMON_DIR="$t/daemon" INSTALL_GT_RIG_DIR="$t/rig" \
      INSTALL_GT_LOCK_WAIT="${LOCK_WAIT:-5}" INSTALL_GT_SIGN_DIR="$t/signing" \
      INSTALL_GT_GATEWAY_HOME="$t/home" \
      INSTALL_GT_IMAGE_RUN="${IMAGE_RUN:-$t/no-image-plugin/run.sh}" INSTALL_GT_IMAGE_LOG="$t/image.log" \
      PATH="$t/stubs:/usr/bin:/bin:/opt/homebrew/bin:/usr/sbin:/sbin"
    bash "$INSTALLER" "$@" ) > "$t/run.out" 2>&1 || rc=$?
  echo "$rc"
}

# last_receipt T FIELD -> that field of the last receipt line ("None" if null)
last_receipt() {
  tail -1 "$1/daemon/install-receipts.jsonl" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1]))' "$2" 2>/dev/null || echo "NO-RECEIPT"
}

# actors T -> the distinct BD_ACTOR values the gt stub ran with, one line
actors() { sort -u "$1/actors.log" 2>/dev/null | tr '\n' ' ' | sed 's/ $//'; }

# escalations T -> the escalations the install raised: every gt.log escalation
# line except the `escalate clear` of the fingerprints it just resolved, which
# a successful install always runs (the stub logs 'escalate' plus its argv).
escalations() { grep "^escalate " "$1/gt.log" 2>/dev/null | grep -v -- "escalate clear" || true; }

# reported T -> the commit the installed gt stub reports
reported() { "$1/bin/gt" stale --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["binary_commit"])'; }

# The world make_world copies. Built here, in the script's own shell: every
# case calls make_world inside a command substitution, and an assignment made
# in one of those dies with the subshell.
WORLD_TEMPLATE=$(build_world)

# The template is a whole temp world, and a case that trips set -e exits before
# the signing section claims the EXIT trap, so this trap has to exist from here
# on; that one chains it instead of replacing it (gt-m8y0b).
cleanup_world_template() {
  [ -z "${WORLD_TEMPLATE:-}" ] || rm -rf "$WORLD_TEMPLATE"
}
trap cleanup_world_template EXIT

echo "=== install-gt.sh tests ==="

# world_manifest T -> what a case reads from a world, one line each: the rig and
# origin trees, the branch, the origin rewrite, and every file outside the git
# dirs with its exec bit and content. Ids that differ between any two worlds are
# masked, so a copy and a fresh build compare equal when interchangeable
# (gt-m8y0b).
world_manifest() {
  local t="$1" f s1 s2 mode
  s1=$(cat "$t/c1"); s2=$(cat "$t/c2")
  printf 'rig-head-tree %s\n' "$(git -C "$t/rig" rev-parse 'HEAD^{tree}')"
  printf 'origin-main-tree %s\n' "$(git -C "$t/origin.git" rev-parse 'main^{tree}')"
  printf 'rig-branch %s\n' "$(git -C "$t/rig" rev-parse --abbrev-ref HEAD)"
  printf 'rig-clean %s\n' "$(git -C "$t/rig" status --porcelain)"
  printf 'origin-url %s\n' "$(git -C "$t/rig" remote get-url origin | sed "s|^$t|__WORLD__|")"
  ( cd "$t" && find . -path ./rig/.git -prune -o -path ./origin.git -prune -o -type f -print | sort ) |
  while IFS= read -r f; do
    if [ -x "$t/$f" ]; then mode=x; else mode=-; fi
    printf 'file %s %s %s\n' "$f" "$mode" "$(sed -e "s/$s1/__C1__/g" -e "s/$s2/__C2__/g" "$t/$f" | cksum)"
  done
}

# --- Guard: a copy is a fresh world. Every case below runs against a copy of
# WORLD_TEMPLATE rather than a world of its own, so a copy that dropped a file,
# an exec bit, or the origin rewrite would change the suite's results without
# failing it. Build one world the slow way and demand the fast one match it. ---
FRESH=$(build_world)
COPY=$(make_world)
FRESH_MANIFEST=$(world_manifest "$FRESH")
COPY_MANIFEST=$(world_manifest "$COPY")
if [ "$FRESH_MANIFEST" = "$COPY_MANIFEST" ]; then
  pass "template: a copy is indistinguishable from a fresh world"
else
  fail "template: a copy differs from a fresh world: $(diff <(printf '%s\n' "$FRESH_MANIFEST") <(printf '%s\n' "$COPY_MANIFEST") | head -20 | tr '\n' ';')"
fi
rm -rf "$FRESH" "$COPY"

# --- Case 1: happy path. c1 installed, merge c2 lands -> fetch, ff, build,
# install, smoke, sync, marker, receipt, RESULT line. ---
T=$(make_world)
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
rc=$(run_install "$T" --sha "$C2_FULL" --source manual)
[ "$rc" = "0" ] && pass "happy: exit 0" || fail "happy: exit $rc: $(cat "$T/run.out")"
[ "$(actors "$T")" = "installer" ] && pass "happy: every gt ran as BD_ACTOR=installer" || fail "happy: gt actors '$(actors "$T")'"
[ "$(reported "$T")" = "$(cat "$T/c2")" ] && pass "happy: c2 in force" || fail "happy: in force is $(reported "$T")"
grep -q "C=\"$(cat "$T/c1")\"" "$T/bin/gt.prev" && pass "happy: gt.prev is the c1 binary" || fail "happy: gt.prev missing or wrong"
[ "$(git -C "$T/rig" rev-parse HEAD)" = "$C2_FULL" ] && pass "happy: rig fast-forwarded" || fail "happy: rig HEAD not at c2"
python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
assert m["commit"] == sys.argv[2], m
assert m["source"] == "manual", m
assert m["repo"] == sys.argv[3], m
assert m["requested_at"].endswith("Z"), m
' "$T/daemon/restart-pending.json" "$C2_FULL" "$T/rig" && pass "happy: marker shape" || fail "happy: marker wrong: $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"
[ "$(last_receipt "$T" event)" = "installed" ] && pass "happy: installed receipt" || fail "happy: receipt $(tail -1 "$T/daemon/install-receipts.jsonl" 2>/dev/null)"
[ "$(last_receipt "$T" commit)" = "$C2_FULL" ] && pass "happy: receipt commit is full" || fail "happy: receipt commit $(last_receipt "$T" commit)"
[ "$(last_receipt "$T" merged_at)" != "None" ] && pass "happy: receipt has merged_at" || fail "happy: no merged_at"
grep -q "sync formula" "$T/gt.log" && grep -q "sync plugin" "$T/gt.log" && pass "happy: syncs ran" || fail "happy: syncs missing: $(cat "$T/gt.log" 2>/dev/null)"
[ "$(tail -1 "$T/run.out")" = "install-gt: RESULT installed $C2_FULL $(git -C "$T/rig" rev-parse HEAD~1) -" ] && pass "happy: RESULT line" || fail "happy: RESULT line: $(tail -1 "$T/run.out")"
grep -q "make SKIP_UPDATE_CHECK=1 INSTALL_DIR=$T/bin install-local" "$T/make.log" && pass "happy: install-local into INSTALL_GT_BIN_DIR" || fail "happy: make.log $(cat "$T/make.log")"

# --- Case 2: already installed -> no-op: no make, no marker, noop receipt ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && [ ! -e "$T/make.log" ] && [ ! -e "$T/daemon/restart-pending.json" ] \
  && pass "noop (same): exit 0, no build, no marker" || fail "noop (same): rc=$rc $(cat "$T/run.out")"
[ "$(last_receipt "$T" event)" = "noop" ] && pass "noop (same): noop receipt" || fail "noop (same): receipt $(last_receipt "$T" event)"

# --- Case 3: an OLDER commit than the installed one -> no-op, never a downgrade ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c1")" --source rebuild-gt)
[ "$rc" = "0" ] && [ ! -e "$T/make.log" ] && pass "noop (ancestor): exit 0, no build" || fail "noop (ancestor): rc=$rc"

# --- Case 3b: an install killed between install-local's swap and the marker
# write leaves the binary ahead of the daemon still running the old build. Its
# retry takes the noop path (the binary already contains the commit) and must
# still write the restart marker, or the daemon keeps the old build (gt-tqxdd).
# The daemon's commit is state.json's, not the binary's: both would read c2. ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
printf '{"running": true, "pid": 1, "commit": "%s"}\n' "$(git -C "$T/rig" rev-parse HEAD)" > "$T/daemon/state.json"
rc=$(run_install "$T" --sha "$C2_FULL" --source rebuild-gt)
[ "$rc" = "0" ] && [ ! -e "$T/make.log" ] \
  && pass "noop behind daemon: exit 0, no build" || fail "noop behind daemon: rc=$rc $(cat "$T/run.out")"
python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
assert m["commit"] == sys.argv[2], m
assert m["source"] == "rebuild-gt", m
assert m["repo"] == sys.argv[3], m
' "$T/daemon/restart-pending.json" "$C2_FULL" "$T/rig" \
  && pass "noop behind daemon: marker names the installed commit" \
  || fail "noop behind daemon: marker wrong: $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"
[ "$(last_receipt "$T" event)" = "noop" ] && pass "noop behind daemon: noop receipt" || fail "noop behind daemon: receipt $(last_receipt "$T" event)"
grep -q "but the daemon is running" "$T/run.out" && pass "noop behind daemon: the log names the daemon's commit" || fail "noop behind daemon: $(cat "$T/run.out")"

# --- Case 3c: binary and daemon both already on the commit -> still no marker ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
printf '{"running": true, "pid": 1, "commit": "%s"}\n' "$C2_FULL" > "$T/daemon/state.json"
rc=$(run_install "$T" --sha "$C2_FULL" --source manual)
[ "$rc" = "0" ] && [ ! -e "$T/daemon/restart-pending.json" ] \
  && pass "noop current daemon: exit 0, no marker" || fail "noop current daemon: rc=$rc $(cat "$T/run.out") $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"

# --- Case 3d: the requested commit is OLDER than the daemon's and contained by
# it -> no marker, and a newer marker already pending is left alone. The old
# equality check saw "daemon != commit" and re-marker-ed the older commit, which
# the daemon read as covered, cleared, and stayed on (gt-tg5to). ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
C1_FULL=$(git -C "$T/origin.git" rev-parse main~1)
printf '{"running": true, "pid": 1, "commit": "%s"}\n' "$C2_FULL" > "$T/daemon/state.json"
python3 -c 'import json,sys; json.dump({"commit": sys.argv[2], "requested_at": "2026-01-01T00:00:00Z", "source": "manual", "repo": sys.argv[3]}, open(sys.argv[1], "w"))' \
  "$T/daemon/restart-pending.json" "$C2_FULL" "$T/rig"
rc=$(run_install "$T" --sha "$C1_FULL" --source rebuild-gt)
[ "$rc" = "0" ] && [ ! -e "$T/make.log" ] \
  && pass "noop contained by daemon: exit 0, no build" || fail "noop contained by daemon: rc=$rc $(cat "$T/run.out")"
python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); assert m["commit"] == sys.argv[2], m' \
  "$T/daemon/restart-pending.json" "$C2_FULL" \
  && pass "noop contained by daemon: newer marker untouched" \
  || fail "noop contained by daemon: marker is $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"

# --- Case 3e: the daemon does NOT contain the request, but a newer marker is
# already pending -> that marker stays; writing this older commit over it would
# drop the restart the newer install asked for (gt-tg5to). ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
C1_FULL=$(git -C "$T/origin.git" rev-parse main~1)
C0_FULL=$(git -C "$T/origin.git" rev-parse main~2)
printf '{"running": true, "pid": 1, "commit": "%s"}\n' "$C0_FULL" > "$T/daemon/state.json"
python3 -c 'import json,sys; json.dump({"commit": sys.argv[2], "requested_at": "2026-01-01T00:00:00Z", "source": "manual", "repo": sys.argv[3]}, open(sys.argv[1], "w"))' \
  "$T/daemon/restart-pending.json" "$C2_FULL" "$T/rig"
rc=$(run_install "$T" --sha "$C1_FULL" --source rebuild-gt)
[ "$rc" = "0" ] && [ ! -e "$T/make.log" ] \
  && pass "noop behind newer marker: exit 0, no build" || fail "noop behind newer marker: rc=$rc $(cat "$T/run.out")"
python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); assert m["commit"] == sys.argv[2], m' \
  "$T/daemon/restart-pending.json" "$C2_FULL" \
  && pass "noop behind newer marker: marker untouched" \
  || fail "noop behind newer marker: marker is $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"
grep -q "leaving it in place" "$T/run.out" && pass "noop behind newer marker: the log says so" || fail "noop behind newer marker: $(cat "$T/run.out")"

# --- Case 3f: an unreadable state.json keeps today's behaviour: no daemon
# commit, no marker (gt-tg5to). ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
C1_FULL=$(git -C "$T/origin.git" rev-parse main~1)
echo "not json" > "$T/daemon/state.json"
rc=$(run_install "$T" --sha "$C1_FULL" --source rebuild-gt)
[ "$rc" = "0" ] && [ ! -e "$T/daemon/restart-pending.json" ] \
  && pass "noop unreadable state: exit 0, no marker" || fail "noop unreadable state: rc=$rc $(cat "$T/run.out") $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"

# --- Case 3g: an unresolvable daemon commit keeps today's behaviour: no marker ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
C1_FULL=$(git -C "$T/origin.git" rev-parse main~1)
printf '{"running": true, "pid": 1, "commit": "%s"}\n' "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" > "$T/daemon/state.json"
rc=$(run_install "$T" --sha "$C1_FULL" --source rebuild-gt)
[ "$rc" = "0" ] && [ ! -e "$T/daemon/restart-pending.json" ] \
  && pass "noop unresolvable daemon commit: exit 0, no marker" || fail "noop unresolvable daemon commit: rc=$rc $(cat "$T/run.out") $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"

# --- Case 4: the new binary reports an unresolvable commit -> rollback to the
# previous binary, HIGH escalation, no marker. ---
T=$(make_world)
echo deadbee > "$T/new_commit"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "1" ] && pass "smoke unverifiable: exit 1" || fail "smoke unverifiable: rc=$rc $(cat "$T/run.out")"
[ "$(reported "$T")" = "$(cat "$T/c1")" ] && pass "smoke unverifiable: c1 restored" || fail "smoke unverifiable: in force $(reported "$T")"
grep -q "escalate .*cannot verify what came into force.*--fingerprint install-gt:smoke-failed" "$T/gt.log" \
  && pass "smoke unverifiable: escalated" || fail "smoke unverifiable: gt.log $(cat "$T/gt.log" 2>/dev/null)"
grep -q -- "-s high" "$T/gt.log" && pass "smoke unverifiable: HIGH" || fail "smoke unverifiable: not HIGH"
[ ! -e "$T/daemon/restart-pending.json" ] && pass "smoke unverifiable: no marker" || fail "smoke unverifiable: marker written"
[ "$(last_receipt "$T" event)" = "rolled_back" ] && pass "smoke unverifiable: rolled_back receipt" || fail "smoke unverifiable: receipt $(last_receipt "$T" event)"

# --- Case 5: the new binary reports a real but wrong commit -> "did not take" ---
T=$(make_world)
cat "$T/c1" > "$T/new_commit"
# A c1 stub would be byte-identical to the installed one, and the rollback
# only fires when the file changed; the not-take stub differs by a comment.
python3 -c 'import sys; p=sys.argv[1]; s=open(p).read().replace("C=\"__COMMIT__\"", "# not-take\nC=\"__COMMIT__\"", 1); open(p,"w").write(s)' "$T/gt.tmpl"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "1" ] && grep -q "escalate .*the install did not take" "$T/gt.log" \
  && pass "did not take: exit 1, escalated" || fail "did not take: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"

# --- Case 6: new binary cannot answer 'stale --json' -> rollback ---
T=$(make_world)
touch "$T/broken_$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "1" ] && [ "$(reported "$T")" = "$(cat "$T/c1")" ] \
  && pass "stale unparseable: rolled back" || fail "stale unparseable: rc=$rc"

# --- Case 7: make build fails -> exit 1, binary untouched, MEDIUM escalation ---
T=$(make_world)
touch "$T/fail_build"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
[ "$rc" = "1" ] && [ "$(reported "$T")" = "$(cat "$T/c1")" ] && pass "build fails: exit 1, untouched" || fail "build fails: rc=$rc"
grep -q -- "--fingerprint install-gt:build-failed" "$T/gt.log" && pass "build fails: escalated" || fail "build fails: no escalation"
[ ! -e "$T/daemon/restart-pending.json" ] && [ "$(last_receipt "$T" event)" = "failed" ] \
  && pass "build fails: no marker, failed receipt" || fail "build fails: marker or receipt wrong"

# --- Case 8: installed commit unreadable and install-local says "already at
# HEAD" -> no-op, not a failure ---
T=$(make_world)
install_stub "$T" ""
touch "$T/already_at_head"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
[ "$rc" = "0" ] && [ "$(last_receipt "$T" event)" = "noop" ] && pass "already at HEAD: noop" || fail "already at HEAD: rc=$rc"

# --- Case 9: another install holds the lock past the wait -> exit 3, lock-busy ---
T=$(make_world)
perl -e 'use Fcntl qw(:flock); open(my $f, ">>", $ARGV[0]) or die; flock($f, LOCK_EX) or die; sleep 20' "$T/daemon/install-gt.lock" &
HOLDER=$!
sleep 1
rc=$(LOCK_WAIT=1 run_install "$T" --sha "$(cat "$T/c2")" --source manual)
kill "$HOLDER" 2>/dev/null || true
[ "$rc" = "3" ] && [ ! -e "$T/make.log" ] && pass "lock busy: exit 3, no build" || fail "lock busy: rc=$rc $(cat "$T/run.out")"
[ "$(last_receipt "$T" reason)" = "lock-busy" ] && pass "lock busy: receipt reason" || fail "lock busy: reason $(last_receipt "$T" reason)"

# --- Case 10: refusals -> exit 2, no build, binary untouched, no escalation ---
T=$(make_world)
# The rig's pre-run HEAD (c2's parent chain before c3) tracks no files, so
# commit one on origin and locally, then dirty it.
echo base > "$T/rig/local.txt"; git -C "$T/rig" add local.txt
git -C "$T/rig" -c user.email=t@t -c user.name=t commit -q -m local-tracked
git -C "$T/rig" push -q origin HEAD:refs/heads/side-seed
echo dirty >> "$T/rig/local.txt"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "2" ] && [ ! -e "$T/make.log" ] && [ "$(last_receipt "$T" reason)" = "dirty" ] \
  && pass "refused dirty" || fail "refused dirty: rc=$rc $(cat "$T/run.out")"
! grep -q "escalate" "$T/gt.log" 2>/dev/null && pass "refused dirty: not escalated here" || fail "refused dirty: escalated"

T=$(make_world)
git -C "$T/rig" checkout -q -b side
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "wrong-branch" ] && pass "refused wrong branch" || fail "refused wrong branch: rc=$rc"

T=$(make_world)
git -C "$T/rig" -c user.email=t@t -c user.name=t commit -q --allow-empty -m local-only
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "diverged" ] && pass "refused diverged" || fail "refused diverged: rc=$rc $(cat "$T/run.out")"

T=$(make_world)
rc=$(run_install "$T" --sha 0123456789abcdef0123456789abcdef01234567 --source manual)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "unknown-commit" ] && pass "refused unknown commit" || fail "refused unknown commit: rc=$rc"

# --- Case 11: --slot-role builds inside 'gt slot run'; a slot never acquired is busy ---
T=$(make_world)
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt --slot-role gastown/rebuild-gt --slot-timeout 30)
[ "$rc" = "0" ] && grep -q "slot run --role gastown/rebuild-gt --timeout 30s" "$T/gt.log" \
  && pass "slot role: built inside the slot" || fail "slot role: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"
T=$(make_world)
touch "$T/slot_refuse"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt --slot-role gastown/rebuild-gt --slot-timeout 30)
[ "$rc" = "3" ] && [ "$(last_receipt "$T" reason)" = "slot-busy" ] && [ "$(reported "$T")" = "$(cat "$T/c1")" ] \
  && pass "slot refused: exit 3, untouched" || fail "slot refused: rc=$rc"

# A gt that cannot run at all (missing, not executable, 126/127) never got to
# ask for the slot: that is a failed install, not a busy slot (gt-bqzoq). Exit 3
# would tell the caller to retry, and a broken gt is the very thing the retry
# would have to replace.
T=$(make_world)
touch "$T/slot_gt_unrunnable"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt --slot-role gastown/rebuild-gt --slot-timeout 30)
[ "$rc" = "1" ] && [ "$(last_receipt "$T" reason)" = "slot-unavailable" ] \
  && pass "unrunnable gt with a slot role: exit 1, slot-unavailable (not busy)" \
  || fail "unrunnable gt with a slot role: rc=$rc reason=$(last_receipt "$T" reason)"

# --- Case 12: the rollback itself fails -> CRITICAL ---
T=$(make_world)
echo deadbee > "$T/new_commit"
touch "$T/lockout_deadbee"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
chmod u+w "$T/bin"
[ "$rc" = "1" ] && grep -q -- "-s critical .*--fingerprint install-gt:rollback-failed" "$T/gt.log" \
  && pass "rollback fails: CRITICAL" || fail "rollback fails: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"

# --- Case 13: bad arguments -> the RESULT-line/exit-0-3 contract holds even
# on a path that never reaches DAEMON_DIR (fix round 1: these used to be a
# bare `exit 1` with no RESULT line at all). ---
T=$(make_world)
rc=$(run_install "$T" --sha "")
[ "$rc" = "1" ] && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed - - unexpected" ] \
  && pass "bad args (empty --sha): exit 1, RESULT line" || fail "bad args (empty --sha): rc=$rc $(cat "$T/run.out")"

T=$(make_world)
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source bogus)
[ "$rc" = "1" ] && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed $(cat "$T/c2") - unexpected" ] \
  && pass "bad args (bad --source): exit 1, RESULT line" || fail "bad args (bad --source): rc=$rc $(cat "$T/run.out")"

# --- Case 13b: no arguments (make install) -> installs origin/main as
# source "manual" (gt-z0l3s). ---
T=$(make_world)
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
rc=$(run_install "$T")
[ "$rc" = "0" ] && [ "$(reported "$T")" = "$(cat "$T/c2")" ] \
  && pass "no args: origin/main in force" || fail "no args: rc=$rc in force $(reported "$T"): $(cat "$T/run.out")"
[ "$(last_receipt "$T" source)" = "manual" ] && [ "$(last_receipt "$T" commit)" = "$C2_FULL" ] \
  && pass "no args: manual receipt for origin/main" || fail "no args: receipt $(tail -1 "$T/daemon/install-receipts.jsonl" 2>/dev/null)"

T=$(make_world)
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "1" ] && [ "$(reported "$T")" = "$(cat "$T/c1")" ] \
  && pass "retired source post-merge: refused as a bad argument" || fail "retired source post-merge: rc=$rc"

# --- Case 13c: without GT_TOWN_ROOT or dir overrides, the town is the
# outermost directory holding mayor/town.json above the script, and with no
# town at all it fails with the RESULT line and names install-local. ---
# run_located T SCRIPTS_DIR ARGS... -> exit code; runs a copy of the script
# from SCRIPTS_DIR with no town env; output in $T/run.out.
run_located() {
  local t="$1" dir="$2" rc=0; shift 2
  mkdir -p "$dir/lib"
  cp "$INSTALLER" "$dir/install-gt.sh"
  cp "$SCRIPT_DIR/lib/install-gt-lib.sh" "$dir/lib/"
  ( unset GT_TOWN_ROOT INSTALL_GT_DAEMON_DIR INSTALL_GT_RIG_DIR
    export T_WORLD="$t" INSTALL_GT_BIN_DIR="$t/bin" INSTALL_GT_LOCK_WAIT=5 \
      INSTALL_GT_SIGN_DIR="$t/signing" INSTALL_GT_GATEWAY_HOME="$t/home" \
      PATH="$t/stubs:/usr/bin:/bin:/opt/homebrew/bin:/usr/sbin:/sbin"
    bash "$dir/install-gt.sh" "$@" ) > "$t/run.out" 2>&1 || rc=$?
  echo "$rc"
}
T=$(make_world)
mkdir -p "$T/town/mayor" "$T/town/gastown/mayor" "$T/town/gastown/crew/x"
echo '{}' > "$T/town/mayor/town.json"
ln -s "$T/rig" "$T/town/gastown/mayor/rig"
rc=$(run_located "$T" "$T/town/gastown/crew/x/scripts")
[ "$rc" = "0" ] && [ "$(reported "$T")" = "$(cat "$T/c2")" ] && [ -f "$T/town/daemon/restart-pending.json" ] \
  && pass "town found above the script: installed, marker in <town>/daemon" || fail "town found above the script: rc=$rc $(cat "$T/run.out")"

T=$(make_world)
rc=$(run_located "$T" "$T/notown/scripts")
[ "$rc" = "1" ] && grep -q "install-local" "$T/run.out" && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed origin/main - unexpected" ] \
  && pass "no town: exit 1, names install-local, RESULT line" || fail "no town: rc=$rc $(cat "$T/run.out")"

# --- Case 14: perl itself dies before it ever reaches exec (the lock path is
# a pre-existing directory, so 'open >>' fails) -> its die() exit code (not
# necessarily 75, and not necessarily outside 0-3) must never leak through as
# a bare exit status with no RESULT line. DAEMON_DIR is left writable so this
# exercises perl's own failure, not a bash-level permission error. ---
T=$(make_world)
mkdir -p "$T/daemon/install-gt.lock"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "1" ] && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed $(cat "$T/c2") - unexpected" ] \
  && pass "lock file unopenable: mapped to exit 1 with a RESULT line" || fail "lock file unopenable: rc=$rc $(cat "$T/run.out")"

# --- Case 15: the new binary is installed and smoke-verified, but writing
# restart-pending.json fails (path is a directory) -> failed/marker-write,
# exit 1, HIGH escalation, and — unlike a build/smoke failure — no rollback:
# the good binary stays in force. ---
T=$(make_world)
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
mkdir -p "$T/daemon/restart-pending.json"
rc=$(run_install "$T" --sha "$C2_FULL" --source manual)
[ "$rc" = "1" ] && pass "marker-write fails: exit 1" || fail "marker-write fails: rc=$rc $(cat "$T/run.out")"
[ "$(reported "$T")" = "$(cat "$T/c2")" ] && pass "marker-write fails: binary stays installed" || fail "marker-write fails: in force $(reported "$T")"
[ "$(last_receipt "$T" event)" = "failed" ] && [ "$(last_receipt "$T" reason)" = "marker-write" ] \
  && pass "marker-write fails: failed/marker-write receipt" || fail "marker-write fails: receipt $(last_receipt "$T" event)/$(last_receipt "$T" reason)"
[ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed $C2_FULL $(git -C "$T/rig" rev-parse HEAD~1) marker-write" ] \
  && pass "marker-write fails: RESULT line" || fail "marker-write fails: RESULT line $(tail -1 "$T/run.out")"
grep -q -- "-s high .*--fingerprint install-gt:marker-write-failed" "$T/gt.log" \
  && pass "marker-write fails: HIGH escalation" || fail "marker-write fails: gt.log $(cat "$T/gt.log" 2>/dev/null)"

# --- Case 16: git fetch/merge run with auto gc/maintenance off, so no
# detached 'gc --auto' inherits the lock fd and holds install-gt.lock. ---
T=$(make_world)
REAL_GIT=$(command -v git)
cat > "$T/stubs/git" <<GIT
#!/usr/bin/env bash
echo "git \$*" >> "\$T_WORLD/git.log"
exec "$REAL_GIT" "\$@"
GIT
chmod +x "$T/stubs/git"
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
rc=$(run_install "$T" --sha "$C2_FULL" --source manual)
[ "$rc" = "0" ] && pass "no auto gc: exit 0" || fail "no auto gc: exit $rc: $(cat "$T/run.out")"
for sub in fetch merge; do
  line=$(grep -a " $sub " "$T/git.log" | head -1 || true)
  case "$line" in
    *"-c gc.auto=0 -c maintenance.auto=false $sub "*) pass "no auto gc: $sub disables auto gc/maintenance" ;;
    *) fail "no auto gc: $sub line: '$line'" ;;
  esac
done

# --- Case 17: gt-aigsx — gt-vya0s can leave the installed binary OS-immutable
# (chflags uchg / chattr +i) between installs; rollback must still be able to
# replace it. `chattr +i` silently no-ops without CAP_LINUX_IMMUTABLE (e.g.
# non-root CI on Linux), so probe for real enforcement first — asserting a
# rollback success the host couldn't actually have tested would be a false
# PASS, worse than skipping (mirrors install-binary_test.sh's own probe). ---
immutable_supported() {
  local probe ok=1
  probe="$(mktemp)"
  if chflags uchg "$probe" 2>/dev/null; then
    ok=0
    chflags nouchg "$probe" 2>/dev/null || true
  elif chattr +i "$probe" 2>/dev/null; then
    ok=0
    chattr -i "$probe" 2>/dev/null || true
  fi
  rm -f "$probe"
  return "$ok"
}

if immutable_supported; then
  T=$(make_world)
  touch "$T/simulate_immutable_replace"
  echo deadbee > "$T/new_commit"
  chflags uchg "$T/bin/gt" 2>/dev/null || chattr +i "$T/bin/gt" 2>/dev/null || true
  rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
  [ "$rc" = "1" ] && grep -q "escalate .*cannot verify what came into force.*--fingerprint install-gt:smoke-failed" "$T/gt.log" \
    && grep -q -- "-s high" "$T/gt.log" \
    && pass "immutable binary: rollback recovers (HIGH smoke-failed, not CRITICAL rollback-failed)" \
    || fail "immutable binary: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"
  [ "$(reported "$T")" = "$(cat "$T/c1")" ] \
    && pass "immutable binary: c1 restored despite the flag" \
    || fail "immutable binary: in force $(reported "$T")"
  rc2=0
  ( printf 'STUB\n' > "$T/bin/gt" ) 2>/dev/null || rc2=$?
  [ "$rc2" -ne 0 ] \
    && pass "immutable binary: rollback re-set the flag on the restored binary" \
    || fail "immutable binary: flag not re-set after rollback"
  chflags nouchg "$T/bin/gt" 2>/dev/null || true
  chattr -i "$T/bin/gt" 2>/dev/null || true
else
  echo "  SKIP: OS immutable-flag enforcement unavailable on this host (needs BSD chflags, or chattr with CAP_LINUX_IMMUTABLE) — gt-aigsx rollback check not run"
fi

# --- Case 17: a caller that already names an actor (rebuild-gt runs as the
# daemon) keeps it for every gt the install runs (gt-kyik6). ---
T=$(make_world)
rc=$(CALLER_ACTOR=daemon run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
[ "$rc" = "0" ] && [ "$(actors "$T")" = "daemon" ] \
  && pass "caller actor: every gt ran as BD_ACTOR=daemon" \
  || fail "caller actor: rc=$rc gt actors '$(actors "$T")'"

# --- Case 18: signing (gt-426fo.1). install-gt signs the binary install-local
# installed, before the smoke check runs it, so the installed gt keeps the
# identifier the Developer Tools grant is keyed on; missing material or a failing
# codesign is one WARN and an ad-hoc install, never a failure or a rollback. ---

# codesign only finds an identity in a keychain the user's search list carries
# (probed on macOS: --keychain alone, by name or by hash, answers "no identity
# found"), so the throwaway keychain below goes on that list for the case and
# comes back off whatever the case does. The list is one global per user, shared
# with every other run of this script (a post-land, a tier sweep, a presubmit)
# and with the town's real signing keychain, so the case never snapshots it and
# puts the snapshot back: a stale snapshot re-adds another run's already-deleted
# keychain and drops a real one. It takes a machine-wide lock for as long as its
# keychain is on the list, reads the list at the moment it changes it, and
# removes only its own entry (gt-52lxg). A lock it cannot get makes the signing
# cases SKIP, never fail.
KC_LOCK="${GT_TEST_KEYCHAIN_LOCK:-/tmp/gt-install-test-keychain.lock}"
KC_LOCK_HELD=0
SIGN_KC=""

# kc_lock — take the lock, waiting up to 4 minutes. A lock whose owner is gone
# (or that never got an owner file and is minutes old) is stale and reaped.
kc_lock() {
  [ "$KC_LOCK_HELD" = "1" ] && return 0
  local waited=0 owner
  while ! mkdir "$KC_LOCK" 2>/dev/null; do
    owner=$(cat "$KC_LOCK/pid" 2>/dev/null || true)
    if { [ -n "$owner" ] && ! kill -0 "$owner" 2>/dev/null; } \
       || { [ -z "$owner" ] && [ -n "$(find "$KC_LOCK" -maxdepth 0 -mmin +2 2>/dev/null)" ]; }; then
      rm -rf "$KC_LOCK"
      continue
    fi
    waited=$((waited + 1))
    [ "$waited" -gt 240 ] && return 1
    sleep 1
  done
  echo $$ > "$KC_LOCK/pid"
  KC_LOCK_HELD=1
}

kc_unlock() {
  [ "$KC_LOCK_HELD" = "1" ] || return 0
  rm -rf "$KC_LOCK"
  KC_LOCK_HELD=0
}

# kc_list prints the user's search list, one path per line.
kc_list() {
  security list-keychains -d user 2>/dev/null | tr -d '"' | sed 's/^[[:space:]]*//'
}

# kc_add KC — append KC to the list as it is right now.
kc_add() {
  local kc="$1" line
  local -a cur=()
  while IFS= read -r line; do [ -n "$line" ] && cur+=("$line"); done < <(kc_list)
  security list-keychains -d user -s ${cur[@]+"${cur[@]}"} "$kc"
}

# kc_remove KC — drop KC from the list as it is right now and nothing else. The
# list spells /var as /private/var, so the two compare equal.
kc_remove() {
  local kc="${1#/private}" line
  local -a keep=()
  while IFS= read -r line; do
    [ -n "$line" ] && [ "${line#/private}" != "$kc" ] && keep+=("$line")
  done < <(kc_list)
  security list-keychains -d user -s ${keep[@]+"${keep[@]}"}
}

restore_keychains() {
  if [ -n "$SIGN_KC" ]; then
    kc_remove "$SIGN_KC" 2>/dev/null || true
    security delete-keychain "$SIGN_KC" 2>/dev/null || true
  fi
  kc_unlock
}
trap 'restore_keychains; cleanup_world_template' EXIT

# make_signing_material T PASSWORD — a throwaway self-signed code-signing
# identity named gastown-test-unattended in $T/signing/gastown-signing.keychain-db,
# with its PASSWORD in the 0600 file beside it, as the installer expects. 0 on
# success; anything missing (no security/codesign/openssl, or an openssl whose
# PKCS#12 `security import` refuses) returns 1 so the case can SKIP.
make_signing_material() {
  local t="$1" pw="$2" kc="$1/signing/gastown-signing.keychain-db"
  command -v security >/dev/null 2>&1 && command -v codesign >/dev/null 2>&1 \
    && command -v openssl >/dev/null 2>&1 || return 1
  kc_lock || return 1
  if [ -n "$SIGN_KC" ]; then
    kc_remove "$SIGN_KC" 2>/dev/null || true
    security delete-keychain "$SIGN_KC" 2>/dev/null || true
  fi
  SIGN_KC=""
  mkdir -p "$t/signing"
  security create-keychain -p "$pw" "$kc" || return 1
  SIGN_KC="$kc"
  security unlock-keychain -p "$pw" "$kc" || return 1
  openssl req -x509 -newkey rsa:2048 -sha256 -days 2 -nodes \
    -keyout "$t/signing/test.key" -out "$t/signing/test.crt" \
    -subj "/CN=gastown-test-unattended" \
    -addext "basicConstraints=critical,CA:FALSE" \
    -addext "keyUsage=critical,digitalSignature" \
    -addext "extendedKeyUsage=critical,codeSigning" 2>/dev/null || return 1
  # OpenSSL 3 writes a PKCS#12 (AES-256/PBKDF2) that `security import` refuses
  # with "MAC verification failed"; -legacy restores the SHA1/3DES encoding it
  # takes. macOS's own LibreSSL has no -legacy and already writes that encoding.
  openssl pkcs12 -export -legacy -out "$t/signing/test.p12" -inkey "$t/signing/test.key" \
    -in "$t/signing/test.crt" -passout "pass:$pw" -name gastown-test-unattended 2>/dev/null \
    || openssl pkcs12 -export -out "$t/signing/test.p12" -inkey "$t/signing/test.key" \
         -in "$t/signing/test.crt" -passout "pass:$pw" -name gastown-test-unattended 2>/dev/null \
    || return 1
  security import "$t/signing/test.p12" -k "$kc" -P "$pw" \
    -T /usr/bin/codesign -T /usr/bin/security >/dev/null 2>&1 || return 1
  security set-key-partition-list -S apple-tool:,apple: -s -k "$pw" "$kc" >/dev/null 2>&1 || return 1
  printf '%s' "$pw" > "$t/signing/keychain.pass"
  chmod 600 "$t/signing/keychain.pass"
  # Appended, so the case never changes which keychain answers a lookup first
  # for anything else on the machine while it runs.
  kc_add "$kc" || return 1
  return 0
}

SIGN_PW="signtest-pw-4a2f"
T=$(make_world)
if make_signing_material "$T" "$SIGN_PW"; then
  rc=$(INSTALL_GT_SIGN_IDENTITY=gastown-test-unattended run_install "$T" --sha "$(cat "$T/c2")" --source manual)
  [ "$rc" = "0" ] && pass "signed install: exit 0" || fail "signed install: rc=$rc $(cat "$T/run.out")"
  [ "$(codesign -dv "$T/bin/gt" 2>&1 | sed -n 's/^Identifier=//p')" = "com.gastown.gt" ] \
    && pass "signed install: installed gt reports Identifier=com.gastown.gt" \
    || fail "signed install: identifier '$(codesign -dv "$T/bin/gt" 2>&1 | tr '\n' ' ')'"
  [ "$(reported "$T")" = "$(cat "$T/c2")" ] && pass "signed install: signed gt still passes the smoke check" \
    || fail "signed install: in force $(reported "$T")"
  grep -q "Signed .* as com.gastown.gt" "$T/run.out" && pass "signed install: logged the signature" \
    || fail "signed install: no signing line: $(cat "$T/run.out")"
  [ -z "$(escalations "$T")" ] && pass "signed install: no escalation" \
    || fail "signed install: escalated: $(escalations "$T")"
  for f in run.out make.log gt.log; do
    if grep -q "$SIGN_PW" "$T/$f" 2>/dev/null; then fail "signed install: the password reached $f"; else pass "signed install: password never in $f"; fi
  done

  # A keychain that holds no identity by the name the caller asked for: codesign
  # fails, and that too is a WARN with the install carrying on ad-hoc.
  T=$(make_world)
  make_signing_material "$T" "$SIGN_PW" || true
  rc=$(INSTALL_GT_SIGN_IDENTITY=gastown-absent run_install "$T" --sha "$(cat "$T/c2")" --source manual)
  [ "$rc" = "0" ] && [ "$(reported "$T")" = "$(cat "$T/c2")" ] \
    && pass "codesign fails: exit 0, install proceeds ad-hoc" \
    || fail "codesign fails: rc=$rc in force $(reported "$T" 2>/dev/null)"
  grep -q "WARNING: installing ad-hoc: codesign failed: .*no identity found" "$T/run.out" \
    && pass "codesign fails: WARN names the failure" || fail "codesign fails: $(grep WARNING "$T/run.out")"
  [ -z "$(escalations "$T")" ] && pass "codesign fails: no escalation, no rollback" \
    || fail "codesign fails: escalated: $(escalations "$T")"

  # The OS-immutable flag install-binary.sh leaves on the installed binary must
  # not stop the signing rename — sign_binary takes it off, replaces the binary,
  # and puts it back.
  if immutable_supported; then
    T=$(make_world)
    touch "$T/simulate_immutable_replace"
    make_signing_material "$T" "$SIGN_PW" || true
    rc=$(INSTALL_GT_SIGN_IDENTITY=gastown-test-unattended run_install "$T" --sha "$(cat "$T/c2")" --source manual)
    [ "$rc" = "0" ] && pass "signed install (immutable): exit 0" \
      || fail "signed install (immutable): rc=$rc $(cat "$T/run.out")"
    [ "$(codesign -dv "$T/bin/gt" 2>&1 | sed -n 's/^Identifier=//p')" = "com.gastown.gt" ] \
      && pass "signed install (immutable): signed despite the flag" \
      || fail "signed install (immutable): identifier '$(codesign -dv "$T/bin/gt" 2>&1 | tr '\n' ' ')'"
    rc2=0
    ( printf 'STUB\n' > "$T/bin/gt" ) 2>/dev/null || rc2=$?
    [ "$rc2" -ne 0 ] && pass "signed install (immutable): flag back on after signing" \
      || fail "signed install (immutable): flag lost"
    chflags nouchg "$T/bin/gt" 2>/dev/null || chattr -i "$T/bin/gt" 2>/dev/null || true
  else
    echo "  SKIP: OS immutable-flag enforcement unavailable on this host — the signed-install-under-immutable-flag check was not run"
  fi
else
  echo "  SKIP: could not build a throwaway code-signing keychain on this host — the signed-install checks were not run"
fi

# --- Case 19: no signing material -> one WARN naming the missing path, and the
# install proceeds ad-hoc. ---
T=$(make_world)
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && [ "$(reported "$T")" = "$(cat "$T/c2")" ] \
  && pass "no signing material: exit 0, installed ad-hoc" || fail "no signing material: rc=$rc"
[ "$(grep -c "WARNING: installing ad-hoc: no keychain at $T/signing/gastown-signing.keychain-db" "$T/run.out")" = "1" ] \
  && pass "no signing material: one WARN naming the missing keychain" \
  || fail "no signing material: $(grep WARNING "$T/run.out")"

T=$(make_world)
mkdir -p "$T/signing"
: > "$T/signing/gastown-signing.keychain-db"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && [ "$(grep -c "WARNING: installing ad-hoc: no password file at $T/signing/keychain.pass" "$T/run.out")" = "1" ] \
  && pass "no password file: one WARN naming it, install proceeds" \
  || fail "no password file: rc=$rc $(grep WARNING "$T/run.out")"

# --- Case 20: not a keychain at all -> unlock fails, still just a WARN. ---
T=$(make_world)
mkdir -p "$T/signing"
printf 'not a keychain\n' > "$T/signing/gastown-signing.keychain-db"
printf 'pw' > "$T/signing/keychain.pass"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && [ "$(reported "$T")" = "$(cat "$T/c2")" ] \
  && pass "unlock fails: exit 0, installed ad-hoc" || fail "unlock fails: rc=$rc"
grep -q "WARNING: installing ad-hoc: cannot unlock $T/signing/gastown-signing.keychain-db" "$T/run.out" \
  && pass "unlock fails: WARN names the keychain" || fail "unlock fails: $(grep WARNING "$T/run.out")"
[ -z "$(escalations "$T")" ] && pass "unlock fails: no escalation, no rollback" \
  || fail "unlock fails: escalated: $(escalations "$T")"

# --- Case 21: the signing cases leave no trace on the user's keychain search
# list (gt-52lxg). Their keychain comes off the list when the cases end, the
# lock goes with it, and a second run may take the list straight away; a
# snapshot-and-restore here once dropped other runs' entries and re-added deleted
# ones, which is what made "no identity found" intermittent. ---
if [ -n "$SIGN_KC" ]; then
  restore_keychains
  if kc_list | grep -q -F "${SIGN_KC#/private}"; then
    fail "keychain list: the signing cases left $SIGN_KC on the search list"
  elif [ -e "$KC_LOCK" ] && [ "$(cat "$KC_LOCK/pid" 2>/dev/null)" = "$$" ]; then
    fail "keychain list: the signing cases left the lock held"
  else
    pass "keychain list: own keychain off the search list, lock released"
  fi
else
  echo "  SKIP: no signing material was made on this host — keychain search-list cleanup check not run"
fi

# --- Cases 22-26: the OpenShell gateway's launchd agent. The home is a temp
# dir and launchctl a stub, so nothing here touches the real LaunchAgents. ---
GW_LABEL="com.gastown.openshell-gateway"
GW_UID=$(id -u)
gw_world() { local t; t=$(make_world); mkdir -p "$t/home/.config/openshell"; echo "$t"; }
gw_toml() { : > "$1/home/.config/openshell/gateway.toml"; }
gw_plist() { echo "$1/home/Library/LaunchAgents/$GW_LABEL.plist"; }
gw_never_kickstarts() { ! grep -q kickstart "$1/launchctl.log" 2>/dev/null; }

# Case 22: dry-run without the operator's gateway.toml -> nothing to install.
T=$(gw_world)
rc=$(run_install "$T" --dry-run)
[ "$rc" = "0" ] && grep -q "gateway: skipped, no $T/home/.config/openshell/gateway.toml" "$T/run.out" \
  && ! grep -q "bootstrap" "$T/run.out" && [ ! -e "$T/make.log" ] && [ ! -e "$(gw_plist "$T")" ] \
  && pass "gateway dry-run, no toml: skipped, no bootstrap line, nothing built or written" \
  || fail "gateway dry-run, no toml: rc=$rc $(cat "$T/run.out")"

# Case 23: dry-run with the toml, unit not loaded -> the bootstrap line, no change.
T=$(gw_world); gw_toml "$T"
rc=$(run_install "$T" --dry-run)
[ "$rc" = "0" ] && grep -q "would run: launchctl bootstrap gui/$GW_UID $(gw_plist "$T")\$" "$T/run.out" \
  && [ ! -e "$(gw_plist "$T")" ] && [ ! -e "$T/make.log" ] && ! grep -q "bootstrap " "$T/launchctl.log" \
  && gw_never_kickstarts "$T" \
  && pass "gateway dry-run, toml present: prints the bootstrap line, writes and runs nothing" \
  || fail "gateway dry-run, toml present: rc=$rc $(cat "$T/run.out") / $(cat "$T/launchctl.log" 2>/dev/null)"

# Case 24: dry-run with the unit already loaded and current -> no write, no
# bootstrap line. A missing or stale unit also prints no bootstrap line (it
# prints "would write" instead), so the case proves the noop install wrote the
# unit and that the dry run asked launchd before asserting silence.
T=$(gw_world); gw_toml "$T"; touch "$T/gw_loaded"
rc=$(run_install "$T" --sha "$(cat "$T/c1")" --source manual) # noop install writes the plist
P=$(gw_plist "$T")
[ "$rc" = "0" ] && [ -f "$P" ] && pass "gateway loaded: the noop install wrote the unit" \
  || fail "gateway loaded: rc=$rc, unit at $P: $(cat "$T/run.out")"
rm -f "$T/launchctl.log"
rc=$(run_install "$T" --dry-run)
[ "$rc" = "0" ] && ! grep -q "gateway: would write" "$T/run.out" \
  && ! grep -q "would run: launchctl bootstrap" "$T/run.out" \
  && grep -q "^launchctl print gui/$GW_UID/$GW_LABEL\$" "$T/launchctl.log" \
  && gw_never_kickstarts "$T" \
  && pass "gateway dry-run, loaded and current: no write, no bootstrap line, launchd asked" \
  || fail "gateway dry-run, loaded and current: rc=$rc $(cat "$T/run.out") / $(cat "$T/launchctl.log" 2>/dev/null)"

# Case 25: a real install renders a valid unit, bootstraps it and never kickstarts.
T=$(gw_world); gw_toml "$T"
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
P=$(gw_plist "$T")
[ "$rc" = "0" ] && [ -f "$P" ] && pass "gateway install: exit 0, unit written" || fail "gateway install: rc=$rc $(cat "$T/run.out")"
if command -v plutil >/dev/null 2>&1; then
  plutil -lint "$P" >/dev/null 2>&1 && pass "gateway install: rendered unit passes plutil -lint" || fail "gateway install: plutil -lint: $(plutil -lint "$P" 2>&1)"
else
  echo "  SKIP: no plutil on this host — plist lint not run"
fi
! grep -q "__HOME__" "$P" && grep -q "$T/home/.config/openshell/gateway.toml" "$P" \
  && grep -q "$T/home/.local/state/openshell/tls" "$P" && grep -q "$T/home/gt/.runtime/openshell/gateway.log" "$P" \
  && pass "gateway install: unit rendered with HOME" || fail "gateway install: unit paths: $(cat "$P")"
python3 -c '
import plistlib, sys
d = plistlib.load(open(sys.argv[1], "rb"))
e = d["EnvironmentVariables"]
assert e["OPENSHELL_TELEMETRY_ENABLED"] == "false", e
assert e["OPENSHELL_LOCAL_TLS_DIR"].endswith("/.local/state/openshell/tls"), e
assert d["KeepAlive"] is True, d
assert d["Label"] == "com.gastown.openshell-gateway", d
assert d["ProgramArguments"][0] == "/opt/homebrew/bin/openshell-gateway", d
' "$P" && pass "gateway install: telemetry off, local TLS dir, KeepAlive true" || fail "gateway install: unit content wrong"
[ -d "$T/home/gt/.runtime/openshell" ] && pass "gateway install: log directory exists" || fail "gateway install: no log directory"
grep -q "^launchctl bootstrap gui/$GW_UID $P\$" "$T/launchctl.log" && gw_never_kickstarts "$T" \
  && pass "gateway install: bootstrapped, never kickstarted" || fail "gateway install: launchctl log: $(cat "$T/launchctl.log")"

# Case 26: unit already loaded and its file stale -> rewritten, not bootstrapped
# and not kickstarted; the log names the kickstart for the operator.
T=$(gw_world); gw_toml "$T"; touch "$T/gw_loaded"
install_stub "$T" "$(cat "$T/c2")"
mkdir -p "$(dirname "$(gw_plist "$T")")"; echo stale > "$(gw_plist "$T")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && ! grep -q "stale" "$(gw_plist "$T")" && ! grep -q "bootstrap " "$T/launchctl.log" && gw_never_kickstarts "$T" \
  && grep -q "keeps its old unit until: launchctl kickstart -k gui/$GW_UID/$GW_LABEL" "$T/run.out" \
  && pass "gateway install, loaded: unit rewritten, no bootstrap, no kickstart, operator told how" \
  || fail "gateway install, loaded: rc=$rc $(cat "$T/run.out") / $(cat "$T/launchctl.log")"

# Case 27: no gateway.toml on a real install -> no unit, launchctl untouched.
T=$(gw_world)
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && [ ! -e "$(gw_plist "$T")" ] && [ ! -e "$T/launchctl.log" ] \
  && pass "gateway install, no toml: no unit written, launchctl never called" \
  || fail "gateway install, no toml: rc=$rc $(cat "$T/run.out")"

# gw_lib T ARGS... -> igt_gateway_agent's exit code, run from the sourced lib;
# output in $T/lib.out. Its launchctl is the stub, as in run_install.
gw_lib() {
  local t="$1" rc=0; shift
  ( export T_WORLD="$t" PATH="$t/stubs:/usr/bin:/bin:/opt/homebrew/bin:/usr/sbin:/sbin"
    . "$SCRIPT_DIR/lib/install-gt-lib.sh"
    igt_gateway_agent "$@" ) > "$t/lib.out" 2>&1 || rc=$?
  echo "$rc"
}

# Case 28: a file sits where the LaunchAgents directory goes, so it cannot be
# made -> one WARNING, no unit, no bootstrap, and the install still succeeds.
T=$(gw_world); gw_toml "$T"
install_stub "$T" "$(cat "$T/c2")"
: > "$T/home/Library"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && grep -q "WARNING: gateway: cannot create $T/home/Library/LaunchAgents" "$T/run.out" \
  && [ ! -e "$(gw_plist "$T")" ] && ! grep -q "^launchctl bootstrap" "$T/launchctl.log" 2>/dev/null \
  && pass "gateway install, cannot make its directory: one WARNING, no unit, no bootstrap" \
  || fail "gateway install, cannot make its directory: rc=$rc $(cat "$T/run.out") / $(cat "$T/launchctl.log" 2>/dev/null)"

# Case 29: the LaunchAgents directory exists but is read-only -> the unit is not
# written, no temporary file is left behind, one WARNING. A root run writes
# through any mode, so the case is skipped there.
if [ "$(id -u)" = "0" ]; then
  echo "  SKIP: running as root — a read-only directory is still writable, write-failure case not run"
else
  T=$(gw_world); gw_toml "$T"
  install_stub "$T" "$(cat "$T/c2")"
  LA_DIR="$(dirname "$(gw_plist "$T")")"
  mkdir -p "$LA_DIR"; chmod a-w "$LA_DIR"
  rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
  chmod u+w "$LA_DIR"
  [ "$rc" = "0" ] && grep -q "WARNING: gateway: cannot write $(gw_plist "$T")" "$T/run.out" \
    && [ ! -e "$(gw_plist "$T")" ] && [ -z "$(ls -A "$LA_DIR")" ] && ! grep -q "^launchctl bootstrap" "$T/launchctl.log" 2>/dev/null \
    && pass "gateway install, unwritable directory: one WARNING, no unit, no temp file, no bootstrap" \
    || fail "gateway install, unwritable directory: rc=$rc $(cat "$T/run.out") / $(ls -A "$LA_DIR")"
fi

# Case 30: launchctl bootstrap fails -> the unit stays, one WARNING names the
# failed bootstrap, nothing is kickstarted, and the install still exits 0.
T=$(gw_world); gw_toml "$T"; touch "$T/gw_bootstrap_fails"
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source manual)
[ "$rc" = "0" ] && [ -f "$(gw_plist "$T")" ] \
  && grep -q "WARNING: gateway: launchctl bootstrap gui/$GW_UID $(gw_plist "$T") failed" "$T/run.out" \
  && ! grep -q "gateway: bootstrapped" "$T/run.out" && gw_never_kickstarts "$T" \
  && pass "gateway install, bootstrap fails: unit kept, WARNING names it, never kickstarted" \
  || fail "gateway install, bootstrap fails: rc=$rc $(cat "$T/run.out") / $(cat "$T/launchctl.log")"

# Case 31: the template is missing -> one WARNING, no unit, nothing loaded.
# install-gt.sh falls back to its own clone's copy, so this branch is driven
# through the library directly.
T=$(gw_world); gw_toml "$T"
rc=$(gw_lib "$T" install "$T/no-such.plist.tmpl" "$T/home")
[ "$rc" = "0" ] && grep -q "WARNING: gateway: no template at $T/no-such.plist.tmpl" "$T/lib.out" \
  && [ ! -e "$(gw_plist "$T")" ] && ! grep -q "^launchctl bootstrap" "$T/launchctl.log" 2>/dev/null \
  && pass "gateway lib, no template: one WARNING, no unit, no bootstrap" \
  || fail "gateway lib, no template: rc=$rc $(cat "$T/lib.out")"

# Case 32: the template is not text (invalid UTF-8) -> rendering fails, one
# WARNING, nothing written or loaded.
T=$(gw_world); gw_toml "$T"
printf '\377\376 not text\n' > "$T/bad.plist.tmpl"
rc=$(gw_lib "$T" install "$T/bad.plist.tmpl" "$T/home")
[ "$rc" = "0" ] && grep -q "WARNING: gateway: could not render $T/bad.plist.tmpl" "$T/lib.out" \
  && [ ! -e "$(gw_plist "$T")" ] && ! grep -q "^launchctl bootstrap" "$T/launchctl.log" 2>/dev/null \
  && pass "gateway lib, unrenderable template: one WARNING, no unit, no bootstrap" \
  || fail "gateway lib, unrenderable template: rc=$rc $(cat "$T/lib.out")"


# --- Cases 33-38: the polecat image rebuild after an install. The plugin's run.sh is
# a stub that logs its call; the default in run_install is a path that does not
# exist, so no other case can start the real plugin. The rebuild is detached, so
# the cases wait for the stub's log.
# image_stub T [sleep seconds]: a run.sh that logs its call, and with a non-zero
# sleep records its pid and lingers until the case kills it.
image_stub() {
  local t="$1" linger="${2:-0}"
  mkdir -p "$t/plugin"
  cat > "$t/plugin/run.sh" <<STUB
#!/usr/bin/env bash
echo "run.sh \$("$t/bin/gt" version | head -1)" >> "$t/image-calls.log"
if [ $linger -gt 0 ]; then echo \$\$ > "$t/image.pid"; exec sleep $linger; fi
exit \${IMAGE_STUB_EXIT:-0}
STUB
  chmod +x "$t/plugin/run.sh"
}
wait_for() { local i; for i in $(seq 1 50); do [ -e "$1" ] && return 0; sleep 0.1; done; return 1; }
wait_for_alarm() { local i; for i in $(seq 1 100); do grep -q 'alarm after' "$1" 2>/dev/null && return 0; sleep 0.1; done; return 1; }
# proc_state PID: the one-letter state of PID (R, S, Z ...); empty once it is gone.
# PROC_ROOT stands in for /proc, so both readers can be checked on either host.
proc_state() {
  local stat="${PROC_ROOT:-/proc}/$1/stat"
  if [ -r "$stat" ]; then sed 's/.*) //' "$stat" 2>/dev/null | cut -c1
  else ps -o stat= -p "$1" 2>/dev/null | cut -c1
  fi
}
# is_live PID: PID is a running process. kill -0 also finds a zombie (exited, not
# yet reaped), and an orphan whose PID 1 never reaps becomes one, so that is not live.
is_live() { kill -0 "$1" 2>/dev/null && [ "$(proc_state "$1")" != Z ]; }

# Case 33: a successful install starts the plugin once, after the new binary is in place.
T=$(make_world); image_stub "$T"
install_stub "$T" "$(cat "$T/c1")"
rc=$(IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
wait_for "$T/image-calls.log" || true
[ "$rc" = "0" ] && [ "$(wc -l < "$T/image-calls.log" 2>/dev/null | tr -d ' ')" = "1" ] \
  && grep -q "gt version $(cat "$T/c2")" "$T/image-calls.log" \
  && grep -q "polecat image: rebuild for .* started in the background" "$T/run.out" \
  && pass "image: a successful install runs the plugin once, on the new binary" \
  || fail "image: success: rc=$rc calls=$(cat "$T/image-calls.log" 2>/dev/null) out=$(tail -4 "$T/run.out")"

# Case 34: a no-op install (already at the commit) does not start it.
T=$(make_world); image_stub "$T"
install_stub "$T" "$(cat "$T/c2")"
rc=$(IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
sleep 0.5
[ "$rc" = "0" ] && [ ! -e "$T/image-calls.log" ] \
  && pass "image: a no-op install does not run the plugin" \
  || fail "image: noop: rc=$rc calls=$(cat "$T/image-calls.log" 2>/dev/null)"

# Case 35: a failed install (build fails) does not start it.
T=$(make_world); image_stub "$T"
touch "$T/fail_build"
rc=$(IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
sleep 0.5
[ "$rc" = "1" ] && [ ! -e "$T/image-calls.log" ] \
  && pass "image: a failed install does not run the plugin" \
  || fail "image: failed install: rc=$rc calls=$(cat "$T/image-calls.log" 2>/dev/null)"

# Case 36: no plugin on disk -> the install succeeds and says the image was not rebuilt.
T=$(make_world)
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
[ "$rc" = "0" ] && grep -q "polecat image: no plugin at '$T/no-image-plugin/run.sh', not rebuilt" "$T/run.out" \
  && [ "$(tail -1 "$T/run.out" | cut -d' ' -f2-3)" = "RESULT installed" ] \
  && pass "image: no plugin: install succeeds, one line says so" \
  || fail "image: no plugin: rc=$rc $(tail -3 "$T/run.out")"

# Case 37: the plugin fails -> the install is still a success (best effort).
T=$(make_world); image_stub "$T"
rc=$(IMAGE_STUB_EXIT=1 IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
wait_for "$T/image-calls.log" || true
[ "$rc" = "0" ] && [ "$(tail -1 "$T/run.out" | cut -d' ' -f2-3)" = "RESULT installed" ] && [ "$(last_receipt "$T" event)" = "installed" ] \
  && pass "image: a failing plugin does not fail the install" \
  || fail "image: plugin fails: rc=$rc $(tail -2 "$T/run.out")"

# Case 38: a plugin that runs for a long time does not hold the install. The
# stub records its pid and sleeps until this case kills it, so nothing is
# timed: the install returning while that pid is alive is the assertion.
T=$(make_world); image_stub "$T" 10
rc=$(IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt | tail -1)
wait_for "$T/image.pid" || true
STUB_PID=$(cat "$T/image.pid" 2>/dev/null || true)
[ "$rc" = "0" ] && [ -n "$STUB_PID" ] && kill -0 "$STUB_PID" 2>/dev/null \
  && pass "image: a slow plugin does not delay the install" \
  || fail "image: slow plugin: rc=$rc pid='${STUB_PID:-}' $(tail -2 "$T/run.out")"
# The stub had closed every fd above 2 by the time it wrote its pid (perl closes
# them before exec), so the lock is free now unless the stub inherited it.
LOCK_RC=$( ( perl -e 'use Fcntl qw(:flock); open(my $f, ">>", $ARGV[0]) or exit 2; exit(flock($f, LOCK_EX|LOCK_NB) ? 0 : 1)' "$T/daemon/install-gt.lock" ) >/dev/null 2>&1; echo $? )
[ "$LOCK_RC" = "0" ] \
  && pass "image: the background plugin does not hold the install lock" \
  || fail "image: the install lock is held by the background plugin (rc=$LOCK_RC)"
if [ -n "$STUB_PID" ]; then kill "$STUB_PID" 2>/dev/null || true; fi

# --- Case 39: INSTALL_DIR names the install dir (gt-tcigd). It is the
# Makefile's name for it and the one the dispatcher's host-safety prompt names
# to test writers; while the script ignored it, a test that followed the prompt
# installed into $HOME/.local/bin. INSTALL_DIR is a mktemp -d here and HOME is
# the helper's temp home, pre-created with its .local/bin so the fallback the
# bug took lands somewhere this case can see instead of failing earlier. ---
T=$(make_world)
BINDIR=$(mktemp -d)
mkdir -p "$T/home-default/.local/bin"
rc=$(run_install_at "$T" "INSTALL_DIR=$BINDIR" --sha "$(cat "$T/c2")")
[ "$rc" = "0" ] && [ "$(tail -1 "$T/run.out" | cut -d' ' -f2-3)" = "RESULT installed" ] \
  && pass "INSTALL_DIR: exit 0, installed" || fail "INSTALL_DIR: rc=$rc: $(tail -3 "$T/run.out")"
grep -q "C=\"$(cat "$T/c2")\"" "$BINDIR/gt" 2>/dev/null \
  && pass "INSTALL_DIR: the binary landed there" || fail "INSTALL_DIR: nothing usable at $BINDIR/gt"
[ -z "$(ls -A "$T/home-default/.local/bin" 2>/dev/null)" ] \
  && pass "INSTALL_DIR: the default bin dir was untouched" || fail "INSTALL_DIR: wrote $(ls -A "$T/home-default/.local/bin" 2>&1)"
grep -q "make SKIP_UPDATE_CHECK=1 INSTALL_DIR=$BINDIR install-local" "$T/make.log" \
  && pass "INSTALL_DIR: install-local was pointed at it" || fail "INSTALL_DIR: make.log $(cat "$T/make.log" 2>/dev/null)"
rm -rf "$BINDIR"

# --- Case 40: INSTALL_GT_BIN_DIR, this script's own name, wins when both are
# set — a caller that pins the script's env is never redirected by an
# INSTALL_DIR it also happens to export. ---
T=$(make_world)
mkdir -p "$T/otherbin"
rc=$( export INSTALL_DIR="$T/otherbin"
      run_install "$T" --sha "$(cat "$T/c2")" )
[ "$rc" = "0" ] && grep -q "C=\"$(cat "$T/c2")\"" "$T/bin/gt" && [ ! -e "$T/otherbin/gt" ] \
  && pass "both set: INSTALL_GT_BIN_DIR wins" || fail "both set: rc=$rc, bin $(grep -o 'C="[^"]*"' "$T/bin/gt" 2>/dev/null), otherbin $(ls "$T/otherbin" 2>&1)"

# --- Case 41: perl is missing, so the detached rebuild cannot start. The
# install says so on its own line instead of reporting a start it never made,
# and still succeeds. INSTALL_GT_PERL is the seam: it names the interpreter. ---
T=$(make_world); image_stub "$T"
rc=$( export INSTALL_GT_PERL="$T/no-such-perl"
      IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt )
[ "$rc" = "0" ] && grep -q "polecat image: $T/no-such-perl is not installed" "$T/run.out" \
  && ! grep -q "started in the background" "$T/run.out" \
  && [ ! -e "$T/image-calls.log" ] \
  && pass "image: no perl: one line says so, install succeeds" \
  || fail "image: no perl: rc=$rc $(tail -3 "$T/run.out")"

# --- Case 42: --dry-run ends with the RESULT line the header contract promises
# (gt-87hlc). It changes nothing, so the commit it names is the one it was
# asked for, unresolved, and the event says so. ---
T=$(make_world)
rc=$(run_install "$T" --dry-run)
[ "$rc" = "0" ] && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT dry-run origin/main - -" ] \
  && pass "dry-run: exit 0, RESULT line last" || fail "dry-run: rc=$rc last=$(tail -1 "$T/run.out")"

# --- Case 43: run.sh is handed this install's town root, gt binary and lock,
# not the ones HOME and PATH would resolve to (gt-87hlc). These three names are
# the contract install-gt.sh documents at polecat_image_step. ---
T=$(make_world); mkdir -p "$T/plugin"
cat > "$T/plugin/run.sh" <<'STUB'
#!/usr/bin/env bash
echo "$POLECAT_IMAGE_TOWN_ROOT $POLECAT_IMAGE_GT $POLECAT_IMAGE_LOCK" > "$T_WORLD/image-env.txt"
STUB
chmod +x "$T/plugin/run.sh"
install_stub "$T" "$(cat "$T/c1")"
rc=$( export GT_TOWN_ROOT="$T"
      IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt )
wait_for "$T/image-env.txt" || true
[ "$rc" = "0" ] && [ "$(cat "$T/image-env.txt" 2>/dev/null)" = "$T $T/bin/gt $T/image.lock" ] \
  && pass "image: run.sh gets the install's town root, gt binary and lock" \
  || fail "image: run.sh env: rc=$rc got='$(cat "$T/image-env.txt" 2>/dev/null)' want='$T $T/bin/gt $T/image.lock'"

# --- Case 45: the build gets the registry settings the daemon gives its plugin,
# read from settings/config.json daemon.env, so one build is also the publish
# (gt-jmd9n). Without the file, the build runs local-only and says so. ---
T=$(make_world); mkdir -p "$T/plugin" "$T/settings"
cat > "$T/plugin/run.sh" <<'STUB'
#!/usr/bin/env bash
echo "${POLECAT_IMAGE_REGISTRY:-unset} ${POLECAT_IMAGE_CRANE:-unset}" > "$T_WORLD/image-env2.txt"
STUB
chmod +x "$T/plugin/run.sh"
cat > "$T/settings/config.json" <<'JSON'
{"daemon":{"env":{"POLECAT_IMAGE_REGISTRY":"localhost:5001","POLECAT_IMAGE_CRANE":"/crane/bin/crane"}}}
JSON
install_stub "$T" "$(cat "$T/c1")"
rc=$( export GT_TOWN_ROOT="$T"
      IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt )
wait_for "$T/image-env2.txt" || true
[ "$rc" = "0" ] && [ "$(cat "$T/image-env2.txt" 2>/dev/null)" = "localhost:5001 /crane/bin/crane" ] \
  && pass "image: run.sh gets the daemon's registry and crane settings from config.json" \
  || fail "image: registry env: rc=$rc got='$(cat "$T/image-env2.txt" 2>/dev/null)'"

T=$(make_world); mkdir -p "$T/plugin"
cat > "$T/plugin/run.sh" <<'STUB'
#!/usr/bin/env bash
echo "${POLECAT_IMAGE_REGISTRY:-unset} ${POLECAT_IMAGE_CRANE:-unset}" > "$T_WORLD/image-env2.txt"
STUB
chmod +x "$T/plugin/run.sh"
install_stub "$T" "$(cat "$T/c1")"
rc=$( export GT_TOWN_ROOT="$T"
      IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt )
wait_for "$T/image-env2.txt" || true
[ "$rc" = "0" ] && grep -q "no POLECAT_IMAGE_REGISTRY in settings/config.json" "$T/run.out" \
  && [ "$(cat "$T/image-env2.txt" 2>/dev/null)" = "unset unset" ] \
  && pass "image: with no config the build runs local-only and says so" \
  || fail "image: no-config case: rc=$rc got='$(cat "$T/image-env2.txt" 2>/dev/null)'"

# --- Case 44: the alarm stops the whole image build and then removes the lock
# run.sh took. The stub takes that lock and starts a child standing in for the
# docker work, then waits on it. INSTALL_GT_IMAGE_ALARM is one second, so the
# alarm is what ends the stub. The supervisor logs its alarm line last, after
# the kill and the lock removal, so that line is the wait: both waits are bounded. ---
T=$(make_world); mkdir -p "$T/plugin"
cat > "$T/plugin/run.sh" <<'STUB'
#!/usr/bin/env bash
mkdir "$T_WORLD/image.lock" && echo "$$" > "$T_WORLD/image.lock/pid"
sleep 30 &
echo "$!" > "$T_WORLD/image-child.pid"
wait
STUB
chmod +x "$T/plugin/run.sh"
install_stub "$T" "$(cat "$T/c1")"
rc=$( export INSTALL_GT_IMAGE_ALARM=1
      IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt )
wait_for "$T/image-child.pid" || true
CHILD=$(cat "$T/image-child.pid" 2>/dev/null || true)
wait_for_alarm "$T/image.log" || true
[ "$rc" = "0" ] && [ -n "$CHILD" ] && [ ! -e "$T/image.lock" ] && ! is_live "$CHILD" \
  && grep -q 'polecat image: alarm after 1 s stopped the build' "$T/image.log" \
  && pass "image: the alarm stops the build, removes the lock run.sh took, and says so" \
  || fail "image: alarm: rc=$rc child=${CHILD:-none} lock=$([ -e "$T/image.lock" ] && echo held || echo free) out=$(tail -2 "$T/image.log" 2>/dev/null)"

# --- Case 45: the alarm removes only a lock that names the build it stopped.
# This stub's lock names its parent (the supervisor), not its own pid, so the
# alarm stops the build and leaves that lock in place. ---
T=$(make_world); mkdir -p "$T/plugin"
cat > "$T/plugin/run.sh" <<'STUB'
#!/usr/bin/env bash
mkdir "$T_WORLD/image.lock" && echo "$PPID" > "$T_WORLD/image.lock/pid"
sleep 30 &
echo "$!" > "$T_WORLD/image-child.pid"
wait
STUB
chmod +x "$T/plugin/run.sh"
install_stub "$T" "$(cat "$T/c1")"
rc=$( export INSTALL_GT_IMAGE_ALARM=1
      IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt )
wait_for "$T/image-child.pid" || true
CHILD=$(cat "$T/image-child.pid" 2>/dev/null || true)
wait_for_alarm "$T/image.log" || true
[ "$rc" = "0" ] && [ -n "$CHILD" ] && [ -e "$T/image.lock/pid" ] && ! is_live "$CHILD" \
  && pass "image: the alarm leaves a lock that names another process" \
  || fail "image: foreign lock: rc=$rc child=${CHILD:-none} lock=$([ -e "$T/image.lock" ] && echo held || echo free) out=$(tail -2 "$T/image.log" 2>/dev/null)"

# --- Case 46: an INSTALL_GT_IMAGE_ALARM that is not a whole number above zero
# is named in the log, and the build still starts under the default bound. Zero
# matters most: perl would read it as no alarm at all. ---
for alarm in 0 soon; do
  T=$(make_world); image_stub "$T"
  rc=$( export INSTALL_GT_IMAGE_ALARM="$alarm"
        IMAGE_RUN="$T/plugin/run.sh" run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt )
  wait_for "$T/image-calls.log" || true
  [ "$rc" = "0" ] && [ -e "$T/image-calls.log" ] \
    && grep -q "INSTALL_GT_IMAGE_ALARM='$alarm' is not a whole number of seconds above zero, so the build gets 1200" "$T/run.out" \
    && pass "image: alarm '$alarm' is named and the build still starts" \
    || fail "image: alarm '$alarm': rc=$rc $(tail -2 "$T/run.out")"
done

# --- Case 47: is_live reads a zombie as gone and a running process as live. The
# python parent forks a child that exits and is never waited for, so the child is
# a zombie that kill -0 still finds. The wait for that state is bounded. The parent
# waits for the test shell to exit (checked every 0.1 s, 30 s at most), so it never
# outlives an early exit, and the case kills it on the normal path. python3 is the
# one tool this case needs; without it the case fails by name. ---
T=$(make_world)
if ! command -v python3 >/dev/null 2>&1; then
  fail "is_live: case 47 needs python3 on PATH to fork a zombie"
else
python3 -c '
import os, sys, time
shell = os.getppid()
pid = os.fork()
if pid == 0:
    os._exit(0)
open(sys.argv[1], "w").write(str(pid))
for _ in range(300):
    if os.getppid() != shell:
        break
    time.sleep(0.1)
' "$T/zombie.pid" &
PARENT=$!
ZOMBIE=""
for _ in $(seq 1 50); do
  ZOMBIE=$(cat "$T/zombie.pid" 2>/dev/null || true)
  [ -n "$ZOMBIE" ] && [ "$(proc_state "$ZOMBIE")" = Z ] && break
  sleep 0.1
done
[ -n "$ZOMBIE" ] && [ "$(proc_state "$ZOMBIE")" = Z ] && kill -0 "$ZOMBIE" 2>/dev/null \
  && ! is_live "$ZOMBIE" && is_live "$PARENT" \
  && pass "is_live: a zombie reads as gone, a running process as live" \
  || fail "is_live: zombie=${ZOMBIE:-none} state=$(proc_state "${ZOMBIE:-0}") parent_live=$(is_live "$PARENT" && echo yes || echo no)"
kill "$PARENT" 2>/dev/null || true
fi

# Case 48: proc_state reads the state from a /proc stat line when one exists.
# A fake PROC_ROOT makes this branch run on macOS as well as on Linux.
mkdir -p "$T/fakeproc/4242"
echo "4242 (sh) S 1 4242 4242 0 -1 4194304" > "$T/fakeproc/4242/stat"
[ "$(PROC_ROOT="$T/fakeproc" proc_state 4242)" = S ] \
  && pass "proc_state: reads the state from a /proc stat line" \
  || fail "proc_state: /proc branch read '$(PROC_ROOT="$T/fakeproc" proc_state 4242)', want S"

# Case 49: with no /proc stat line, proc_state asks ps. This test shell is alive,
# so ps has a state for it, and the first letter of that state is the answer.
want=$(ps -o stat= -p $$ | cut -c1)
got=$(PROC_ROOT="$T/no-proc" proc_state $$)
[ -n "$want" ] && [ "$got" = "$want" ] \
  && pass "proc_state: falls back to ps when /proc has no stat line" \
  || fail "proc_state: ps branch read '$got', want '$want'"

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all install-gt tests passed"

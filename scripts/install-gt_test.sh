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
make_world() {
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
      INSTALL_GT_SIGN_DIR="$t/signing" \
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

echo "=== install-gt.sh tests ==="

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
      INSTALL_GT_SIGN_DIR="$t/signing" \
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
trap restore_keychains EXIT

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

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all install-gt tests passed"

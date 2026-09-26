#!/usr/bin/env bash
# Tests for scripts/install-gt.sh: the one install path shared by the
# post-merge hook and rebuild-gt (claude-7fc). A stub make on PATH "installs" by
# writing a stub gt that reports a chosen commit; the stub gt answers
# stale/version/sync/escalate/slot and logs what it was asked.
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
  build) [ -e "$T_WORLD/fail_build" ] && { echo "build failed" >&2; exit 2; }; exit 0 ;;
  safe-install)
    if [ -e "$T_WORLD/already_at_head" ]; then echo "Binary is already at HEAD, nothing to do"; exit 1; fi
    [ -e "$T_WORLD/fail_install" ] && exit 2
    c=$(cat "$T_WORLD/new_commit" 2>/dev/null || git rev-parse --short HEAD)
    sed "s/__COMMIT__/$c/" "$T_WORLD/gt.tmpl" > "$inst/.gt.new"
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

# run_install T ARGS... -> exit code; output in $T/run.out
run_install() {
  local t="$1" rc=0; shift
  ( export T_WORLD="$t" INSTALL_GT_BIN_DIR="$t/bin" INSTALL_GT_DAEMON_DIR="$t/daemon" \
      INSTALL_GT_RIG_DIR="$t/rig" INSTALL_GT_LOCK_WAIT="${LOCK_WAIT:-5}" \
      PATH="$t/stubs:/usr/bin:/bin:/opt/homebrew/bin"
    bash "$INSTALLER" "$@" ) > "$t/run.out" 2>&1 || rc=$?
  echo "$rc"
}

# last_receipt T FIELD -> that field of the last receipt line ("None" if null)
last_receipt() {
  tail -1 "$1/daemon/install-receipts.jsonl" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1]))' "$2" 2>/dev/null || echo "NO-RECEIPT"
}

# reported T -> the commit the installed gt stub reports
reported() { "$1/bin/gt" stale --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["binary_commit"])'; }

echo "=== install-gt.sh tests ==="

# --- Case 1: happy path. c1 installed, merge c2 lands -> fetch, ff, build,
# install, smoke, sync, marker, receipt, RESULT line. ---
T=$(make_world)
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
rc=$(run_install "$T" --sha "$C2_FULL" --source post-merge)
[ "$rc" = "0" ] && pass "happy: exit 0" || fail "happy: exit $rc: $(cat "$T/run.out")"
[ "$(reported "$T")" = "$(cat "$T/c2")" ] && pass "happy: c2 in force" || fail "happy: in force is $(reported "$T")"
grep -q "C=\"$(cat "$T/c1")\"" "$T/bin/gt.prev" && pass "happy: gt.prev is the c1 binary" || fail "happy: gt.prev missing or wrong"
[ "$(git -C "$T/rig" rev-parse HEAD)" = "$C2_FULL" ] && pass "happy: rig fast-forwarded" || fail "happy: rig HEAD not at c2"
python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
assert m["commit"] == sys.argv[2], m
assert m["source"] == "post-merge", m
assert m["repo"] == sys.argv[3], m
assert m["requested_at"].endswith("Z"), m
' "$T/daemon/restart-pending.json" "$C2_FULL" "$T/rig" && pass "happy: marker shape" || fail "happy: marker wrong: $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"
[ "$(last_receipt "$T" event)" = "installed" ] && pass "happy: installed receipt" || fail "happy: receipt $(tail -1 "$T/daemon/install-receipts.jsonl" 2>/dev/null)"
[ "$(last_receipt "$T" commit)" = "$C2_FULL" ] && pass "happy: receipt commit is full" || fail "happy: receipt commit $(last_receipt "$T" commit)"
[ "$(last_receipt "$T" merged_at)" != "None" ] && pass "happy: receipt has merged_at" || fail "happy: no merged_at"
grep -q "sync formula" "$T/gt.log" && grep -q "sync plugin" "$T/gt.log" && pass "happy: syncs ran" || fail "happy: syncs missing: $(cat "$T/gt.log" 2>/dev/null)"
[ "$(tail -1 "$T/run.out")" = "install-gt: RESULT installed $C2_FULL $(git -C "$T/rig" rev-parse HEAD~1) -" ] && pass "happy: RESULT line" || fail "happy: RESULT line: $(tail -1 "$T/run.out")"
grep -q "make SKIP_UPDATE_CHECK=1 INSTALL_DIR=$T/bin safe-install" "$T/make.log" && pass "happy: safe-install into INSTALL_GT_BIN_DIR" || fail "happy: make.log $(cat "$T/make.log")"

# --- Case 2: already installed -> no-op: no make, no marker, noop receipt ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
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
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
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
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "1" ] && grep -q "escalate .*the install did not take" "$T/gt.log" \
  && pass "did not take: exit 1, escalated" || fail "did not take: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"

# --- Case 6: new binary cannot answer 'stale --json' -> rollback ---
T=$(make_world)
touch "$T/broken_$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
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

# --- Case 8: installed commit unreadable and safe-install says "already at
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
rc=$(LOCK_WAIT=1 run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
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
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "2" ] && [ ! -e "$T/make.log" ] && [ "$(last_receipt "$T" reason)" = "dirty" ] \
  && pass "refused dirty" || fail "refused dirty: rc=$rc $(cat "$T/run.out")"
! grep -q "escalate" "$T/gt.log" 2>/dev/null && pass "refused dirty: not escalated here" || fail "refused dirty: escalated"

T=$(make_world)
git -C "$T/rig" checkout -q -b side
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "wrong-branch" ] && pass "refused wrong branch" || fail "refused wrong branch: rc=$rc"

T=$(make_world)
git -C "$T/rig" -c user.email=t@t -c user.name=t commit -q --allow-empty -m local-only
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "diverged" ] && pass "refused diverged" || fail "refused diverged: rc=$rc $(cat "$T/run.out")"

T=$(make_world)
rc=$(run_install "$T" --sha 0123456789abcdef0123456789abcdef01234567 --source post-merge)
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
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
chmod u+w "$T/bin"
[ "$rc" = "1" ] && grep -q -- "-s critical .*--fingerprint install-gt:rollback-failed" "$T/gt.log" \
  && pass "rollback fails: CRITICAL" || fail "rollback fails: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"

# --- Case 13: bad arguments -> the RESULT-line/exit-0-3 contract holds even
# on a path that never reaches DAEMON_DIR (fix round 1: these used to be a
# bare `exit 1` with no RESULT line at all). ---
T=$(make_world)
rc=$(run_install "$T" --source post-merge)
[ "$rc" = "1" ] && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed - - unexpected" ] \
  && pass "bad args (missing --sha): exit 1, RESULT line" || fail "bad args (missing --sha): rc=$rc $(cat "$T/run.out")"

T=$(make_world)
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source bogus)
[ "$rc" = "1" ] && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed $(cat "$T/c2") - unexpected" ] \
  && pass "bad args (bad --source): exit 1, RESULT line" || fail "bad args (bad --source): rc=$rc $(cat "$T/run.out")"

# --- Case 14: perl itself dies before it ever reaches exec (the lock path is
# a pre-existing directory, so 'open >>' fails) -> its die() exit code (not
# necessarily 75, and not necessarily outside 0-3) must never leak through as
# a bare exit status with no RESULT line. DAEMON_DIR is left writable so this
# exercises perl's own failure, not a bash-level permission error. ---
T=$(make_world)
mkdir -p "$T/daemon/install-gt.lock"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "1" ] && [ "$(tail -1 "$T/run.out")" = "install-gt: RESULT failed $(cat "$T/c2") - unexpected" ] \
  && pass "lock file unopenable: mapped to exit 1 with a RESULT line" || fail "lock file unopenable: rc=$rc $(cat "$T/run.out")"

# --- Case 15: the new binary is installed and smoke-verified, but writing
# restart-pending.json fails (path is a directory) -> failed/marker-write,
# exit 1, HIGH escalation, and — unlike a build/smoke failure — no rollback:
# the good binary stays in force. ---
T=$(make_world)
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
mkdir -p "$T/daemon/restart-pending.json"
rc=$(run_install "$T" --sha "$C2_FULL" --source post-merge)
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
rc=$(run_install "$T" --sha "$C2_FULL" --source post-merge)
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
  rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
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

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all install-gt tests passed"

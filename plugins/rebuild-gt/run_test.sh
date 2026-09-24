#!/usr/bin/env bash
# End-to-end tests for rebuild-gt/run.sh with a stub gt on PATH and a fake rig.
#
# gt-htx3: the rebuild must yield to a running gate suite (make build competes
# for CPU with load-sensitive tests).
# gt-oqbw: merged is not in force until something installs. A run that
# accomplished nothing exits 3 so the next heartbeat retries it (a run record
# would spend the whole cooldown), the install is verified against the gt the
# town will actually execute, the commits brought into force are recorded, and
# the daemon was restarted last (claude-7fc: install-gt.sh now writes a
# restart marker and the daemon restarts itself at its idle point). Refusals (dirty checkout, diverged main, not
# safe to rebuild) exit 0 and do not escalate on the run that first hits them —
# they usually clear by themselves — but a due binary one of them leaves out of
# force escalates under the starvation clock like any other block. Failures
# (build failed, install did not take, installed commit unverifiable) exit 1
# and escalate under their own fingerprints.
# gt-kox0: a block that repeats must not stay silent. The defer path counts the
# block and escalates past the threshold; past it a build blocked BY the gate
# waits for the gate to release the slot and then builds inside it, while an
# install still waits for any merge that is mid-push.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_SH="$SCRIPT_DIR/run.sh"
FAILURES=0
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { echo "PASS: $*"; }

make_town() {
  local town; town=$(mktemp -d)
  local origin="$town/origin.git"
  git init -q --bare -b main "$origin"
  local seed; seed=$(mktemp -d)
  git -C "$seed" init -q -b main
  git -C "$seed" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
  git -C "$seed" remote add origin "$origin"
  git -C "$seed" push -q origin main
  mkdir -p "$town/gastown/mayor"
  git clone -q "$origin" "$town/gastown/mayor/rig"
  # The plugin delegates the build and install to the rig's own
  # scripts/install-gt.sh (claude-7fc), so the fake rig carries the real one.
  # Committed before the Makefile so the Makefile commit stays the tip and the
  # "commits brought into force" range (HEAD~1..HEAD) names it.
  mkdir -p "$town/gastown/mayor/rig/scripts/lib"
  cp "$SCRIPT_DIR/../../scripts/install-gt.sh" "$town/gastown/mayor/rig/scripts/"
  cp "$SCRIPT_DIR/../../scripts/lib/install-gt-lib.sh" "$town/gastown/mayor/rig/scripts/lib/"
  git -C "$town/gastown/mayor/rig" add scripts
  git -C "$town/gastown/mayor/rig" -c user.email=t@t -c user.name=t commit -q -m installer
  git -C "$town/gastown/mayor/rig" push -q origin main
  # Fake make targets: build drops a marker so the test can see the branch
  # taken; safe-install no-ops the way the real one's atomic replace would
  # inside a stub world. '@' suppresses Make's echo of the recipe line, so
  # build.marker stays the sole observable of a build.
  printf 'build:\n\t@touch "$(GT_TEST_TOWN)/build.marker"\nsafe-install:\n\t@true\n' > "$town/gastown/mayor/rig/Makefile"
  git -C "$town/gastown/mayor/rig" add Makefile
  git -C "$town/gastown/mayor/rig" -c user.email=t@t -c user.name=t commit -q -m makefile
  git -C "$town/gastown/mayor/rig" push -q origin main
  # Stub gt driven by fixture files.
  mkdir -p "$town/bin"
  cat > "$town/bin/gt" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "town root") echo "$GT_TEST_TOWN" ;;
  # stale.switch: what a 'stale --json' read taken AFTER the build has landed
  # returns — installed_stale.json by default (the binary in force is now the
  # tip the run built; gt-b5mpe verifies against exactly this reading),
  # fresh_stale.json when the fixture names it (the binary was already at the
  # tip, so the next run's due check sees fresh). Reads before the build
  # always return stale.json (the pre-build due/threshold check).
  "stale --json")
    if [ -e "$GT_TEST_TOWN/build.marker" ]; then
      for n in $(cat "$GT_TEST_TOWN/stale.switch" 2>/dev/null || echo installed_stale); do
        [ -e "$GT_TEST_TOWN/$n.json" ] && { cat "$GT_TEST_TOWN/$n.json"; break; }
      done
    else
      cat "$GT_TEST_TOWN/stale.json"
    fi ;;
  # Each read is logged to its own file, not gt.log: the reserve path polls
  # this command, and the cases that assert on gt.log must not see slot reads.
  # slot.flip makes the first read (the plugin's own pre-build gate check) see
  # the held fixture and every later read see slot.free.json, so a gate that
  # releases while the plugin waits is observable without racing the test.
  "slot status")
    echo "slot-status $*" >> "$GT_TEST_TOWN/slot.log"
    if [ -e "$GT_TEST_TOWN/slot.flip" ]; then
      if [ -e "$GT_TEST_TOWN/slot.flip.seen" ]; then cat "$GT_TEST_TOWN/slot.free.json"
      else touch "$GT_TEST_TOWN/slot.flip.seen"; cat "$GT_TEST_TOWN/slot.json"
      fi
    else
      cat "$GT_TEST_TOWN/slot.json"
    fi ;;
  # mq.flip: the first read is quiet and every read after it is busy, so a merge
  # that goes in flight between the pre-build check and the pre-install one is
  # observable without racing the test against the plugin.
  "mq list")
    if [ -e "$GT_TEST_TOWN/mq.flip" ]; then
      if [ -e "$GT_TEST_TOWN/mq.flip.seen" ]; then cat "$GT_TEST_TOWN/mq.busy.json"
      else touch "$GT_TEST_TOWN/mq.flip.seen"; cat "$GT_TEST_TOWN/mq.json"
      fi
    else
      cat "$GT_TEST_TOWN/mq.json"
    fi ;;
  # The real 'gt slot run' waits for the container-gate slot and then runs the
  # command under it; slot_refuse makes the acquire fail the way contention or a
  # timed-out wait does.
  "slot run")
    echo "slot run $*" >> "$GT_TEST_TOWN/gt.log"
    shift 2
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
    [ "${1:-}" = "--" ] && shift
    if [ -e "$GT_TEST_TOWN/slot_refuse" ]; then
      echo "acquiring container-gate slot: timed out waiting for slot 0 (contention)" >&2
      exit 1
    fi
    echo "Waiting for container-gate slot (role=gastown/rebuild-gt)..."
    # The literal mirrors slotAcquiredFormat in internal/cmd/slot.go; the build
    # greps for it to tell "never got the slot" from "the build failed", and
    # TestSlotAcquiredFormat pins the Go side of that pair (gt-kox0).
    echo "Container-gate slot acquired (role=gastown/rebuild-gt, waited 2s, slot 0/2)."
    exec "$@" ;;
  # clear_alarms calls 'gt escalate clear' with one --fingerprint per alarm
  # key; a key that is not open writes nothing (the real semantics).
  # clear_refuse makes the clear fail the way a Dolt outage does.
  "escalate clear")
    echo "escalate $*" >> "$GT_TEST_TOWN/gt.log"
    [ -e "$GT_TEST_TOWN/clear_refuse" ] && exit 1
    exit 0 ;;
  "daemon restart") echo "daemon restart $*" >> "$GT_TEST_TOWN/gt.log"; echo "Daemon restarted" ;;
  "plugin record-run") echo "record-run $*" >> "$GT_TEST_TOWN/gt.log" ;;
  "plugin sync"|"formula sync") echo "synced" ;;
  # Every attempt lands in gt.log; which ones the town actually received is in
  # escalate.log, so a test can tell a retried escalation from one marked sent
  # before the call was known to have succeeded (gt-kox0). escalate_refuse is
  # the failing call: Dolt down, a transient error.
  "escalate "*)
    echo "escalate $*" >> "$GT_TEST_TOWN/gt.log"
    if [ -e "$GT_TEST_TOWN/escalate_refuse" ]; then
      echo "escalate-refused $*" >> "$GT_TEST_TOWN/escalate.log"
      echo "gt escalate: dolt unreachable" >&2
      exit 1
    fi
    echo "escalate-sent $*" >> "$GT_TEST_TOWN/escalate.log" ;;
  "version "*|"version ")
    # What the town executes. A build replaces it with what the rig's tip
    # produces, so the two readings around an install differ the way they do
    # in the town — that is the difference the in-force check reads.
    if [ -e "$GT_TEST_TOWN/build.marker" ]; then cat "$GT_TEST_TOWN/built_version.txt"; else cat "$GT_TEST_TOWN/installed_version.txt"; fi
    ;;
  *) echo "stub gt: unhandled: $*" >&2; exit 1 ;;
esac
STUB
  chmod +x "$town/bin/gt"
  # Quiet by default: no slot held, no MR in flight.
  echo '{"held": false, "busy": false, "slots": [{"index": 0, "held": false}, {"index": 1, "held": false}]}' > "$town/slot.json"
  # What slot.flip's later reads see: the gate released the slot.
  echo '{"held": false, "busy": false, "slots": [{"index": 0, "held": false}, {"index": 1, "held": false}]}' > "$town/slot.free.json"
  echo '[]' > "$town/mq.json"
  write_stale "$town" 5
  echo "$town"
}

# write_stale TOWN BEHIND [SAFE] — fixture whose binary_commit is a real
# ancestor of the rig's main (so the "commits in force" range resolves) and
# whose repo_commit is its tip. The binary in force reports the ancestor; a
# build of the rig produces the tip.
#
# repo_commit is the FULL 40-character hash, matching 'gt stale --json' in
# production (internal/version reports StaleOutput.RepoCommit from a plain
# 'git rev-parse', never --short). binary_commit stays short, also matching
# production (the Makefile embeds the binary's own commit via 'git rev-parse
# --short HEAD'). A fixture that used --short for both, as this one used to,
# hid the CRITICAL bug in gt-oqbw's first rework: 'gt version' only ever
# prints the abbreviated form, so comparing it against a full repo_commit
# never matches in the real town even on a successful install — the shell
# test passed anyway because both sides here were short.
write_stale() {
  local town="$1" behind="$2" safe="${3:-True}"
  local rig="$town/gastown/mayor/rig"
  local tip tip_short anc safe_json=true
  [ "$safe" = "True" ] || safe_json=false
  tip=$(git -C "$rig" rev-parse HEAD)
  tip_short=$(git -C "$rig" rev-parse --short HEAD)
  anc=$(git -C "$rig" rev-parse --short HEAD~1)
  printf '{"stale": true, "forward": true, "on_main_branch": true, "safe_to_rebuild": %s, "binary_commit": "%s", "repo_commit": "%s", "compare_ref": "origin/main", "commits_behind": %s}\n' \
    "$safe_json" "$anc" "$tip" "$behind" > "$town/stale.json"
  echo "gt version $anc (dev: main@$anc)" > "$town/installed_version.txt"
  # 'gt version' always prints the abbreviated commit (ShortCommit, <=12
  # chars) even though it was built from $tip: this is what the stub must
  # mimic to reproduce the CRITICAL bug's shape (short from 'gt version' vs
  # full in stale.json's repo_commit above).
  echo "gt version $tip_short (dev: main@$tip_short)" > "$town/built_version.txt"
  # Post-install staleness: the build replaces the in-force binary, so the
  # binary's own commit — the one 'gt stale --json' reads back as
  # binary_commit — becomes the tip the run built. binary_commit stays SHORT
  # here too: it is the same Makefile-embedded 'git rev-parse --short HEAD'
  # value whether read before or after the install, never resolved to full by
  # 'gt stale --json' itself (that resolution is the plugin's job, gt-b5mpe).
  # The stub flips to this fixture on the first 'stale --json' read after
  # build.marker exists (stale.switch, mirroring the built/installed
  # version-file switch); every read before the build still gets stale.json,
  # which is what the pre-build due/threshold check reads (gt-b5mpe).
  printf '{"stale": true, "forward": true, "on_main_branch": true, "safe_to_rebuild": %s, "binary_commit": "%s", "repo_commit": "%s", "compare_ref": "origin/main", "commits_behind": %s}\n' \
    "$safe_json" "$tip_short" "$tip" "$behind" > "$town/installed_stale.json"
  # The not-due variant for the flip: a binary at the tip reads back as
  # fresh, so the pre-build due check of a second run sees that (used only by
  # the no-'@' case below; every other case's flip lands on installed).
  printf '{"stale": false, "forward": true, "on_main_branch": true, "safe_to_rebuild": true, "binary_commit": "%s", "repo_commit": "%s", "compare_ref": "origin/main", "commits_behind": 0}\n' \
    "$tip_short" "$tip" > "$town/fresh_stale.json"
}

run_plugin() {
  # Capture the exit code without set -e aborting the test script on non-zero
  # (om review of gt-htx3: with set -e the rc assertions could never fire).
  # Extra args are VAR=value assignments for the plugin: the reserve budget and
  # the poll interval have to come down to seconds for the timeout paths to be
  # testable at all.
  local town="$1" rc=0; shift
  # PATH is rebuilt from the stub dir plus system dirs only: the real gt lives
  # in ~/.local/bin and must be unreachable, so a stub miss fails loudly
  # ("gt: command not found") instead of writing a real plugin-run receipt
  # into the town's beads (one such stray receipt was seen on 2026-09-19).
  #
  # The INSTALL_GT_* overrides point install-gt.sh (which the plugin delegates
  # to) at the stub world: its default BIN_DIR is the REAL ~/.local/bin, and
  # its default lock wait is 5 minutes. HOME is the temp town as a second
  # guard, so a missed override still cannot reach the real ~/.local/bin.
  ( export GT_TEST_TOWN="$town" GT_TOWN_ROOT="$town" HOME="$town" PATH="$town/bin:/opt/homebrew/bin:/usr/bin:/bin" \
      INSTALL_GT_BIN_DIR="$town/bin" INSTALL_GT_DAEMON_DIR="$town/daemon" INSTALL_GT_LOCK_WAIT=5 "$@"; bash "$RUN_SH" ) > "$town/run.out" 2>&1 || rc=$?
  echo "$rc"
}

# holder_json ROLE -> slot.json with that role holding slot 0
holder_json() {
  printf '{"held": true, "busy": false, "slots": [{"index": 0, "held": true, "owner": {"role": "%s", "pid": 1, "slot": 0}}, {"index": 1, "held": false}]}' "$1"
}

# age_state TOWN MINUTES -> a block that opened MINUTES ago, written where the
# plugin reads it. Aging the clock is how a test reaches the starvation
# threshold without waiting 30 minutes.
age_state() {
  mkdir -p "$1/daemon"
  python3 -c '
import json, sys, time
path, minutes = sys.argv[1], int(sys.argv[2])
d = {"blocked_since_epoch": int(time.time()) - minutes * 60,
     "blocked_since": "aged by the test",
     "consecutive_defers": 3,
     "last_reason": "aged by the test"}
json.dump(d, open(path, "w"))
' "$1/daemon/rebuild-gt-state.json" "$2"
}

# --- Case 1: a gate-class role holds a slot -> DEFERRED (exit 3), no build,
# no record: the record would spend the cooldown that the retry needs. ---
T=$(make_town)
holder_json "gastown/refinery" > "$T/slot.json"
rc=$(run_plugin "$T")
if [ "$rc" != "3" ]; then fail "busy gate: exit $rc, want 3 (deferred): $(cat "$T/run.out")"; else pass "busy gate: exit 3 (deferred)"; fi
if [ -e "$T/build.marker" ]; then fail "busy gate: make build ran while gastown/refinery held a slot"; else pass "busy gate: no build"; fi
if grep -q "record-run" "$T/gt.log" 2>/dev/null; then
  fail "busy gate: recorded a run, which would spend the cooldown: $(cat "$T/gt.log")"
else
  pass "busy gate: no run record, so the next heartbeat retries"
fi
if grep -q "gate suite holds a slot (gastown/refinery)" "$T/run.out"; then pass "busy gate: names the holder"; else fail "busy gate: no holder named: $(cat "$T/run.out")"; fi

# --- Case 2: every gate-class role suffix defers the rebuild ---
for role in gastown/refinery-batch gastown/main-branch-test om/om-review; do
  T=$(make_town)
  holder_json "$role" > "$T/slot.json"
  rc=$(run_plugin "$T")
  if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ] && grep -q "gate suite holds a slot ($role)" "$T/run.out"; then
    pass "$role holder: deferred"
  else
    fail "$role holder: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no) out=$(cat "$T/run.out")"
  fi
done

# --- Case 3: a polecat holding a shared slot is not a gate -> build runs ---
T=$(make_town)
echo '{"held": true, "busy": false, "slots": [{"index": 0, "held": false}, {"index": 1, "held": true, "owner": {"role": "gastown/polecats/opal", "pid": 2, "slot": 1}}]}' > "$T/slot.json"
rc=$(run_plugin "$T")
if [ -e "$T/build.marker" ]; then pass "polecat holder: build ran"; else fail "polecat holder: build did not run: $(cat "$T/run.out")"; fi

# --- Case 4: a suite running outside the gate is not quiet: defer the
# rebuild, but an unreadable Docker is NOT a deferral — a Docker VM that is
# down runs no suite, and treating "could not tell" as busy would park the
# install forever. ---
T=$(make_town)
echo '{"held": false, "busy": true, "docker_unknown": false, "unwrapped_containers": ["gt-gate-abc"], "slots": [{"index": 0, "held": false}]}' > "$T/slot.json"
rc=$(run_plugin "$T")
if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ]; then pass "unwrapped container: deferred"; else fail "unwrapped container: rc=$rc: $(cat "$T/run.out")"; fi

T=$(make_town)
echo '{"held": false, "busy": true, "docker_unknown": true, "unwrapped_containers": [], "slots": [{"index": 0, "held": false}]}' > "$T/slot.json"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "docker unknown: fail-open, build ran"; else fail "docker unknown: rc=$rc: $(cat "$T/run.out")"; fi

# --- Case 4b: every slot held is a busy machine ---
T=$(make_town)
echo '{"held": true, "busy": true, "saturated": true, "slots": [{"index": 0, "held": true, "owner": {"role": "gastown/polecats/opal", "pid": 3, "slot": 0}}, {"index": 1, "held": true, "owner": {"role": "gastown/polecats/jade", "pid": 4, "slot": 1}}]}' > "$T/slot.json"
rc=$(run_plugin "$T")
if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ]; then pass "saturated pool: deferred"; else fail "saturated pool: rc=$rc: $(cat "$T/run.out")"; fi

# --- Case 5: a broken slot status (non-JSON) fails OPEN: the build still runs ---
T=$(make_town)
echo "gt slot status: dolt unreachable" > "$T/slot.json"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "broken slot status: fail-open, build ran"; else fail "broken slot status: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no): $(cat "$T/run.out")"; fi

# --- Case 6: an MR in flight defers: the town is not quiet ---
T=$(make_town)
echo '[{"id": "gt-wisp-x", "status": "in_progress", "title": "Merge: gt-x"}]' > "$T/mq.json"
rc=$(run_plugin "$T")
if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ]; then pass "MR in flight: deferred"; else fail "MR in flight: rc=$rc: $(cat "$T/run.out")"; fi

# --- Case 6b: a broken 'gt mq list' (non-JSON) fails OPEN like the gate
# check, but must say so in the log instead of silently reading it as "0 in
# flight" (gt-oqbw minor: the in-flight check failed open without saying so)
# ---
T=$(make_town)
echo "gt mq list: dolt unreachable" > "$T/mq.json"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "broken mq list: fail-open, build ran"; else fail "broken mq list: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no): $(cat "$T/run.out")"; fi
if grep -q "WARNING: could not read in-flight MR count" "$T/run.out"; then
  pass "broken mq list: fail-open is logged"
else
  fail "broken mq list: fail-open was silent: $(cat "$T/run.out")"
fi

# --- Case 7: quiet and past the threshold -> build, verify, record the
# commits that came into force, write the restart marker (no restart) ---
T=$(make_town)
rc=$(run_plugin "$T")
if [ "$rc" != "0" ]; then fail "quiet + behind: exit $rc: $(cat "$T/run.out")"; else pass "quiet + behind: exit 0"; fi
if [ -e "$T/build.marker" ]; then pass "quiet + behind: build ran"; else fail "quiet + behind: build did not run"; fi
if grep -q -- "--result success" "$T/gt.log" && grep -q "in force .* -> .* (5 commits)" "$T/gt.log"; then
  pass "quiet + behind: success recorded naming what came into force"
else
  fail "quiet + behind: no in-force record: $(cat "$T/gt.log")"
fi
if grep -q "makefile" "$T/gt.log"; then pass "quiet + behind: record lists the commits brought into force"; else fail "quiet + behind: record omits the commit list"; fi
# '^daemon restart' is the stub's own log line for the command; the run's
# success text ("the daemon restarts at its idle point") must not match it.
if grep -q "^daemon restart" "$T/gt.log" 2>/dev/null; then fail "quiet + behind: restarted the daemon (install-gt leaves that to the daemon)"; else pass "quiet + behind: no daemon restart"; fi
TIP=$(git -C "$T/gastown/mayor/rig" rev-parse HEAD)
if python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); assert m["commit"]==sys.argv[2] and m["source"]=="rebuild-gt"' "$T/daemon/restart-pending.json" "$TIP" 2>/dev/null; then
  pass "quiet + behind: restart marker names the installed tip"
else
  fail "quiet + behind: marker missing or wrong: $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"
fi
# gt-b5mpe: a verified install closes this plugin's alarms — including the
# false-positive 'rebuild-gt:unverified' that pre-dates the fix (without the
# key the last false positive stays open forever), and the formula sync that
# the receipt depends on has run by the time the success is recorded.
if grep -q "escalate clear .*--fingerprint rebuild-gt:unverified" "$T/gt.log"; then
  pass "quiet + behind: the unverified alarm key is cleared with the rest"
else
  fail "quiet + behind: rebuild-gt:unverified was left open: $(cat "$T/gt.log")"
fi
if grep -q "escalate clear" "$T/gt.log"; then
  pass "quiet + behind: alarms cleared once the binary is in force"
else
  fail "quiet + behind: no clear of alarms: $(cat "$T/gt.log")"
fi
if grep -q "synced" "$T/run.out"; then
  pass "quiet + behind: formula/plugin sync ran with the verified install"
else
  fail "quiet + behind: no sync ran: $(cat "$T/run.out")"
fi

# --- Case 8: stale but under the install threshold -> deferred, no build ---
T=$(make_town)
write_stale "$T" 2
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=5)
if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ]; then pass "under threshold: deferred"; else fail "under threshold: rc=$rc: $(cat "$T/run.out")"; fi
# ...and the threshold is configurable. Invoked directly rather than through
# run_plugin, so it carries run_plugin's install-gt overrides itself: without
# INSTALL_GT_BIN_DIR the delegated install would write the REAL
# ~/.local/bin/gt.prev and run the real gt.
T=$(make_town)
write_stale "$T" 2
rc=$( ( export GT_TEST_TOWN="$T" GT_TOWN_ROOT="$T" HOME="$T" REBUILD_GT_INSTALL_THRESHOLD=1 PATH="$T/bin:/opt/homebrew/bin:/usr/bin:/bin" \
    INSTALL_GT_BIN_DIR="$T/bin" INSTALL_GT_DAEMON_DIR="$T/daemon" INSTALL_GT_LOCK_WAIT=5; bash "$RUN_SH" ) > "$T/run.out" 2>&1; echo $? )
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "threshold override: build ran at 2 behind with a threshold of 1"; else fail "threshold override: rc=$rc: $(cat "$T/run.out")"; fi

# --- Case 8b: the default threshold is 1 commit — a single merge is due
# (claude-7fc: a lone urgent fix was never due under the old default of 5) ---
T=$(make_town)
write_stale "$T" 1
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "default threshold: 1 behind installs"; else fail "default threshold: rc=$rc $(cat "$T/run.out")"; fi

# --- Case 9: a grubby checkout is refused: skip recorded, no build, no alarm
# on the run that first hits it — a refusal clears itself, and an alarm here
# would fire on states that fix themselves. The block's age is 0 in this case,
# which is why nothing escalates; Case 26 ages it past the threshold. ---
T=$(make_town)
echo "junk" >> "$T/gastown/mayor/rig/Makefile"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ ! -e "$T/build.marker" ] && grep -q -- "--result skipped" "$T/gt.log"; then
  pass "dirty checkout: refused, skip recorded, no build"
else
  fail "dirty checkout: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q "escalate" "$T/gt.log" 2>/dev/null; then
  fail "dirty checkout: escalated, which the drift alarm already covers"
else
  pass "dirty checkout: no escalation of its own"
fi

# --- Case 10: diverged local main is REFUSED, not reset ---
T=$(make_town)
RIG="$T/gastown/mayor/rig"
# Local main gains a commit origin does not have...
git -C "$RIG" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "local only"
# ...and origin gains a different one, so the ff-only merge cannot land and a
# reset --hard would be the only way through — which is never the answer.
OTHER=$(mktemp -d)
git clone -q "$T/origin.git" "$OTHER"
git -C "$OTHER" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "origin only"
git -C "$OTHER" push -q origin main
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && ! grep -q "daemon restart" "$T/gt.log" && grep -q "diverged from origin/main" "$T/gt.log"; then
  pass "diverged main: refused, skip recorded, no install"
else
  fail "diverged main: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null) out=$(cat "$T/run.out")"
fi

# --- Case 10b: gt-9jax — the divergence count check must be able to read as
# "not diverged" (0), not just "diverged" (>0, Case 10 above): the buggy form
# ('[ -n "$(... --count)" ]', always true since --count prints "0") could
# never take the retry branch below it. An untracked file the fast-forward
# would overwrite is the one way 'merge --ff-only' fails with zero local-only
# commits — a real divergence always has at least one (Case 10); the
# dirty-checkout guard above excludes untracked files on purpose (gt-50k), so
# this state reaches the sync step unblocked. ---
T=$(make_town)
RIG="$T/gastown/mayor/rig"
OTHER=$(mktemp -d)
git clone -q "$T/origin.git" "$OTHER"
echo "origin content" > "$OTHER/newfile.txt"
git -C "$OTHER" add newfile.txt
git -C "$OTHER" -c user.email=t@t -c user.name=t commit -q -m "adds newfile.txt"
git -C "$OTHER" push -q origin main
echo "untracked, collides with the incoming commit" > "$RIG/newfile.txt"
rc=$(run_plugin "$T")
if grep -q "origin/main moved during sync; re-fetching and re-merging once" "$T/run.out"; then
  pass "divergence retry: an untracked-file conflict (0 local-only commits) takes the retry path, not an immediate bail"
else
  fail "divergence retry: no retry attempted (count check regressed to always-true): $(cat "$T/run.out")"
fi
if [ "$rc" = "0" ] && ! grep -q "daemon restart" "$T/gt.log" 2>/dev/null && grep -q "diverged from origin/main" "$T/gt.log" 2>/dev/null; then
  pass "divergence retry: still refused after the retry (the untracked file persists), not reset"
else
  fail "divergence retry: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi

# --- Case 11: not safe to rebuild (binary not an ancestor of main) is
# refused rather than deferred: waiting cannot clear it ---
T=$(make_town)
write_stale "$T" 5 False
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ ! -e "$T/build.marker" ] && grep -q -- "--result skipped" "$T/gt.log" && grep -q "not safe to rebuild" "$T/gt.log"; then
  pass "unsafe to rebuild: refused, skip recorded, no build"
else
  fail "unsafe to rebuild: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi

# --- Case 12: a failed build exits non-zero and records a failure (proves the
# rc capture: with set -e swallowing it this assertion could never run) ---
T=$(make_town)
printf 'build:\n\tfalse\nsafe-install:\n\t@true\n' > "$T/gastown/mayor/rig/Makefile"
git -C "$T/gastown/mayor/rig" -c user.email=t@t -c user.name=t commit -q -am "break build"
git -C "$T/gastown/mayor/rig" push -q origin main
rc=$(run_plugin "$T")
if [ "$rc" != "0" ] && grep -q -- "--result failure" "$T/gt.log" 2>/dev/null; then
  pass "failed build: non-zero exit ($rc) and failure recorded"
else
  fail "failed build: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi

# --- Case 13: an install that does not take is caught, not reported as
# success: gt on PATH still reports the old (but otherwise valid,
# resolvable) commit after the build. A garbage/unresolvable string here
# would exercise the [unverified] "cannot resolve" path instead of this one
# — this is the realistic production shape: a shadowed gt or a wrong
# INSTALL_DIR still resolves to SOME real commit, just not the one built. ---
T=$(make_town)
write_stale "$T" 5
cp "$T/installed_version.txt" "$T/built_version.txt"
# Mirror the version-file switch into the stale fixture: the install left the
# old binary in force, so binary_commit read back after the build is the
# ancestor the pre-build fixture already names — write_stale's flip would
# claim the tip and mask the not-take (gt-b5mpe).
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
json.dump(d, open(sys.argv[2], "w"))
' "$T/stale.json" "$T/installed_stale.json"
rc=$(run_plugin "$T")
if [ "$rc" != "0" ] && grep -q "escalate .*install did not take" "$T/gt.log"; then
  pass "install that did not take: failed and escalated"
else
  fail "install that did not take: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q "record-run .*--result success" "$T/gt.log" 2>/dev/null; then
  fail "install that did not take: recorded success anyway"
else
  pass "install that did not take: no success recorded"
fi
if grep -q "daemon restart" "$T/gt.log" 2>/dev/null; then
  fail "install that did not take: restarted the daemon onto a binary that is not in force"
else
  pass "install that did not take: daemon left alone"
fi
if [ -e "$T/daemon/restart-pending.json" ]; then fail "install that did not take: wrote a restart marker"; else pass "install that did not take: no marker"; fi
if grep -q -- "--fingerprint install-gt:smoke-failed" "$T/gt.log"; then pass "install that did not take: install-gt fingerprint"; else fail "install that did not take: fingerprint $(cat "$T/gt.log")"; fi

# --- Case 14: binary already fresh -> recorded, no build, no restart ---
T=$(make_town)
echo '{"stale": false, "forward": true, "on_main_branch": true, "safe_to_rebuild": true, "binary_commit": "abc1234", "repo_commit": "abc1234", "commits_behind": 0}' > "$T/stale.json"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ ! -e "$T/build.marker" ] && grep -q "binary is fresh" "$T/gt.log" && ! grep -q "daemon restart" "$T/gt.log"; then
  pass "already fresh: recorded, no build, no restart"
else
  fail "already fresh: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no) log=$(cat "$T/gt.log" 2>/dev/null)"
fi

# --- Case 15: no rig root is skipped without a build ---
T=$(make_town)
rm -rf "$T/gastown/mayor/rig"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ ! -e "$T/build.marker" ] && grep -q "does not exist" "$T/run.out"; then
  pass "missing rig root: skipped"
else
  fail "missing rig root: rc=$rc out=$(cat "$T/run.out")"
fi

# --- Case 16: an unreadable staleness check defers rather than recording a
# run that accomplished nothing ---
T=$(make_town)
rm -f "$T/stale.json"
rc=$(run_plugin "$T")
if [ "$rc" = "3" ] && ! grep -q "record-run" "$T/gt.log" 2>/dev/null; then
  pass "unreadable staleness: deferred, nothing recorded"
else
  fail "unreadable staleness: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null) out=$(cat "$T/run.out")"
fi

# --- Case 17: an installed commit that does not resolve inside RIG_ROOT at
# all is caught as unverified -- distinct from Case 13's resolvable-but-wrong
# commit ("not-in-force"), this is 'gt version' reporting something that
# isn't a commit in this repo (a shadowing gt, a build against a different
# checkout). Neither is reported as success. ---
T=$(make_town)
write_stale "$T" 5
echo "gt version deadbee (dev: main@deadbee)" > "$T/built_version.txt"
# The unverified case is the one where 'gt stale --json' also has no commit to
# read (a dev build installed by some other path) — the only way the '@'
# fallback ever runs. So clear the post-install flip's commit: the stub's
# stale reading after the build has none either, and the plugin falls through
# to 'gt version', which reports the unresolvable deadbee (gt-b5mpe).
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
d["binary_commit"] = ""
json.dump(d, open(sys.argv[2], "w"))
' "$T/stale.json" "$T/installed_stale.json"
rc=$(run_plugin "$T")
if [ "$rc" != "0" ] && grep -q "escalate .*cannot verify what came into force" "$T/gt.log"; then
  pass "unresolvable installed commit: failed and escalated as unverified"
else
  fail "unresolvable installed commit: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q "daemon restart" "$T/gt.log" 2>/dev/null; then
  fail "unresolvable installed commit: restarted the daemon anyway"
else
  pass "unresolvable installed commit: daemon left alone"
fi

# --- Case 18: commits_behind missing from 'gt stale --json' must not read as
# "0 behind, under threshold" -- that reading is what let a stale, safe,
# quiet binary sit deferred every heartbeat forever, since the threshold gate
# could never be satisfied by an unmeasurable count (gt-oqbw). The
# install must proceed, and the drift check (unconditional, at the top of the
# script) must also escalate rather than silently reading the same unknown
# count as "0 behind, nothing to worry about". ---
T=$(make_town)
RIG="$T/gastown/mayor/rig"
TIP=$(git -C "$RIG" rev-parse HEAD)
TIP_SHORT=$(git -C "$RIG" rev-parse --short HEAD)
ANC=$(git -C "$RIG" rev-parse --short HEAD~1)
printf '{"stale": true, "forward": true, "on_main_branch": true, "safe_to_rebuild": true, "binary_commit": "%s", "repo_commit": "%s", "compare_ref": "origin/main"}\n' \
  "$ANC" "$TIP" > "$T/stale.json"
echo "gt version $ANC (dev: main@$ANC)" > "$T/installed_version.txt"
echo "gt version $TIP_SHORT (dev: main@$TIP_SHORT)" > "$T/built_version.txt"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then
  pass "commits_behind unknown: proceeded with install instead of parking"
else
  fail "commits_behind unknown: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no) out=$(cat "$T/run.out")"
fi
if grep -q -- "--result success" "$T/gt.log" 2>/dev/null; then
  pass "commits_behind unknown: success recorded"
else
  fail "commits_behind unknown: no success recorded: $(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q "escalate .*commits_behind could not be determined" "$T/gt.log" 2>/dev/null; then
  pass "commits_behind unknown: drift-unknown escalation fired (cannot rule out being over the drift threshold)"
else
  fail "commits_behind unknown: no drift-unknown escalation: $(cat "$T/gt.log" 2>/dev/null)"
fi

# --- Case 19: the exit-code contract itself (plugin.md's table), one
# assertion per code, so a future change to any of the three has to touch
# this test as well as the doc — the thing gt-oqbw's rework was rejected for
# leaving out of sync. Reuses the scenarios above rather than re-deriving
# them, so this is a contract check, not new coverage. ---
# exit 0 (worked): Case 7's success run.
T=$(make_town)
write_stale "$T" 5
rc=$(run_plugin "$T")
[ "$rc" = "0" ] || fail "contract exit 0 (worked): got rc=$rc"
# exit 0 (refused safely, not escalated): Case 9's dirty checkout, with a
# block age of 0 — a refusal escalates only past the starvation threshold.
T=$(make_town)
echo "junk" >> "$T/gastown/mayor/rig/Makefile"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && ! grep -q "escalate" "$T/gt.log" 2>/dev/null; then
  pass "contract exit 0 (refused): rc=0, no escalation"
else
  fail "contract exit 0 (refused): rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
# exit 3 (deferred, no record): Case 8's under-threshold defer.
T=$(make_town)
write_stale "$T" 2
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=5)
if [ "$rc" = "3" ] && [ ! -s "$T/gt.log" ]; then
  pass "contract exit 3 (deferred): rc=3, nothing recorded"
else
  fail "contract exit 3 (deferred): rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
# exit 1 (failed, recorded and escalated): Case 12's broken build.
T=$(make_town)
printf 'build:\n\tfalse\nsafe-install:\n\t@true\n' > "$T/gastown/mayor/rig/Makefile"
git -C "$T/gastown/mayor/rig" -c user.email=t@t -c user.name=t commit -q -am "break build"
git -C "$T/gastown/mayor/rig" push -q origin main
rc=$(run_plugin "$T")
if [ "$rc" = "1" ] && grep -q -- "--result failure" "$T/gt.log" 2>/dev/null && grep -q "escalate" "$T/gt.log" 2>/dev/null; then
  pass "contract exit 1 (failed): rc=1, recorded as failure, escalated"
else
  fail "contract exit 1 (failed): rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi

# --- gt-kox0: a rebuild that cannot win must not starve silently ---

# Case 20: a block that repeats is counted and named in the log, so the town can
# see starvation without reading the state file.
T=$(make_town)
holder_json "gastown/refinery-batch" > "$T/slot.json"
run_plugin "$T" >/dev/null
rc=$(run_plugin "$T")
if grep -q "blocked 0m over 2 run(s)" "$T/run.out"; then
  pass "starvation: the second blocked run is counted in the log"
else
  fail "starvation: the block was not counted: $(cat "$T/run.out")"
fi
if python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); sys.exit(0 if d.get("consecutive_defers")==2 and d.get("blocked_since_epoch") else 1)' "$T/daemon/rebuild-gt-state.json" 2>/dev/null; then
  pass "starvation: the state file carries the count and the block's start"
else
  fail "starvation: state file wrong or missing: $(cat "$T/daemon/rebuild-gt-state.json" 2>/dev/null)"
fi
if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ]; then
  pass "starvation: still a deferral under the threshold, no build"
else
  fail "starvation: rc=$rc under the threshold"
fi

# Case 21: past the threshold the run escalates loudly AND waits for the gate
# to release the slot before building inside it — the two things that make an
# inert fix loud and let it win (gt-kox0).
#
# The gate releases while the plugin waits (slot.flip), which is also the
# assertion that separates a real wait from the racing this replaced: against
# the old code, which read the slot once and went straight to 'gt slot run',
# slot.log holds a single read (gt-kox0 major).
T=$(make_town)
holder_json "gastown/refinery-batch" > "$T/slot.json"
touch "$T/slot.flip"
age_state "$T" 40
rc=$(run_plugin "$T" REBUILD_GT_POLL_SECONDS=0)
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then
  pass "starvation: past the threshold the rebuild reaches the build"
else
  fail "starvation: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no): $(cat "$T/run.out")"
fi
READS=$(wc -l < "$T/slot.log" 2>/dev/null | tr -d ' ') || READS=0
if [ "${READS:-0}" -ge 2 ]; then
  pass "starvation: the build waited for the gate to release the slot (${READS} slot reads)"
else
  fail "starvation: the build did not wait for the gate ($READS slot read(s)): $(cat "$T/slot.log" 2>/dev/null)"
fi
if grep -q "slot run --role gastown/rebuild-gt" "$T/gt.log" 2>/dev/null; then
  pass "starvation: the build ran inside the container-gate slot"
else
  fail "starvation: the reservation path did not run: $(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q "escalate .* --fingerprint rebuild-gt:starved" "$T/gt.log" 2>/dev/null && grep -q -- "-s high" "$T/gt.log"; then
  pass "starvation: escalated loudly (high) under rebuild-gt:starved"
else
  fail "starvation: no loud escalation: $(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q -- "--result success" "$T/gt.log" 2>/dev/null && [ -e "$T/daemon/restart-pending.json" ]; then
  pass "starvation: the reserved rebuild installed and marked for restart"
else
  fail "starvation: nothing installed: $(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q "escalate clear .*--fingerprint rebuild-gt:starved" "$T/gt.log" 2>/dev/null; then
  pass "starvation: the alarm closes once the binary is in force"
else
  fail "starvation: the alarm was left open after a successful install: $(cat "$T/gt.log" 2>/dev/null)"
fi

# Case 22: the gate releases, but the acquire still fails — a deferral, not a
# failure, because the build never started.
T=$(make_town)
holder_json "gastown/refinery-batch" > "$T/slot.json"
touch "$T/slot.flip"
touch "$T/slot_refuse"
age_state "$T" 40
rc=$(run_plugin "$T" REBUILD_GT_POLL_SECONDS=0)
if [ "$rc" = "3" ] && grep -q "did not get it" "$T/run.out"; then
  pass "starvation: a slot it never got defers on the heartbeat"
else
  fail "starvation: rc=$rc out=$(cat "$T/run.out")"
fi
if grep -q -- "--result failure" "$T/gt.log" 2>/dev/null; then
  fail "starvation: reported a build failure for a build that never started"
else
  pass "starvation: no failure recorded for a wait"
fi

# Case 22b: a gate that never releases defers on the reserve budget, rather
# than waiting forever or failing. The budget is what keeps the plugin inside
# its own [execution] timeout (gt-kox0).
T=$(make_town)
holder_json "gastown/refinery-batch" > "$T/slot.json"
age_state "$T" 40
rc=$(run_plugin "$T" REBUILD_GT_RESERVE_WAIT=1s REBUILD_GT_POLL_SECONDS=0)
if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ] && grep -q "did not get it" "$T/run.out"; then
  pass "starvation: a gate that never releases defers on the budget"
else
  fail "starvation: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no): $(cat "$T/run.out")"
fi
if grep -q "slot run" "$T/gt.log" 2>/dev/null; then
  fail "starvation: took the slot from a gate that was still holding it"
else
  pass "starvation: no build started while the gate held the slot"
fi
# The wait and the deferral that ends the run are one blocked run, not two: the
# count is what the log reads out, and a run that noted itself twice would
# double-count every reserve attempt. age_state seeds 3, so one run reads 4.
DEFERS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("consecutive_defers"))' "$T/daemon/rebuild-gt-state.json" 2>/dev/null || true)
if [ "$DEFERS" = "4" ]; then
  pass "starvation: one blocked run is counted once"
else
  fail "starvation: one run counted $DEFERS defers, want 4: $(cat "$T/daemon/rebuild-gt-state.json" 2>/dev/null)"
fi

# Case 23: a container running outside the gate is not something the gate slot
# serializes, so past the threshold it escalates but does NOT queue for the slot
# (gt-kox0).
T=$(make_town)
echo '{"held": false, "busy": true, "docker_unknown": false, "unwrapped_containers": ["gt-gate-abc"], "slots": [{"index": 0, "held": false}]}' > "$T/slot.json"
age_state "$T" 40
rc=$(run_plugin "$T")
if [ "$rc" = "3" ] && ! grep -q "slot run" "$T/gt.log" 2>/dev/null; then
  pass "unwrapped container: escalated without taking the slot"
else
  fail "unwrapped container: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q -- "-s high" "$T/gt.log" 2>/dev/null; then
  pass "unwrapped container: still escalates loudly"
else
  fail "unwrapped container: no escalation: $(cat "$T/gt.log" 2>/dev/null)"
fi

# Case 24: a block past the threshold escalates once, not once per heartbeat. A
# firing per retry would be a Dolt commit every few minutes for the same
# condition; the town re-escalates an unacked escalation on its own cadence.
T=$(make_town)
echo '{"held": false, "busy": true, "docker_unknown": false, "unwrapped_containers": ["gt-gate-abc"], "slots": [{"index": 0, "held": false}]}' > "$T/slot.json"
age_state "$T" 40
run_plugin "$T" >/dev/null
run_plugin "$T" >/dev/null
FIRINGS=$(grep -c -- "-s high" "$T/gt.log" 2>/dev/null || true)
if [ "$FIRINGS" = "1" ]; then
  pass "starvation: escalated once for two blocked runs past the threshold"
else
  fail "starvation: $FIRINGS escalations for one block: $(cat "$T/gt.log" 2>/dev/null)"
fi

# Case 25: a merge that goes in flight while the build runs no longer holds
# the install back. The re-check existed because the install ended in a daemon
# restart that killed in-flight work; install-gt only renames the binary and
# leaves the restart to the daemon's idle point (claude-7fc).
T=$(make_town)
echo '[{"id": "gt-wisp-z", "status": "in_progress", "title": "Merge: gt-z"}]' > "$T/mq.busy.json"
touch "$T/mq.flip"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && grep -q -- "--result success" "$T/gt.log" 2>/dev/null; then
  pass "merge goes in flight mid-build: the install still lands"
else
  fail "merge goes in flight mid-build: rc=$rc $(cat "$T/run.out")"
fi

# Case 26: a refusal past the threshold escalates like any other block. A
# dirty checkout is refused without an alarm on the run that first hits it
# (Case 9), but a due binary stuck on one for half an hour is the starvation
# gt-kox0 is about, and it must not stay silent. No slot is taken: a refusal
# means there is nothing to build (gt-kox0 minor).
T=$(make_town)
echo "junk" >> "$T/gastown/mayor/rig/Makefile"
age_state "$T" 40
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && grep -q "escalate .* --fingerprint rebuild-gt:starved" "$T/gt.log" && grep -q -- "-s high" "$T/gt.log"; then
  pass "blocked refusal: a refusal past the threshold escalates under rebuild-gt:starved"
else
  fail "blocked refusal: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
if grep -q "slot run" "$T/gt.log" 2>/dev/null; then
  fail "blocked refusal: took a container-gate slot for a refusal"
else
  pass "blocked refusal: no slot taken"
fi

# Case 27: a run that finds the binary NOT due closes a block from an earlier
# one. Without this the block's start time outlives the condition it measured,
# and the next block — after main moves past the threshold again — arrives
# pre-aged and escalates and reserves on its very first run (gt-kox0 minor).
T=$(make_town)
write_stale "$T" 2
age_state "$T" 40
rc=$(run_plugin "$T")
if python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); sys.exit(0 if not d.get("blocked_since_epoch") else 1)' "$T/daemon/rebuild-gt-state.json" 2>/dev/null; then
  pass "not due: a stale block is closed by a not-due run"
else
  fail "not due: the block outlived the condition: $(cat "$T/daemon/rebuild-gt-state.json" 2>/dev/null)"
fi
if grep -q -- "-s high" "$T/gt.log" 2>/dev/null; then
  fail "not due: escalated a block for a binary that needs no installing"
else
  pass "not due: no escalation opened"
fi
if grep -q "escalate clear .*--fingerprint rebuild-gt:starved" "$T/gt.log" 2>/dev/null; then
  pass "not due: the stale block's alarm is closed"
else
  fail "not due: the stale block's alarm was left open: $(cat "$T/gt.log" 2>/dev/null)"
fi

# Case 28: a block whose escalation never reached the town is retried on the
# next run rather than recorded as delivered — otherwise one failed call
# silences the alarm for the rest of the block, which is the silence gt-kox0
# exists to end (gt-kox0 major).
T=$(make_town)
echo '{"held": false, "busy": true, "docker_unknown": false, "unwrapped_containers": ["gt-gate-abc"], "slots": [{"index": 0, "held": false}]}' > "$T/slot.json"
age_state "$T" 40
touch "$T/escalate_refuse"
run_plugin "$T" >/dev/null
if python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); sys.exit(1 if d.get("escalated_epoch") else 0)' "$T/daemon/rebuild-gt-state.json" 2>/dev/null; then
  pass "failed escalation: the failed attempt did not mark the block"
else
  fail "failed escalation: marked sent anyway: $(cat "$T/daemon/rebuild-gt-state.json" 2>/dev/null)"
fi
rm -f "$T/escalate_refuse"
run_plugin "$T" >/dev/null
SENT=$(grep -c "escalate-sent" "$T/escalate.log" 2>/dev/null || true)
if [ "$SENT" = "1" ]; then
  pass "failed escalation: the next run retried, and the escalation landed once"
else
  fail "failed escalation: $SENT successful escalation(s): $(cat "$T/escalate.log" 2>/dev/null)"
fi

# --- gt-b5mpe: the shape of the production failure — a binary whose
# 'gt version' output carries no '@' at all (the dev form this binary prints
# from a cwd that is not a repo, which is where the plugin always runs) and
# whose stale reading therefore has no binary_commit either. The 'gt version'
# '@'-form was the only check this file had, so a correct install was the
# unresolvable case; the commit is now read from 'gt stale --json', whose
# binary_commit the production binary reports even though 'gt version' from
# this cwd cannot. A no-'@' version line must not fail a verified install. ---
T=$(make_town)
write_stale "$T" 5
# The in-force binary reports no commit in either reading: dev build (the
# 'gt version' line has no '@') and an empty binary_commit in stale.json.
echo "gt version dev" > "$T/installed_version.txt"
echo "gt version dev" > "$T/built_version.txt"
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
d["binary_commit"] = ""
json.dump(d, open(sys.argv[1], "w"))
' "$T/stale.json"
# The post-install flip must name the tip in the SHORT form 'gt stale --json'
# actually reports (the Makefile-embedded 'git rev-parse --short HEAD',
# internal/cmd.Commit) — not full. The plugin resolves it to a full hash
# inside RIG_ROOT before comparing to EXPECTED_COMMIT; a fixture that used the
# full hash here would hide a regression back to the un-resolved comparison
# (gt-b5mpe).
TIP_SHORT=$(git -C "$T/gastown/mayor/rig" rev-parse --short HEAD)
python3 -c '
import json, sys
d = json.load(open(sys.argv[2]))
d["binary_commit"] = sys.argv[1]
json.dump(d, open(sys.argv[2], "w"))
' "$TIP_SHORT" "$T/installed_stale.json"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && grep -q -- "--result success" "$T/gt.log" 2>/dev/null; then
  pass "no-@ version + no stale commit: verified install succeeds via binary_commit"
else
  fail "no-@ version + no stale commit: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi
if [ -e "$T/daemon/restart-pending.json" ]; then
  pass "no-@ version + no stale commit: restart marker written for the verified binary"
else
  fail "no-@ version + no stale commit: no marker: $(cat "$T/run.out")"
fi
if grep -q "escalate .*cannot verify" "$T/gt.log" 2>/dev/null; then
  fail "no-@ version + no stale commit: a correct install escalated as unverified — the gt-b5mpe false positive"
else
  pass "no-@ version + no stale commit: no false-positive unverified escalation"
fi

# --- gt-9jax: a merge lands on origin/main while 'make ... build' is
# running. The run already fast-forwarded RIG_ROOT to origin/main before the
# build started, so the build compiles the right tree either way; what fails
# is 'make safe-install', whose real check-up-to-date target does its OWN
# fetch-and-compare against the now-moved origin/main. SKIP_UPDATE_CHECK=1
# (passed to both 'make build' and 'make safe-install') is what makes
# check-up-to-date a no-op, which is the actual fix — not a Makefile change.
#
# The fake Makefile below reproduces check-up-to-date's real shape (an
# ifndef SKIP_UPDATE_CHECK-gated fetch-and-compare on safe-install), and its
# 'build' recipe pushes a second commit to origin from another clone before
# touching build.marker — a deterministic stand-in for a merge landing
# during the (in production, minutes-long) build. RIG_ROOT's own checkout
# never sees that push (only a fetch would advance it), so EXPECTED_COMMIT
# stays pinned to what was actually built; only a live check-up-to-date can
# be fooled by it.
T=$(make_town)
RIG="$T/gastown/mayor/rig"
cat > "$RIG/Makefile" <<'MK'
check-up-to-date:
ifndef SKIP_UPDATE_CHECK
	@git fetch origin main --quiet; \
	LOCAL=$$(git rev-parse HEAD); \
	REMOTE=$$(git rev-parse origin/main); \
	if [ -n "$$REMOTE" ] && [ "$$LOCAL" != "$$REMOTE" ]; then \
	  echo "ERROR: Local branch is not up to date with origin/main"; \
	  exit 1; \
	fi
endif
build:
	@bash "$(GT_TEST_TOWN)/land-merge.sh"
	@touch "$(GT_TEST_TOWN)/build.marker"
safe-install: check-up-to-date
	@true
MK
git -C "$RIG" -c user.email=t@t -c user.name=t add Makefile
git -C "$RIG" -c user.email=t@t -c user.name=t commit -q -m "makefile"
git -C "$RIG" push -q origin main
# Cloned AFTER the Makefile commit lands on origin, so its own push during
# the build (below) is a clean fast-forward rather than a rejected non-ff.
OTHER=$(mktemp -d)
git clone -q "$T/origin.git" "$OTHER"
cat > "$T/land-merge.sh" <<EOF
#!/usr/bin/env bash
set -e
git -C "$OTHER" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "lands mid-build"
git -C "$OTHER" push -q origin main
EOF
write_stale "$T" 5
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && grep -q -- "--result success" "$T/gt.log" 2>/dev/null; then
  pass "merge lands mid-build: SKIP_UPDATE_CHECK skips the live re-check, install succeeds"
else
  fail "merge lands mid-build: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null) out=$(cat "$T/run.out")"
fi
# The same fixture against run.sh as it stood before this fix (no
# SKIP_UPDATE_CHECK passed to safe-install) is the regression this guards:
# check-up-to-date's live fetch sees the moved origin/main and fails a build
# that was otherwise correct. Confirmed by hand against origin/main's run.sh
# (see the commit message for both runs) rather than re-run here, so this
# file tests one version of run.sh, not two.

# --- claude-7fc: backstop — the daemon must come into force after an install ---
# age_marker TOWN MINUTES -> a restart-pending marker requested MINUTES ago
age_marker() {
  mkdir -p "$1/daemon"
  python3 -c '
import datetime, json, sys, time
t = datetime.datetime.fromtimestamp(time.time() - int(sys.argv[2]) * 60, datetime.timezone.utc)
json.dump({"commit": "0" * 40, "requested_at": t.strftime("%Y-%m-%dT%H:%M:%SZ"), "source": "post-merge", "repo": "/x"}, open(sys.argv[1], "w"))
' "$1/daemon/restart-pending.json" "$2"
}

# Case 29: a marker older than 30m escalates HIGH, once per condition.
T=$(make_town)
age_marker "$T" 40
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "escalate .*-s high .*--fingerprint rebuild-gt:daemon-not-in-force" "$T/gt.log" 2>/dev/null; then
  pass "marker 40m old: escalated daemon-not-in-force"
else
  fail "marker 40m old: no escalation: $(cat "$T/gt.log" 2>/dev/null) $(cat "$T/run.out")"
fi
[ -e "$T/daemon/rebuild-gt-daemon-lag" ] && pass "marker 40m old: lag recorded" || fail "marker 40m old: lag not recorded"

# Case 30: a fresh marker is the daemon working as designed: no escalation.
T=$(make_town)
age_marker "$T" 5
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "daemon-not-in-force" "$T/gt.log" 2>/dev/null; then fail "marker 5m old: escalated early"; else pass "marker 5m old: quiet"; fi

# Case 31: no marker, but the daemon's recorded commit is behind the installed
# binary and the binary has been in place for over 30m -> escalate.
T=$(make_town)
RIG="$T/gastown/mayor/rig"
OLD=$(git -C "$RIG" rev-parse --short HEAD~2)
mkdir -p "$T/daemon"
printf '{"running": true, "commit": "%s"}\n' "$OLD" > "$T/daemon/state.json"
python3 -c 'import os,sys,time; t=time.time()-3600; os.utime(sys.argv[1], (t, t))' "$T/bin/gt"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q -- "--fingerprint rebuild-gt:daemon-not-in-force" "$T/gt.log" 2>/dev/null; then
  pass "daemon behind installed binary: escalated"
else
  fail "daemon behind installed binary: $(cat "$T/gt.log" 2>/dev/null) $(cat "$T/run.out")"
fi

# Case 31b: the same daemon lag on a binary installed moments ago is the
# daemon's idle-point restart still pending: no escalation yet.
T=$(make_town)
RIG="$T/gastown/mayor/rig"
OLD=$(git -C "$RIG" rev-parse --short HEAD~2)
mkdir -p "$T/daemon"
printf '{"running": true, "commit": "%s"}\n' "$OLD" > "$T/daemon/state.json"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "daemon-not-in-force" "$T/gt.log" 2>/dev/null; then fail "daemon behind a fresh install: escalated early"; else pass "daemon behind a fresh install: quiet"; fi

# Case 31c: the daemon records the FULL form of the binary's short commit (the
# daemon resolves its own commit; the binary reports --short) -> the same
# commit, no escalation even on a binary installed an hour ago.
T=$(make_town)
RIG="$T/gastown/mayor/rig"
mkdir -p "$T/daemon"
printf '{"running": true, "commit": "%s"}\n' "$(git -C "$RIG" rev-parse HEAD~1)" > "$T/daemon/state.json"
python3 -c 'import os,sys,time; t=time.time()-3600; os.utime(sys.argv[1], (t, t))' "$T/bin/gt"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "daemon-not-in-force" "$T/gt.log" 2>/dev/null; then fail "daemon at the binary's commit (full form): escalated"; else pass "daemon at the binary's commit (full form): quiet"; fi

# Case 31d: the daemon runs a descendant of the installed binary's commit
# (restarted onto a newer build) -> no escalation.
T=$(make_town)
RIG="$T/gastown/mayor/rig"
mkdir -p "$T/daemon"
printf '{"running": true, "commit": "%s"}\n' "$(git -C "$RIG" rev-parse HEAD)" > "$T/daemon/state.json"
python3 -c 'import os,sys,time; t=time.time()-3600; os.utime(sys.argv[1], (t, t))' "$T/bin/gt"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "daemon-not-in-force" "$T/gt.log" 2>/dev/null; then fail "daemon descends from the binary: escalated"; else pass "daemon descends from the binary: quiet"; fi

# in_force_state TOWN -> state.json whose daemon commit descends from the
# binary's, and no marker: a positive healthy reading.
in_force_state() {
  mkdir -p "$1/daemon"
  printf '{"running": true, "commit": "%s"}\n' "$(git -C "$1/gastown/mayor/rig" rev-parse HEAD)" > "$1/daemon/state.json"
}

# Case 32: the condition clears -> the open escalation is cleared, once.
T=$(make_town)
in_force_state "$T"; touch "$T/daemon/rebuild-gt-daemon-lag"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "escalate clear .*--fingerprint rebuild-gt:daemon-not-in-force" "$T/gt.log" && [ ! -e "$T/daemon/rebuild-gt-daemon-lag" ]; then
  pass "daemon in force again: escalation cleared"
else
  fail "daemon in force again: $(cat "$T/gt.log" 2>/dev/null)"
fi

# Case 32b: an episode is open but the reading is unavailable (state.json has
# no commit) -> neither lag nor health: no clear, the flag stays.
T=$(make_town)
mkdir -p "$T/daemon"; touch "$T/daemon/rebuild-gt-daemon-lag"
printf '{"running": true}\n' > "$T/daemon/state.json"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "daemon-not-in-force" "$T/gt.log" 2>/dev/null; then
  fail "reading unavailable: touched the escalation: $(cat "$T/gt.log")"
else
  pass "reading unavailable: no clear"
fi
[ -e "$T/daemon/rebuild-gt-daemon-lag" ] && pass "reading unavailable: flag kept" || fail "reading unavailable: flag removed"

# Case 32c: a healthy reading whose clear fails keeps the flag for a retry.
T=$(make_town)
in_force_state "$T"; touch "$T/daemon/rebuild-gt-daemon-lag" "$T/clear_refuse"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "escalate clear .*--fingerprint rebuild-gt:daemon-not-in-force" "$T/gt.log" && [ -e "$T/daemon/rebuild-gt-daemon-lag" ]; then
  pass "failed clear: flag kept for the next run"
else
  fail "failed clear: gt.log=$(cat "$T/gt.log" 2>/dev/null) flag=$([ -e "$T/daemon/rebuild-gt-daemon-lag" ] && echo kept || echo gone)"
fi

# Case 32d: a healthy daemon but a young marker still pending -> no clear yet.
T=$(make_town)
in_force_state "$T"; touch "$T/daemon/rebuild-gt-daemon-lag"
age_marker "$T" 5
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "daemon-not-in-force" "$T/gt.log" 2>/dev/null; then fail "marker pending: cleared or escalated: $(cat "$T/gt.log")"; else pass "marker pending: no clear"; fi

# --- claude-7fc: rebuild-gt's own fetch + fast-forward of mayor/rig is a write
# to it, so it runs under install-gt's lock. Another install holding the lock
# defers the run (as install-gt's exit 3 does): no sync, no build, no record.
# That the plugin releases the lock before delegating is what every install
# case above already shows — install-gt would otherwise wait out its own lock
# and exit 3. ---
# Case 33
T=$(make_town)
RIG="$T/gastown/mayor/rig"
BEFORE=$(git -C "$RIG" rev-parse HEAD)
OTHER=$(mktemp -d)
git clone -q "$T/origin.git" "$OTHER"
git -C "$OTHER" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "lands while the lock is held"
git -C "$OTHER" push -q origin main
mkdir -p "$T/daemon"
perl -e '
  use Fcntl qw(:flock);
  open(my $fh, ">>", $ARGV[0]) or die "open: $!";
  flock($fh, LOCK_EX) or die "flock: $!";
  open(my $r, ">", $ARGV[1]) or die; close($r);
  sleep 60;
' "$T/daemon/install-gt.lock" "$T/lock.held" &
HOLDER=$!
for _ in $(seq 1 50); do [ -e "$T/lock.held" ] && break; sleep 0.1; done
rc=$(run_plugin "$T" REBUILD_GT_LOCK_WAIT=1)
kill "$HOLDER" 2>/dev/null || true
wait "$HOLDER" 2>/dev/null || true
if [ ! -e "$T/lock.held" ]; then
  fail "install lock busy: the test never took the lock"
elif [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ] && grep -q "install lock" "$T/run.out"; then
  pass "install lock busy: deferred before syncing mayor/rig"
else
  fail "install lock busy: rc=$rc $(cat "$T/run.out")"
fi
if [ "$(git -C "$RIG" rev-parse HEAD)" = "$BEFORE" ]; then
  pass "install lock busy: mayor/rig not fast-forwarded without the lock"
else
  fail "install lock busy: mayor/rig moved while another install held the lock"
fi
if grep -q "record-run" "$T/gt.log" 2>/dev/null; then
  fail "install lock busy: recorded a run, which would spend the cooldown"
else
  pass "install lock busy: no run record"
fi

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all rebuild-gt tests passed"

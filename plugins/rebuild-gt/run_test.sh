#!/usr/bin/env bash
# End-to-end tests for rebuild-gt/run.sh with a stub gt on PATH and a fake rig.
#
# gt-htx3: the rebuild must yield to a running gate suite (make build competes
# for CPU with load-sensitive tests).
# gt-oqbw: merged is not in force until something installs. A run that
# accomplished nothing exits 3 so the next heartbeat retries it (a run record
# would spend the whole cooldown), the install is verified against the gt the
# town will actually execute, the commits brought into force are recorded, and
# the daemon is restarted last. Refusals (dirty checkout, diverged main,
# install that did not take) are the only things that escalate.
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
  # Fake make targets: build drops a marker so the test can see the branch taken.
  printf 'build:\n\ttouch "$(GT_TEST_TOWN)/build.marker"\nsafe-install:\n\t@true\n' > "$town/gastown/mayor/rig/Makefile"
  git -C "$town/gastown/mayor/rig" add Makefile
  git -C "$town/gastown/mayor/rig" -c user.email=t@t -c user.name=t commit -q -m makefile
  git -C "$town/gastown/mayor/rig" push -q origin main
  # Stub gt driven by fixture files.
  mkdir -p "$town/bin"
  cat > "$town/bin/gt" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "town root") echo "$GT_TEST_TOWN" ;;
  "stale --json") cat "$GT_TEST_TOWN/stale.json" ;;
  "slot status") cat "$GT_TEST_TOWN/slot.json" ;;
  "mq list") cat "$GT_TEST_TOWN/mq.json" ;;
  "daemon restart") echo "daemon restart $*" >> "$GT_TEST_TOWN/gt.log"; echo "Daemon restarted" ;;
  "plugin record-run") echo "record-run $*" >> "$GT_TEST_TOWN/gt.log" ;;
  "plugin sync"|"formula sync") echo "synced" ;;
  "escalate "*) echo "escalate $*" >> "$GT_TEST_TOWN/gt.log" ;;
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
}

run_plugin() {
  # Capture the exit code without set -e aborting the test script on non-zero
  # (om review of gt-htx3: with set -e the rc assertions could never fire).
  local town="$1" rc=0
  # PATH is rebuilt from the stub dir plus system dirs only: the real gt lives
  # in ~/.local/bin and must be unreachable, so a stub miss fails loudly
  # ("gt: command not found") instead of writing a real plugin-run receipt
  # into the town's beads (one such stray receipt was seen on 2026-09-19).
  ( export GT_TEST_TOWN="$town" GT_TOWN_ROOT="$town" PATH="$town/bin:/opt/homebrew/bin:/usr/bin:/bin"; bash "$RUN_SH" ) > "$town/run.out" 2>&1 || rc=$?
  echo "$rc"
}

# holder_json ROLE -> slot.json with that role holding slot 0
holder_json() {
  printf '{"held": true, "busy": false, "slots": [{"index": 0, "held": true, "owner": {"role": "%s", "pid": 1, "slot": 0}}, {"index": 1, "held": false}]}' "$1"
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

# --- Case 4: a suite running outside the gate is not quiet: delete the
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

# --- Case 7: quiet and past the threshold -> build, verify, record the
# commits that came into force, restart the daemon ---
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
if grep -q "daemon restart" "$T/gt.log"; then pass "quiet + behind: daemon restarted"; else fail "quiet + behind: daemon not restarted: $(cat "$T/gt.log")"; fi

# --- Case 8: stale but under the install threshold -> deferred, no build ---
T=$(make_town)
write_stale "$T" 2
rc=$(run_plugin "$T")
if [ "$rc" = "3" ] && [ ! -e "$T/build.marker" ]; then pass "under threshold: deferred"; else fail "under threshold: rc=$rc: $(cat "$T/run.out")"; fi
# ...and the threshold is configurable.
T=$(make_town)
write_stale "$T" 2
rc=$( ( export GT_TEST_TOWN="$T" GT_TOWN_ROOT="$T" REBUILD_GT_INSTALL_THRESHOLD=1 PATH="$T/bin:/opt/homebrew/bin:/usr/bin:/bin"; bash "$RUN_SH" ) > "$T/run.out" 2>&1; echo $? )
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "threshold override: build ran at 2 behind with a threshold of 1"; else fail "threshold override: rc=$rc: $(cat "$T/run.out")"; fi

# --- Case 9: a grubby checkout is refused: skip recorded, no build. It does
# not escalate on its own — gt-bce's drift alarm above is what covers every
# bail path, and adding a second alarm here would fire on states that clear
# by themselves. ---
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
# could never be satisfied by an unmeasurable count (gt-oqbw MAJOR #3). The
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

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all rebuild-gt tests passed"

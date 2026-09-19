#!/usr/bin/env bash
# End-to-end tests for rebuild-gt/run.sh with a stub gt on PATH and a fake rig.
# gt-htx3: the rebuild must yield to a running gate suite (make build competes
# for CPU with load-sensitive tests) and record the skip.
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
  "plugin record-run") echo "record-run $*" >> "$GT_TEST_TOWN/gt.log" ;;
  "plugin sync"|"formula sync") echo "synced" ;;
  "escalate "*) echo "escalate $*" >> "$GT_TEST_TOWN/gt.log" ;;
  "version "*|"version ") echo "gt version test" ;;
  *) echo "stub gt: unhandled: $*" >&2; exit 1 ;;
esac
STUB
  chmod +x "$town/bin/gt"
  echo '{"stale": true, "safe_to_rebuild": true, "commits_behind": 3}' > "$town/stale.json"
  echo "$town"
}

run_plugin() {
  # Capture the exit code without set -e aborting the test script on non-zero
  # (om review of gt-htx3: with set -e the rc assertions could never fire).
  local town="$1" rc=0
  ( export GT_TEST_TOWN="$town" GT_TOWN_ROOT="$town" PATH="$town/bin:$PATH"; bash "$RUN_SH" ) > "$town/run.out" 2>&1 || rc=$?
  echo "$rc"
}

# holder_json ROLE -> slot.json with that role holding slot 0
holder_json() {
  printf '{"held": true, "slots": [{"index": 0, "held": true, "owner": {"role": "%s", "pid": 1, "slot": 0}}, {"index": 1, "held": false}]}' "$1"
}

# --- Case 1: a gate-class role holds a slot -> recorded skip, no build ---
T=$(make_town)
cat > "$T/slot.json" <<'JSON'
{"held": true, "slots": [
  {"index": 0, "held": true, "owner": {"role": "gastown/refinery", "pid": 1, "slot": 0}},
  {"index": 1, "held": false}
]}
JSON
rc=$(run_plugin "$T")
if [ "$rc" != "0" ]; then fail "busy gate: exit $rc, want 0: $(cat "$T/run.out")"; else pass "busy gate: exit 0"; fi
if [ -e "$T/build.marker" ]; then fail "busy gate: make build ran while gastown/refinery held a slot"; else pass "busy gate: no build"; fi
if grep -q -- "--result skipped" "$T/gt.log" 2>/dev/null && grep -q "gate busy gastown/refinery" "$T/gt.log"; then
  pass "busy gate: skip recorded with the holder role"
else
  fail "busy gate: no recorded skip naming the holder: $(cat "$T/gt.log" 2>/dev/null)"
fi

# --- Case 2: pool free and binary stale -> build runs ---
T=$(make_town)
echo '{"held": false, "slots": [{"index": 0, "held": false}, {"index": 1, "held": false}]}' > "$T/slot.json"
rc=$(run_plugin "$T")
if [ "$rc" != "0" ]; then fail "free gate: exit $rc: $(cat "$T/run.out")"; else pass "free gate: exit 0"; fi
if [ -e "$T/build.marker" ]; then pass "free gate: build ran"; else fail "free gate: build did not run: $(cat "$T/run.out")"; fi

# --- Case 3: a polecat holding a shared slot is not a gate -> build runs ---
T=$(make_town)
echo '{"held": true, "slots": [{"index": 0, "held": false}, {"index": 1, "held": true, "owner": {"role": "gastown/polecats/opal", "pid": 2, "slot": 1}}]}' > "$T/slot.json"
rc=$(run_plugin "$T")
if [ -e "$T/build.marker" ]; then pass "polecat holder: build ran"; else fail "polecat holder: build did not run: $(cat "$T/run.out")"; fi

# --- Case 4: every gate-class role suffix defers the rebuild ---
for role in gastown/refinery-batch gastown/main-branch-test om/om-review; do
  T=$(make_town)
  holder_json "$role" > "$T/slot.json"
  rc=$(run_plugin "$T")
  if [ "$rc" = "0" ] && [ ! -e "$T/build.marker" ] && grep -q "gate busy $role" "$T/gt.log" 2>/dev/null; then
    pass "$role holder: deferred and recorded"
  else
    fail "$role holder: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no) log=$(cat "$T/gt.log" 2>/dev/null)"
  fi
done

# --- Case 5: a broken slot status (non-JSON) fails OPEN: the build still runs ---
T=$(make_town)
echo "gt slot status: dolt unreachable" > "$T/slot.json"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "broken slot status: fail-open, build ran"; else fail "broken slot status: rc=$rc marker=$([ -e "$T/build.marker" ] && echo yes || echo no): $(cat "$T/run.out")"; fi

# --- Case 6: a failed build exits non-zero and records a failure (proves the
# rc capture: with set -e swallowing it this assertion could never run) ---
T=$(make_town)
echo '{"held": false, "slots": [{"index": 0, "held": false}]}' > "$T/slot.json"
printf 'build:\n\tfalse\nsafe-install:\n\t@true\n' > "$T/gastown/mayor/rig/Makefile"
git -C "$T/gastown/mayor/rig" -c user.email=t@t -c user.name=t commit -q -am "break build"
git -C "$T/gastown/mayor/rig" push -q origin main
rc=$(run_plugin "$T")
if [ "$rc" != "0" ] && grep -q -- "--result failure" "$T/gt.log" 2>/dev/null; then
  pass "failed build: non-zero exit ($rc) and failure recorded"
else
  fail "failed build: rc=$rc log=$(cat "$T/gt.log" 2>/dev/null)"
fi

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all rebuild-gt tests passed"

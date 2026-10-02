#!/usr/bin/env bash
# Tests for scripts/flake-sweep.sh (gt-22hdp.64): a stub go on PATH records
# every call and fails one run's named test plus a package-level failure of
# another package, so a 2x2 sweep exercises the tally, the table and the exit
# codes without running the real unit tier. A green stub run and the usage and
# setup paths cover the other verdicts.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SWEEP="$SCRIPT_DIR/flake-sweep.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

mkdir -p "$TMP/bin"
# The stub logs its argv, the container opt-in it saw and its nice value, then
# fails one run's named test and a package-level failure of a second package.
# mkdir picks the one failing run: the first concurrent copy to win the race
# closes the door on the rest. STUB_NO_JSON_FAIL models a run that dies with no
# test-level failure line at all.
cat >"$TMP/bin/go" <<'STUB'
#!/usr/bin/env bash
# One write per record: concurrent copies append to the same log, and several
# printfs interleave into spliced lines (the JSON below goes to a per-run log,
# so it needs no such care).
record="go"
for arg in "$@"; do record="$record $arg"; done
record="$record | GT_TEST_DOCKER=${GT_TEST_DOCKER:-unset} nice=$(ps -o nice= -p $$ | tr -d ' ')"
printf '%s\n' "$record" >>"$STUB_LOG"
if [[ -n "${STUB_NO_JSON_FAIL:-}" ]]; then
	printf '%s\n' '{"Action":"pass","Package":"example.com/alpha"}'
	exit 1
fi
if mkdir "$STUB_STATE/once" 2>/dev/null; then
	printf '%s\n' '{"Action":"fail","Package":"example.com/alpha","Test":"TestFlaky"}'
	printf '%s\n' '{"Action":"fail","Package":"example.com/alpha"}'
	printf '%s\n' '{"Action":"fail","Package":"example.com/beta"}'
	exit 1
fi
printf '%s\n' '{"Action":"pass","Package":"example.com/alpha","Test":"TestFlaky"}'
exit 0
STUB
chmod +x "$TMP/bin/go"

# The nice the sweep's stub should see: 10 above this test's own, capped at 20.
base_nice=$(ps -o nice= -p $$ | tr -d ' ')
want_nice=$((base_nice + 10))
if [[ "$want_nice" -gt 20 ]]; then want_nice=20; fi

# run_sweep STATE ITER CONC [VAR=value...] runs the sweep against the stub and
# prints its exit code; stdout is in $TMP/out, stderr in $TMP/err, the stub's
# call log in $TMP/calls. GT_TEST_DOCKER=1 is inherited, as a gate seat would
# pass it: the sweep must override it, never take the container tier.
run_sweep() {
	local state="$1" iter="$2" conc="$3"; shift 3
	: >"$TMP/calls"
	rm -rf "$TMP/logs"
	local rc=0
	env "$@" STUB_LOG="$TMP/calls" STUB_STATE="$state" FLAKE_SWEEP_LOGDIR="$TMP/logs" \
		GT_TEST_DOCKER=1 PATH="$TMP/bin:$PATH" bash "$SWEEP" "$iter" "$conc" >"$TMP/out" 2>"$TMP/err" || rc=$?
	echo "$rc"
}

mkdir -p "$TMP/flaky" "$TMP/green/once"

echo "flake-sweep: a 2x2 sweep with one failing run"
rc=$(run_sweep "$TMP/flaky" 2 2)
if [[ "$rc" == 1 ]]; then pass "exits 1"; else fail "exits 1 (rc=$rc)"; fi
if grep -q -x 'flake-sweep: iterations=2 runs=4 failed_runs=1' "$TMP/out"; then
	pass "summary line"
else
	fail "summary line" "$(cat "$TMP/out")"
fi
if grep -q -E '^test +package +failures/4 +first_log$' "$TMP/out" &&
	grep -q -E '^TestFlaky +example\.com/alpha +1/4 +.*iter[0-9]+-run[0-9]+\.json$' "$TMP/out" &&
	grep -q -E '^example\.com/alpha \(package\) +example\.com/alpha +1/4 ' "$TMP/out" &&
	grep -q -E '^example\.com/beta \(package\) +example\.com/beta +1/4 ' "$TMP/out"; then
	pass "table names the failing test, both package rows and the header"
else
	fail "table names the failing test, both package rows and the header" "$(cat "$TMP/out")"
fi
first_log=$(awk '$1 == "TestFlaky" { print $NF }' "$TMP/out")
if [[ -n "$first_log" && -f "$first_log" ]]; then
	pass "first_log names a log the sweep wrote"
else
	fail "first_log names a log the sweep wrote (got '$first_log')" "$(cat "$TMP/out")"
fi
if [[ "$(find "$TMP/logs" -name '*.json' | wc -l | tr -d ' ')" == 4 ]]; then
	pass "one log per run"
else
	fail "one log per run" "$(find "$TMP/logs" -type f)"
fi
flags_ok=1
for flag in ' -timeout 20m ' ' -count=1 ' ' -json ' ' ./... '; do
	[[ "$(grep -c -F -- "$flag" "$TMP/calls")" == 4 ]] || flags_ok=0
done
if [[ "$(grep -c '^go ' "$TMP/calls")" == 4 ]] &&
	[[ "$(grep -c 'GT_TEST_DOCKER=0' "$TMP/calls")" == 4 ]] &&
	! grep -q 'GT_TEST_DOCKER=1' "$TMP/calls" &&
	[[ "$flags_ok" == 1 ]]; then
	pass "every run is the unit tier with -count=1 -json and GT_TEST_DOCKER=0 over an inherited 1"
else
	fail "every run is the unit tier with -count=1 -json and GT_TEST_DOCKER=0 over an inherited 1" "$(cat "$TMP/calls")"
fi
if [[ "$(grep -c "nice=$want_nice\$" "$TMP/calls")" == 4 ]]; then
	pass "every run is under nice -n 10"
else
	fail "every run is under nice -n 10 (want nice=$want_nice)" "$(cat "$TMP/calls")"
fi

echo "flake-sweep: a green 2x2 sweep"
rc=$(run_sweep "$TMP/green" 2 2)
if [[ "$rc" == 0 ]] && grep -q -x 'flake-sweep: iterations=2 runs=4 failed_runs=0' "$TMP/out" && ! grep -q 'failures/4' "$TMP/out"; then
	pass "exits 0 with no table"
else
	fail "exits 0 with no table (rc=$rc)" "$(cat "$TMP/out" "$TMP/err")"
fi

echo "flake-sweep: a run that dies with no failing-test line"
rc=$(run_sweep "$TMP/green" 1 1 STUB_NO_JSON_FAIL=1)
if [[ "$rc" == 1 ]] && grep -q -x 'flake-sweep: iterations=1 runs=1 failed_runs=1' "$TMP/out" &&
	grep -q -E '^go test \(log names no failing test\) +- +1/1 ' "$TMP/out"; then
	pass "the exit code still decides the verdict"
else
	fail "the exit code still decides the verdict (rc=$rc)" "$(cat "$TMP/out" "$TMP/err")"
fi

echo "flake-sweep: usage and setup errors"
for iter in abc 1x 0; do
	rc=0
	PATH="$TMP/bin:$PATH" bash "$SWEEP" "$iter" 2>/dev/null || rc=$?
	if [[ "$rc" == 2 ]]; then pass "exit 2 on ITER '$iter'"; else fail "exit 2 on ITER '$iter' (rc=$rc)"; fi
done
rc=0
PATH="$TMP/bin:$PATH" bash "$SWEEP" 1 2 3 >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 2 ]]; then pass "exit 2 on too many arguments"; else fail "exit 2 on too many arguments (rc=$rc)"; fi
rc=0
env FLAKE_SWEEP_LOGDIR=/dev/null/nope PATH="$TMP/bin:$PATH" bash "$SWEEP" 1 1 >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 2 ]]; then pass "exit 2 on an unwritable log dir"; else fail "exit 2 on an unwritable log dir (rc=$rc)"; fi

echo "flake-sweep: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]

#!/usr/bin/env bash
# Tests for the Makefile's gate contract (docs/testing.md, "The gate"):
# `make gate`, `make test-slow` and `make test-integration` are the test
# tiers (`make test` only chains them), the gate runs the exec-tax preflight
# (a warning fails nothing, gt-2ycne.1), lint, build and the unit
# tier over every package in that order (no slow tier since gt-ik4a1.9),
# test-slow runs the gate and then the shell tests, neither starts a container
# or takes the container-gate slot, the integration tier runs every
# Docker-backed package listed in internal/testpolicy/docker.txt, and
# test-integration-wall reruns that tier through tierwall for the wall its
# post-merge cadence is judged by (gt-ik4a1.4.5), uncached (-count=1, gt-gq4gl).
#
# The recipe shape is read through `make -n`, which prints recipes without
# running them. That only holds while no gate recipe line references $(MAKE):
# make runs such a line even under -n. The first case checks that too. The
# failure paths are then driven for real, with stub go and golangci-lint on
# PATH and a stub shell-test script (SHELL_TESTS), so every red branch is
# seen to exit non-zero.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [[ $# -gt 1 ]] && printf '%s\n' "$2" | sed 's/^/    /'; FAIL=$((FAIL + 1)); }

dry() { make -C "$ROOT" --no-print-directory -n "$@" 2>&1; }

# line_of prints the first line number of text holding the fixed string.
line_of() { { grep -n -F -- "$2" <<<"$1" || true; } | head -1 | cut -d: -f1; }

echo "gate: dry run"
if out=$(dry gate); then
  pass "make -n gate exits 0"
else
  fail "make -n gate exits 0" "$out"
fi

if grep -q -E '^gate:' "$ROOT/Makefile" && grep -q -E '^test-slow:' "$ROOT/Makefile" && grep -q -E '^test-integration:' "$ROOT/Makefile" && ! awk '/^(gate|test-slow|test-integration|test):/{f=1;next} /^[^\t]/{f=0} f' "$ROOT/Makefile" | grep -q -F '$(MAKE)'; then
  pass "gate, test-slow, test-integration and test recipes do not recurse through \$(MAKE), so -n runs nothing"
else
  fail "gate, test-slow, test-integration and test recipes do not recurse through \$(MAKE), so -n runs nothing"
fi

lint=$(line_of "$out" "golangci-lint run")
build=$(line_of "$out" "go build ./...")
unit=$(line_of "$out" "go test -timeout 20m ./...")
shell=$(line_of "$out" "scripts/test-makefile.sh")
if [[ -n "$lint" && -n "$build" && -n "$unit" && "$lint" -lt "$build" && "$build" -lt "$unit" ]]; then
  pass "gate runs lint, then go build ./..., then the cached unit tier"
else
  fail "gate runs lint, then go build ./..., then the cached unit tier (lines: lint=$lint build=$build unit=$unit)" "$out"
fi
if [[ -z "$shell" ]]; then
  pass "gate leaves the shell tests (scripts/test-makefile.sh) to test-slow"
else
  fail "gate leaves the shell tests (scripts/test-makefile.sh) to test-slow" "$out"
fi
# gt-s1vff: the gate's unit tier is plain go test, so the test result cache
# reruns only the packages a landing can affect; the budget runner's -exec
# wrapper bypassed the cache and reran every test on every landing. Budgets
# stay enforced by make tier-check (the hourly sweep), not the landing path.
if grep -q -F 'go test -timeout 20m ./...' <<<"$out" && ! grep -q -F 'cmd/budget' <<<"$out" && ! grep -q -E -- '-count[= ]1|-exec' <<<"$out" && ! grep -q -E -- '-slow( |=)|slow\.txt' <<<"$out"; then
  pass "gate runs the unit tier over every package through the test cache (no budget -exec, no -count=1)"
else
  fail "gate runs the unit tier over every package through the test cache (no budget -exec, no -count=1)" "$out"
fi
if ! grep -q -F -- '-strict-wall' <<<"$out" && tout=$(dry tier-check) && grep -q -F 'cmd/budget -fast-tier -strict-wall -- -timeout 20m ./...' <<<"$tout" && ! grep -q -E 'golangci-lint|go build|slot +run' <<<"$tout"; then
  pass "gate only warns on wall time; make tier-check runs the unit tier with -strict-wall and no lint or build (gt-z7qtk)"
else
  fail "gate only warns on wall time; make tier-check runs the unit tier with -strict-wall and no lint or build (gt-z7qtk)" "${tout:-}"
fi
if grep -q -E 'gate: PASSED in \$\{wall\}s wall' <<<"$out"; then
  pass "gate prints its wall time"
else
  fail "gate prints its wall time" "$out"
fi

if grep -q -F 'golangci-lint run --timeout=5m --allow-serial-runners' <<<"$out" && lout=$(dry lint) && ! grep -q -F -- '--allow-serial-runners' <<<"$lout" && grep -q -F 'scripts/lint-lock-wait.sh 600 golangci-lint run --timeout=5m' <<<"$lout"; then
  pass "gate's lint waits on the lint lock; plain make lint retries a contended lint for LINT_LOCK_WAIT (gt-uoppq)"
else
  fail "gate's lint waits on the lint lock; plain make lint retries a contended lint for LINT_LOCK_WAIT (gt-uoppq)" "$(grep -F 'golangci-lint run' <<<"$out" ${lout:+<<<"$lout"})"
fi

if grep -q -E 'slot +run' <<<"$out"; then
  fail "gate never takes the container-gate slot" "$(grep -E 'slot +run' <<<"$out")"
else
  pass "gate never takes the container-gate slot"
fi

if grep -q -F 'GT_TEST_DOCKER=0 go test -timeout 20m ./...' <<<"$out"; then
  pass "gate writes the container opt-in off where the unit tier starts"
else
  fail "gate writes the container opt-in off where the unit tier starts" "$out"
fi
if grep -q -E 'GT_TEST_DOCKER=[^0]' <<<"$out"; then
  fail "gate never turns the container opt-in on or defaults it" "$(grep -E 'GT_TEST_DOCKER=[^0]' <<<"$out")"
else
  pass "gate never turns the container opt-in on or defaults it"
fi
# An inherited opt-in must not change what the gate runs. The gate's start
# time, baked into the recipe when make parses it, is masked.
gate_shape() { dry gate | grep -v -E '^(go: |Warning: )' | sed -E 's/date \+%s\) - [0-9]+/date +%s) - START/'; }
if [[ "$(GT_TEST_DOCKER=1 gate_shape)" == "$(gate_shape)" ]]; then
  pass "an inherited GT_TEST_DOCKER=1 does not change the gate"
else
  fail "an inherited GT_TEST_DOCKER=1 does not change the gate"
fi

if [[ "$(grep -v -E '^[[:space:]]*#' <<<"$out" | grep -c -E -- '(^| )-timeout[ =]')" == 1 ]] && grep -q -F -- 'go test -timeout 20m ./...' <<<"$out"; then
  pass "gate carries one -timeout, on the unit tier's go test (the one gate definition)"
else
  fail "gate carries one -timeout, on the unit tier's go test (the one gate definition)" "$(grep -n -E -- '-timeout' <<<"$out")"
fi

echo "deleted entry points"
# Read from the Makefile, never through make -n: a deleted target's old recipe
# recursed through $(MAKE), which make runs even under -n.
for t in test-changed; do
  if grep -q -E "^$t:" "$ROOT/Makefile" || grep -q -E "^\.PHONY:.* $t( |$)" "$ROOT/Makefile"; then
    fail "make $t is gone"
  else
    pass "make $t is gone"
  fi
done
if [[ "$(grep -E '^test:' "$ROOT/Makefile")" == "test: test-slow test-integration" ]] && ! awk '/^test:/{f=1;next} /^[^\t]/{f=0} f' "$ROOT/Makefile" | grep -q .; then
  pass "make test only chains test-slow (the gate, then the shell tests) and test-integration, with no recipe of its own"
else
  fail "make test only chains test-slow (the gate, then the shell tests) and test-integration, with no recipe of its own" "$(grep -A3 -E '^test:' "$ROOT/Makefile")"
fi
if [[ ! -e "$ROOT/internal/testpolicy/slow.txt" ]] && ! grep -q -E 'SLOW_(LIST|PKGS)|slow\.txt' "$ROOT/Makefile"; then
  pass "the slow tier is gone: no slow.txt, and the Makefile names none (gt-ik4a1.9)"
else
  fail "the slow tier is gone: no slow.txt, and the Makefile names none (gt-ik4a1.9)" "$(grep -n -E 'SLOW_(LIST|PKGS)|slow\.txt' "$ROOT/Makefile")"
fi
if grep -q -E '(^|[^A-Za-z_])PKGS *\?=' "$ROOT/Makefile"; then
  fail "no PKGS default for a changed-package gate"
else
  pass "no PKGS default for a changed-package gate"
fi
if grep -q -F 'GT_TEST_DOCKER:-' "$ROOT/Makefile"; then
  fail "no recipe defaults the container opt-in from the environment" "$(grep -n -F 'GT_TEST_DOCKER:-' "$ROOT/Makefile")"
else
  pass "no recipe defaults the container opt-in from the environment"
fi

echo "test-slow"
if sout=$(dry test-slow); then
  pass "make -n test-slow exits 0"
else
  fail "make -n test-slow exits 0" "$sout"
fi
sgate=$(line_of "$sout" "go test -timeout 20m ./...")
sshell=$(line_of "$sout" "GT_TEST_DOCKER=0 bash scripts/test-makefile.sh")
if [[ -n "$sgate" && -n "$sshell" && "$sgate" -lt "$sshell" ]] && grep -q -F 'go test -timeout 20m ./...' <<<"$sout" && ! grep -q -E 'GT_TEST_DOCKER=[^0]|slot +run' <<<"$sout"; then
  pass "test-slow runs the gate, then the shell tests, containers off, no slot"
else
  fail "test-slow runs the gate, then the shell tests, containers off, no slot (lines: gate=$sgate shell=$sshell)" "$sout"
fi

# The container suite's test-Dolt init pool must be as wide as the scheduler
# town slot count (gt-ik4a1.4.12). Package beads reads the pool override in
# init(), so TestMain is too late: it has to be in the environment before the
# test binary starts, which is the make target. The count is read from
# schedulerTownSlots, the channel that defines it, so raising that cap without
# the recipes fails here.
town_slots="$(sed -n 's/.*schedulerTownSlots = make(chan struct{}, \([0-9][0-9]*\)).*/\1/p' "$ROOT/internal/cmd/scheduler_integration_test.go")"
init_pool_lines() { grep -c -F -- "GT_TEST_DOLT_INIT_CONCURRENCY=$town_slots" <<<"$1" || true; }

echo "test-integration"
if iout=$(dry test-integration); then
  pass "make -n test-integration exits 0"
else
  fail "make -n test-integration exits 0" "$iout"
fi
if grep -q -F -- '-tags integration' <<<"$iout" && grep -q -F 'GT_TEST_DOCKER=1' <<<"$iout"; then
  pass "test-integration runs the integration tag with containers on"
else
  fail "test-integration runs the integration tag with containers on" "$iout"
fi
# The tier is every integration-tagged test, not the ones whose name a filter
# admits (gt-ik4a1.2): a name filter is how ~120 tests ran nowhere.
no_run_filter() { ! grep -q -E -- '(^|[[:space:]])-(test\.)?run[ =]' <<<"$1"; }
if no_run_filter "$iout"; then
  pass "test-integration passes no -run name filter"
else
  fail "test-integration passes no -run name filter" "$iout"
fi
# The check itself is exercised on the shape it exists to catch: the recipe
# this target carried before gt-ik4a1.2, whose -run ^TestIntegration admitted
# only the tests named TestIntegration* and left every other integration-tagged
# test running nowhere.
if no_run_filter "GT_TEST_DOCKER=1 go test -tags integration -run '^TestIntegration' -timeout 20m ./..."; then
  fail "the no-filter check rejects the pre-gt-ik4a1.2 recipe that passed -run"
else
  pass "the no-filter check rejects the pre-gt-ik4a1.2 recipe that passed -run"
fi
missing=""
while IFS= read -r pkg; do
  pkg="${pkg%%#*}"
  pkg="$(echo "$pkg" | tr -d '[:space:]')"
  [[ -z "$pkg" ]] && continue
  grep -q -F "./$pkg" <<<"$iout" || missing="$missing $pkg"
done <"$ROOT/internal/testpolicy/docker.txt"
if [[ -z "$missing" ]]; then
  pass "test-integration runs every package in internal/testpolicy/docker.txt"
else
  fail "test-integration runs every package in internal/testpolicy/docker.txt; missing:$missing" "$iout"
fi
if [[ -n "$town_slots" && "$(init_pool_lines "$iout")" == 2 ]]; then
  pass "test-integration sets the test-Dolt init pool to the town slot count ($town_slots) on both commands"
else
  fail "test-integration sets the test-Dolt init pool to the town slot count (read '$town_slots' from schedulerTownSlots)" "$iout"
fi

echo "test-integration-wall"
if wout=$(dry test-integration-wall); then
  pass "make -n test-integration-wall exits 0"
else
  fail "make -n test-integration-wall exits 0" "$wout"
fi
# The wall report is only worth reading if it measures the tier the cadence
# runs (gt-ik4a1.4.5), so each of test-integration's two suite commands must
# reappear verbatim but for -json and -count=1. A suite command carries the
# container opt-in and the test-Dolt init pool ahead of go test (gt-ik4a1.4.12),
# so match on the opt-in and `go test` rather than a fixed prefix.
wall_missing=""
suite_seen=0
while IFS= read -r line; do
  [[ "$line" != *'GT_TEST_DOCKER=1'* || "$line" != *'go test '* ]] && continue
  suite_seen=$((suite_seen + 1))
  want="${line/'go test '/'go test -json -count=1 '}"
  grep -q -F -- "$want" <<<"$wout" || wall_missing="$wall_missing [$want]"
done <<<"$iout"
if [[ "$suite_seen" == 2 && -z "$wall_missing" ]]; then
  pass "test-integration-wall runs test-integration's two go test commands, each -json -count=1"
else
  fail "test-integration-wall runs test-integration's two go test commands, each -json -count=1 (suite lines: $suite_seen); missing:$wall_missing" "$wout"
fi
# -count=1 keeps the wall an uncached run (gt-gq4gl): the number decides the
# cadence switch (gt-ik4a1.4), and without it a second run of unchanged source
# is served from the Go test cache, whose wall can sit far under a cold run's
# (6.6s cached against 209s cold on 2026-10-03). Both commands, exactly.
wall_uncached() { [[ "$(grep -c -E -- 'go test -json -count=1' <<<"$1")" == 2 ]]; }
if wall_uncached "$wout"; then
  pass "test-integration-wall is an uncached run: both commands carry -count=1 (gt-gq4gl)"
else
  fail "test-integration-wall is an uncached run: both commands carry -count=1 (gt-gq4gl)" "$wout"
fi
# The check is exercised on the shape it exists to catch: the pre-gt-gq4gl
# recipe, whose two commands took -json and no -count.
if wall_uncached "GT_TEST_DOCKER=1 go test -json -tags integration -timeout 20m ./..."; then
  fail "the -count=1 check rejects the pre-gt-gq4gl wall recipe"
else
  pass "the -count=1 check rejects the pre-gt-gq4gl wall recipe"
fi
# The flag rides after $(INTEGRATION_GO_TEST), not inside the default, or a CI
# runner swap would drop it and silently restore the cached measurement.
if wswap=$(dry test-integration-wall INTEGRATION_GO_TEST="go test -x") && [[ "$(grep -c -F -- 'go test -x -json -count=1' <<<"$wswap")" == 2 ]]; then
  pass "INTEGRATION_GO_TEST still swaps the wall's runner, and -count=1 rides after it (gt-gq4gl)"
else
  fail "INTEGRATION_GO_TEST still swaps the wall's runner, and -count=1 rides after it (gt-gq4gl)" "$wswap"
fi
if grep -q -F -- '-count=1' <<<"$(grep -E '^INTEGRATION_GO_TEST' "$ROOT/Makefile" || true)"; then
  fail "the runner default (INTEGRATION_GO_TEST) carries no -count=1; the wall's recipe does"
else
  pass "the runner default (INTEGRATION_GO_TEST) carries no -count=1; the wall's recipe does"
fi
wall_runs=$(grep -c -E -- 'GT_TEST_DOCKER=1.*go test -json' <<<"$wout" || true)
wall_tool=$(grep -c -F 'cmd/tierwall' <<<"$wout" || true)
# set -o pipefail is what keeps a go test that died before emitting a FAIL
# event red; without it the pipeline's exit status is tierwall's alone.
if [[ "$wall_runs" == 2 && "$wall_tool" == 1 ]] && grep -q -F 'set -o pipefail' <<<"$wout" && grep -q -F 'test-integration-wall: internal/testpolicy/docker.txt lists no package' <<<"$wout" && no_run_filter "$wout"; then
  pass "test-integration-wall pipes both commands through one tierwall run under pipefail, containers on, no -run filter"
else
  fail "test-integration-wall pipes both commands through one tierwall run under pipefail, containers on, no -run filter (lines: runs=$wall_runs tierwall=$wall_tool)" "$wout"
fi
if [[ -n "$town_slots" && "$(init_pool_lines "$wout")" == 2 ]]; then
  pass "test-integration-wall sets the test-Dolt init pool to the town slot count ($town_slots) on both commands"
else
  fail "test-integration-wall sets the test-Dolt init pool to the town slot count (read '$town_slots' from schedulerTownSlots)" "$wout"
fi

echo "gate: failure paths, driven with stubs"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
# The go stub logs each call with the container opt-in it saw, and fails the
# subcommand named in STUB_GO_FAIL (build or test).
cat >"$TMP/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "go $1 GT_TEST_DOCKER=${GT_TEST_DOCKER:-unset}" >>"$STUB_LOG"
if [[ "$1" == test && -n "${STUB_GO_INTERRUPT:-}" ]]; then
  # Signal the recipe shell (the parent), then wait to be killed.
  kill -TERM "$PPID"
  sleep 20
  echo "go test survived the interrupt" >>"$STUB_LOG"
fi
[[ "$1" == "${STUB_GO_FAIL:-}" ]] && exit 1
exit 0
STUB
cat >"$TMP/bin/golangci-lint" <<'STUB'
#!/usr/bin/env bash
echo "golangci-lint $*" >>"$STUB_LOG"
[[ "$1" == run && -n "${STUB_LINT_FAIL:-}" ]] && exit 1
exit 0
STUB
cat >"$TMP/shell-tests.sh" <<'STUB'
echo "shell-tests" >>"$STUB_LOG"
[[ -n "${STUB_SHELL_FAIL:-}" ]] && exit 1
exit 0
STUB
# The exec-tax preflight, which the real gate reaches through `go run`: the
# go stub would swallow that call, so every case below points
# EXEC_TAX_PREFLIGHT at this stub. It warns only when EXEC_TAX_WARN is set,
# and exits 0 either way.
cat >"$TMP/preflight.sh" <<'STUB'
#!/usr/bin/env bash
if [[ -n "${EXEC_TAX_WARN:-}" ]]; then
  echo "gate: WARNING exec tax 180 ms/exec in this process tree: a fresh executable here waits on a macOS scan or a throttled process, and the unit tier builds one per package (see gt-2ycne.1)" >&2
fi
exit 0
STUB
chmod +x "$TMP/bin/go" "$TMP/bin/golangci-lint" "$TMP/preflight.sh"

# run_gate [make flags] -- VAR=value... runs the real gate recipe against the
# stubs, with an inherited GT_TEST_DOCKER=1 the recipe must override, and
# prints its exit code; the log of stub calls is in $TMP/calls, stderr in
# $TMP/err. Cases past lint pass `-o lint` (make treats the target as up to
# date), and the lint case `-o docs-lint`, so the real docs-lint (~20 s) runs
# once, in the green case.
run_gate() {
  local flags=()
  while [[ $# -gt 0 && "$1" != -- ]]; do flags+=("$1"); shift; done
  [[ $# -gt 0 ]] && shift
  : >"$TMP/calls"
  local rc=0
  env "$@" STUB_LOG="$TMP/calls" GT_TEST_DOCKER=1 PATH="$TMP/bin:$PATH" \
    make -C "$ROOT" --no-print-directory "${flags[@]}" "${TARGET:-gate}" SHELL_TESTS="$TMP/shell-tests.sh" EXEC_TAX_PREFLIGHT="$TMP/preflight.sh" >"$TMP/out" 2>"$TMP/err" || rc=$?
  echo "$rc"
}

rc=$(run_gate)
if [[ "$rc" == 0 ]] && grep -q -E 'gate: PASSED in [0-9]+s wall' "$TMP/err" && grep -q -x 'go test GT_TEST_DOCKER=0' "$TMP/calls" && ! grep -q -x 'shell-tests' "$TMP/calls" && grep -q -F 'golangci-lint run --timeout=5m --allow-serial-runners' "$TMP/calls"; then
  pass "green stubs: exit 0 with the wall printed, lint waited on the lock, only the Go suite ran, and it saw GT_TEST_DOCKER=0 despite an inherited 1"
else
  fail "green stubs: exit 0 with the wall printed, lint waited on the lock, only the Go suite ran, and it saw GT_TEST_DOCKER=0 despite an inherited 1 (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

# The preflight's warning path: a taxed host says so, and the gate still
# passes. -o lint so the real docs-lint runs once, in the green case above.
rc=$(run_gate -o lint -- EXEC_TAX_WARN=1)
if [[ "$rc" == 0 ]] && grep -q -F 'gate: WARNING exec tax 180 ms/exec' "$TMP/err" && grep -q -E 'gate: PASSED in [0-9]+s wall' "$TMP/err"; then
  pass "a taxed host: the gate warns on the exec tax and still passes (gt-2ycne.1)"
else
  fail "a taxed host: the gate warns on the exec tax and still passes (gt-2ycne.1) (rc=$rc)" "$(cat "$TMP/err")"
fi

rc=$(run_gate -o docs-lint -- STUB_LINT_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -E '^golangci-lint run' "$TMP/calls" && ! grep -q -E '^go (build|test)' "$TMP/calls"; then
  pass "lint fails: non-zero, nothing built or tested"
else
  fail "lint fails: non-zero, nothing built or tested (rc=$rc)" "$(cat "$TMP/calls")"
fi

rc=$(run_gate -o lint -- STUB_GO_FAIL=build)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at build' "$TMP/err" && ! grep -q -E '^go test' "$TMP/calls"; then
  pass "build fails: non-zero, names the build stage, unit tier never starts"
else
  fail "build fails: non-zero, names the build stage, unit tier never starts (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o lint -- STUB_GO_FAIL=test)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at unit tier (Go suite' "$TMP/err" && ! grep -q -F 'gate: PASSED' "$TMP/err"; then
  pass "Go suite fails: non-zero, names the Go half"
else
  fail "Go suite fails: non-zero, names the Go half (rc=$rc)" "$(cat "$TMP/err")"
fi

# An interrupted gate stops the Go suite and exits red. The stub go run
# signals the recipe shell (its parent) and then waits to be killed; a trap
# that did not fire would let it finish and the gate would pass.
rc=$(run_gate -o lint -- STUB_GO_INTERRUPT=1)
sleep 1
if [[ "$rc" != 0 ]] && ! grep -q -F 'gate: PASSED' "$TMP/err" && ! grep -q -F 'survived' "$TMP/calls"; then
  pass "interrupted gate: non-zero, and the trap stopped the running suite"
else
  fail "interrupted gate: non-zero, and the trap stopped the running suite (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -o lint)
if [[ "$rc" == 0 ]] && grep -q -x 'go test GT_TEST_DOCKER=0' "$TMP/calls" && grep -q -x 'shell-tests' "$TMP/calls" && grep -q -E 'gate: PASSED in [0-9]+s wall' "$TMP/err" && grep -q -F 'test-slow: PASSED' "$TMP/err"; then
  pass "test-slow green stubs: exit 0, the Go suite and the shell tests ran with GT_TEST_DOCKER=0"
else
  fail "test-slow green stubs: exit 0, the Go suite and the shell tests ran with GT_TEST_DOCKER=0 (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -o lint -- STUB_GO_FAIL=test)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at unit tier (Go suite' "$TMP/err" && ! grep -q -F 'test-slow: PASSED' "$TMP/err"; then
  pass "test-slow Go suite fails: non-zero, the gate names the Go half"
else
  fail "test-slow Go suite fails: non-zero, the gate names the Go half (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -o lint -- STUB_SHELL_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -F 'test-slow: FAILED at shell tests' "$TMP/err"; then
  pass "test-slow shell tests fail: non-zero, names the shell tests"
else
  fail "test-slow shell tests fail: non-zero, names the shell tests (rc=$rc)" "$(cat "$TMP/err")"
fi

# scripts/lint-lock-wait.sh against a lint stub that reports the module lock
# held for its first STUB_CONTEND calls (gt-uoppq).
cat >"$TMP/contended-lint" <<'STUB'
#!/usr/bin/env bash
n=$(( $(wc -l <"$STUB_LOG") ))
echo "lint" >>"$STUB_LOG"
if (( n < STUB_CONTEND )); then
  echo "Error: parallel golangci-lint is running"
  exit 3
fi
[[ -n "${STUB_FINDING:-}" ]] && { echo "x.go:1: finding"; exit 1; }
exit 0
STUB
chmod +x "$TMP/contended-lint"
lock_wait() {
  : >"$TMP/calls"
  local rc=0
  env "${@:2}" STUB_LOG="$TMP/calls" LINT_LOCK_POLL=0 bash "$ROOT/scripts/lint-lock-wait.sh" "$1" "$TMP/contended-lint" >"$TMP/out" 2>"$TMP/err" || rc=$?
  echo "$rc"
}

rc=$(lock_wait 5 STUB_CONTEND=2)
if [[ "$rc" == 0 ]] && [[ $(wc -l <"$TMP/calls") -eq 3 ]] && grep -q -F 'retrying in 0s' "$TMP/err"; then
  pass "lint-lock-wait: a lint contended twice is retried and passes"
else
  fail "lint-lock-wait: a lint contended twice is retried and passes (rc=$rc)" "$(cat "$TMP/calls" "$TMP/out" "$TMP/err")"
fi

rc=$(lock_wait 0 STUB_CONTEND=99)
if [[ "$rc" == 3 ]] && [[ $(wc -l <"$TMP/calls") -eq 1 ]] && grep -q -F 'parallel golangci-lint is running' "$TMP/out" && grep -q -F 'nothing was linted' "$TMP/err"; then
  pass "lint-lock-wait: still contended at the wait bound exits with the lint's code and keeps the lock line"
else
  fail "lint-lock-wait: still contended at the wait bound exits with the lint's code and keeps the lock line (rc=$rc)" "$(cat "$TMP/calls" "$TMP/out" "$TMP/err")"
fi

rc=$(lock_wait 5 STUB_CONTEND=0 STUB_FINDING=1)
if [[ "$rc" == 1 ]] && [[ $(wc -l <"$TMP/calls") -eq 1 ]] && grep -q -F 'finding' "$TMP/out"; then
  pass "lint-lock-wait: a lint with findings is never retried"
else
  fail "lint-lock-wait: a lint with findings is never retried (rc=$rc)" "$(cat "$TMP/calls" "$TMP/out" "$TMP/err")"
fi

rc=0
env PATH="$TMP/bin:$PATH" STUB_LOG="$TMP/calls" make -C "$ROOT" --no-print-directory test-integration DOCKER_PKGS= >"$TMP/out" 2>"$TMP/err" || rc=$?
if [[ "$rc" != 0 ]] && grep -q -F 'lists no package' "$TMP/err"; then
  pass "test-integration refuses an empty docker.txt instead of testing nothing"
else
  fail "test-integration refuses an empty docker.txt instead of testing nothing (rc=$rc)" "$(cat "$TMP/err")"
fi

echo ""
echo "makefile-gate_test: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]

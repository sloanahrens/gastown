#!/usr/bin/env bash
# Tests for the Makefile's gate contract (docs/testing.md, "The gate"):
# `make gate`, `make test-slow` and `make test-integration` are the test
# tiers (`make test` only chains them), the gate runs lint, build and the fast
# tier in that order, skipping the packages in internal/testpolicy/slow.txt
# that test-slow runs (gt-z862q), neither starts a container or takes the
# container-gate slot, and the integration tier runs every Docker-backed
# package listed in internal/testpolicy/docker.txt.
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
unit=$(line_of "$out" "internal/testpolicy/cmd/budget")
shell=$(line_of "$out" "scripts/test-makefile.sh")
if [[ -n "$lint" && -n "$build" && -n "$unit" && "$lint" -lt "$build" && "$build" -lt "$unit" ]]; then
  pass "gate runs lint, then go build ./..., then the budget runner"
else
  fail "gate runs lint, then go build ./..., then the budget runner (lines: lint=$lint build=$build unit=$unit)" "$out"
fi
if [[ -z "$shell" ]]; then
  pass "gate leaves the shell tests (scripts/test-makefile.sh) to the slow tier"
else
  fail "gate leaves the shell tests (scripts/test-makefile.sh) to the slow tier" "$out"
fi
if grep -q -F 'cmd/budget -fast-tier -slow internal/testpolicy/slow.txt --' <<<"$out"; then
  pass "gate runs the fast tier: skips slow.txt, warns on a package over testpolicy.FastTierMaxWall"
else
  fail "gate runs the fast tier: skips slow.txt, warns on a package over testpolicy.FastTierMaxWall" "$out"
fi
if ! grep -q -F -- '-strict-wall' <<<"$out" && tout=$(dry tier-check) && grep -q -F 'cmd/budget -fast-tier -strict-wall -slow internal/testpolicy/slow.txt --' <<<"$tout" && ! grep -q -E 'golangci-lint|go build|slot +run' <<<"$tout"; then
  pass "gate only warns on wall time; make tier-check runs the fast tier with -strict-wall and no lint or build (gt-z7qtk)"
else
  fail "gate only warns on wall time; make tier-check runs the fast tier with -strict-wall and no lint or build (gt-z7qtk)" "${tout:-}"
fi
if grep -q -E 'gate: PASSED in \$\{wall\}s wall' <<<"$out"; then
  pass "gate prints its wall time"
else
  fail "gate prints its wall time" "$out"
fi

if grep -q -F 'golangci-lint run --timeout=5m --allow-serial-runners' <<<"$out" && ! dry lint | grep -q -F -- '--allow-serial-runners'; then
  pass "gate's lint waits on the lint lock; plain make lint keeps the fast contention exit"
else
  fail "gate's lint waits on the lint lock; plain make lint keeps the fast contention exit" "$(grep -F 'golangci-lint run' <<<"$out")"
fi

if grep -q -E 'slot +run' <<<"$out"; then
  fail "gate never takes the container-gate slot" "$(grep -E 'slot +run' <<<"$out")"
else
  pass "gate never takes the container-gate slot"
fi

if grep -q -F 'GT_TEST_DOCKER=0 go run ./internal/testpolicy/cmd/budget' <<<"$out"; then
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

if [[ "$(grep -v -E '^[[:space:]]*#' <<<"$out" | grep -c -E -- '(^| )-timeout[ =]')" == 1 ]] && grep -q -F -- 'slow.txt -- -timeout 20m ./...' <<<"$out"; then
  pass "gate carries one -timeout, on the budget runner (the one gate definition)"
else
  fail "gate carries one -timeout, on the budget runner (the one gate definition)" "$(grep -n -E -- '-timeout' <<<"$out")"
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
if [[ "$(grep -E '^test:' "$ROOT/Makefile")" == "test: gate test-slow test-integration" ]] && ! awk '/^test:/{f=1;next} /^[^\t]/{f=0} f' "$ROOT/Makefile" | grep -q .; then
  pass "make test only chains gate, test-slow and test-integration, with no recipe of its own"
else
  fail "make test only chains gate, test-slow and test-integration, with no recipe of its own" "$(grep -A3 -E '^test:' "$ROOT/Makefile")"
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
missing=""
while IFS= read -r pkg; do
  pkg="${pkg%%#*}"
  pkg="$(echo "$pkg" | awk '{print $1}')"
  [[ -z "$pkg" ]] && continue
  grep -q -E "\./$pkg( |$)" <<<"$sout" || missing="$missing $pkg"
  grep -q -E "\./$pkg( |$)" <<<"$out" && missing="$missing (gate runs $pkg)"
done <"$ROOT/internal/testpolicy/slow.txt"
if [[ -z "$missing" ]] && grep -q -F 'GT_TEST_DOCKER=0 go run ./internal/testpolicy/cmd/budget -- -timeout 20m ./internal/' <<<"$sout" && grep -q -F 'GT_TEST_DOCKER=0 bash scripts/test-makefile.sh' <<<"$sout" && ! grep -q -E 'GT_TEST_DOCKER=[^0]|slot +run' <<<"$sout"; then
  pass "test-slow runs every slow.txt package and the shell tests, containers off, no slot"
else
  fail "test-slow runs every slow.txt package and the shell tests, containers off, no slot;$missing" "$sout"
fi

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

echo "gate: failure paths, driven with stubs"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
# The go stub logs each call with the container opt-in it saw, and fails the
# subcommand named in STUB_GO_FAIL (build, run or test).
cat >"$TMP/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "go $1 GT_TEST_DOCKER=${GT_TEST_DOCKER:-unset}" >>"$STUB_LOG"
if [[ "$1" == run && -n "${STUB_GO_INTERRUPT:-}" ]]; then
  # Signal the recipe shell (the parent), then wait to be killed.
  kill -TERM "$PPID"
  sleep 20
  echo "go run survived the interrupt" >>"$STUB_LOG"
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
chmod +x "$TMP/bin/go" "$TMP/bin/golangci-lint"

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
    make -C "$ROOT" --no-print-directory "${flags[@]}" "${TARGET:-gate}" SHELL_TESTS="$TMP/shell-tests.sh" >"$TMP/out" 2>"$TMP/err" || rc=$?
  echo "$rc"
}

rc=$(run_gate)
if [[ "$rc" == 0 ]] && grep -q -E 'gate: PASSED in [0-9]+s wall' "$TMP/err" && grep -q -x 'go run GT_TEST_DOCKER=0' "$TMP/calls" && ! grep -q -x 'shell-tests' "$TMP/calls" && grep -q -F 'golangci-lint run --timeout=5m --allow-serial-runners' "$TMP/calls"; then
  pass "green stubs: exit 0 with the wall printed, lint waited on the lock, only the Go suite ran, and it saw GT_TEST_DOCKER=0 despite an inherited 1"
else
  fail "green stubs: exit 0 with the wall printed, lint waited on the lock, only the Go suite ran, and it saw GT_TEST_DOCKER=0 despite an inherited 1 (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o docs-lint -- STUB_LINT_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -E '^golangci-lint run' "$TMP/calls" && ! grep -q -E '^go (build|run)' "$TMP/calls"; then
  pass "lint fails: non-zero, nothing built or tested"
else
  fail "lint fails: non-zero, nothing built or tested (rc=$rc)" "$(cat "$TMP/calls")"
fi

rc=$(run_gate -o lint -- STUB_GO_FAIL=build)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at build' "$TMP/err" && ! grep -q -E '^go run' "$TMP/calls"; then
  pass "build fails: non-zero, names the build stage, unit tier never starts"
else
  fail "build fails: non-zero, names the build stage, unit tier never starts (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o lint -- STUB_GO_FAIL=run)
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

rc=$(TARGET=test-slow run_gate)
if [[ "$rc" == 0 ]] && grep -q -x 'go run GT_TEST_DOCKER=0' "$TMP/calls" && grep -q -x 'shell-tests' "$TMP/calls" && grep -q -E 'test-slow: PASSED in [0-9]+s wall' "$TMP/err"; then
  pass "test-slow green stubs: exit 0, the Go suite and the shell tests ran with GT_TEST_DOCKER=0"
else
  fail "test-slow green stubs: exit 0, the Go suite and the shell tests ran with GT_TEST_DOCKER=0 (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -- STUB_GO_FAIL=run)
if [[ "$rc" != 0 ]] && grep -q -F 'test-slow: FAILED at Go suite' "$TMP/err" && grep -q -x 'shell-tests' "$TMP/calls"; then
  pass "test-slow Go suite fails: non-zero, names it, the shell tests still run"
else
  fail "test-slow Go suite fails: non-zero, names it, the shell tests still run (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -- STUB_SHELL_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -F 'test-slow: FAILED at shell tests' "$TMP/err"; then
  pass "test-slow shell tests fail: non-zero, names the shell tests"
else
  fail "test-slow shell tests fail: non-zero, names the shell tests (rc=$rc)" "$(cat "$TMP/err")"
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

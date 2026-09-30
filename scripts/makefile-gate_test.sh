#!/usr/bin/env bash
# Tests for the Makefile's gate contract (docs/testing.md, "The gate"):
# `make gate` and `make test-integration` are the only test entry points,
# the gate runs lint, build and the unit tier in that order, it never starts a
# container or takes the container-gate slot, and the integration tier runs
# every Docker-backed package listed in internal/testpolicy/docker.txt.
#
# The recipe shape is read through `make -n`, which prints recipes without
# running them. That only holds while no gate recipe line references $(MAKE):
# make runs such a line even under -n. The first case checks that too. The
# failure paths are then driven for real, with stub go and golangci-lint on
# PATH and a stub shell-test script (GATE_SHELL_TESTS), so every red branch
# is seen to exit non-zero.
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

if grep -q -E '^gate:' "$ROOT/Makefile" && grep -q -E '^test-integration:' "$ROOT/Makefile" && ! awk '/^(gate|test-integration):/{f=1;next} /^[^\t]/{f=0} f' "$ROOT/Makefile" | grep -q -F '$(MAKE)'; then
  pass "gate and test-integration recipes do not recurse through \$(MAKE), so -n runs nothing"
else
  fail "gate and test-integration recipes do not recurse through \$(MAKE), so -n runs nothing"
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
if [[ -n "$shell" ]]; then
  pass "gate runs the shell tests (scripts/test-makefile.sh) as part of the unit tier"
else
  fail "gate runs the shell tests (scripts/test-makefile.sh) as part of the unit tier" "$out"
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
# An inherited opt-in must not change what the gate runs.
if [[ "$(GT_TEST_DOCKER=1 dry gate | grep -v -E '^(go: |Warning: )')" == "$(dry gate | grep -v -E '^(go: |Warning: )')" ]]; then
  pass "an inherited GT_TEST_DOCKER=1 does not change the gate"
else
  fail "an inherited GT_TEST_DOCKER=1 does not change the gate"
fi

if [[ "$(grep -v -E '^[[:space:]]*#' <<<"$out" | grep -c -E -- '(^| )-timeout[ =]')" == 1 ]] && grep -q -F 'cmd/budget -- -timeout 20m ./...' <<<"$out"; then
  pass "gate carries one -timeout, on the budget runner (the one gate definition)"
else
  fail "gate carries one -timeout, on the budget runner (the one gate definition)" "$(grep -n -E -- '-timeout' <<<"$out")"
fi

echo "deleted entry points"
# Read from the Makefile, never through make -n: a deleted target's old recipe
# recursed through $(MAKE), which make runs even under -n.
for t in test test-changed; do
  if grep -q -E "^$t:" "$ROOT/Makefile" || grep -q -E "^\.PHONY:.* $t( |$)" "$ROOT/Makefile"; then
    fail "make $t is gone"
  else
    pass "make $t is gone"
  fi
done
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

if grep -q -F 'plugins/dolt-snapshots' <<<"$out" && grep -q -F '(cd "$m" && go build -o "$out/" ./...)' <<<"$out"; then
  pass "gate builds every nested module in its own directory"
else
  fail "gate builds every nested module in its own directory" "$out"
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
[[ "$1" == "${STUB_GO_FAIL:-}" ]] && exit 1
exit 0
STUB
cat >"$TMP/bin/golangci-lint" <<'STUB'
#!/usr/bin/env bash
echo "golangci-lint $1" >>"$STUB_LOG"
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
    make -C "$ROOT" --no-print-directory "${flags[@]}" gate GATE_SHELL_TESTS="$TMP/shell-tests.sh" >"$TMP/out" 2>"$TMP/err" || rc=$?
  echo "$rc"
}

rc=$(run_gate)
if [[ "$rc" == 0 ]] && grep -q -F 'gate: PASSED' "$TMP/err" && grep -q -x 'go run GT_TEST_DOCKER=0' "$TMP/calls" && grep -q -x 'shell-tests' "$TMP/calls"; then
  pass "green stubs: exit 0, both unit halves ran, the suite saw GT_TEST_DOCKER=0 despite an inherited 1"
else
  fail "green stubs: exit 0, both unit halves ran, the suite saw GT_TEST_DOCKER=0 despite an inherited 1 (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o docs-lint -- STUB_LINT_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -x 'golangci-lint run' "$TMP/calls" && ! grep -q -E '^go (build|run)' "$TMP/calls"; then
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

rc=$(run_gate -o lint -- STUB_SHELL_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at unit tier (shell tests' "$TMP/err" && ! grep -q -F 'gate: PASSED' "$TMP/err"; then
  pass "shell tests fail: non-zero, names the shell half"
else
  fail "shell tests fail: non-zero, names the shell half (rc=$rc)" "$(cat "$TMP/err")"
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

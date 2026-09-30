#!/usr/bin/env bash
# Tests for the Makefile's gate contract (docs/testing.md, "The gate"):
# `make gate` and `make test-integration` are the only test entry points,
# the gate runs lint, build and the unit tier in that order, it never starts a
# container or takes the container-gate slot, and the integration tier runs
# every Docker-backed package listed in internal/testpolicy/docker.txt.
#
# Everything is read through `make -n`, which prints recipes without running
# them. That only holds while no gate recipe line references $(MAKE): make
# runs such a line even under -n. The first case checks that too.
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

if grep -q -E -- '-timeout[ =]20m' <<<"$out"; then
  fail "gate carries no -timeout 20m variant"
else
  pass "gate carries no -timeout 20m variant"
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
if grep -q -E -- '-timeout[ =]20m' "$ROOT/Makefile"; then
  fail "no -timeout 20m anywhere in the Makefile" "$(grep -n -E -- '-timeout[ =]20m' "$ROOT/Makefile")"
else
  pass "no -timeout 20m anywhere in the Makefile"
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

echo ""
echo "makefile-gate_test: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]

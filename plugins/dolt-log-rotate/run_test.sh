#!/usr/bin/env bash
# Tests for dolt-log-rotate/run.sh's town-root requirement (gt-fcxe9.5). gt
# has no command that prints the town root, so with GT_TOWN_ROOT unset the
# plugin must stop with the reason instead of guessing a town.
set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/run.sh"
PASS=0
FAIL=0
record_pass() { PASS=$((PASS + 1)); printf 'PASS: %s\n' "$1"; }
record_fail() { FAIL=$((FAIL + 1)); printf 'FAIL: %s\n' "$1"; }

CASE_DIR=$(mktemp -d)
trap 'rm -rf "$CASE_DIR"' EXIT

# Unset: fails with the reason.
rc=0
( unset GT_TOWN_ROOT; bash "$SCRIPT" ) > "$CASE_DIR/out.log" 2>&1 || rc=$?
if [ "$rc" -ne 0 ]; then record_pass "unset town root: fails (exit $rc)"; else record_fail "unset town root: exit 0: $(cat "$CASE_DIR/out.log")"; fi
if grep -q "GT_TOWN_ROOT is unset" "$CASE_DIR/out.log"; then record_pass "unset town root: says why"; else record_fail "unset town root: no reason: $(cat "$CASE_DIR/out.log")"; fi

# Set, with no log file yet: nothing to rotate, exits 0.
mkdir -p "$CASE_DIR/town/daemon"
rc=0
GT_TOWN_ROOT="$CASE_DIR/town" bash "$SCRIPT" > "$CASE_DIR/out.log" 2>&1 || rc=$?
if [ "$rc" -eq 0 ] && grep -q "Nothing to do" "$CASE_DIR/out.log"; then
  record_pass "set town root, no log: nothing to do"
else
  record_fail "set town root, no log: exit $rc: $(cat "$CASE_DIR/out.log")"
fi

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]

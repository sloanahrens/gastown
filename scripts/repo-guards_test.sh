#!/usr/bin/env bash
# Tests for scripts/repo-guards.sh: a clean repo passes; a replace directive,
# a replace block, or a tracked .beads/issues.jsonl fails; an untracked
# issues.jsonl (a local bd export) passes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GUARDS="$SCRIPT_DIR/repo-guards.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

check() {
	local name="$1" want="$2" got=0
	bash "$GUARDS" "$TMP/repo" >/dev/null 2>&1 || got=$?
	if [[ "$want" == "pass" && "$got" -eq 0 ]] || [[ "$want" == "fail" && "$got" -ne 0 ]]; then
		echo "  PASS: $name"; PASS=$((PASS + 1))
	else
		echo "  FAIL: $name (want $want, exit $got)"; FAIL=$((FAIL + 1))
	fi
}

fresh() {
	rm -rf "$TMP/repo"; mkdir -p "$TMP/repo/.beads"
	git -C "$TMP/repo" init -q
	printf 'module example.com/x\n\ngo 1.26\n' >"$TMP/repo/go.mod"
}

fresh; check "clean repo" pass
fresh; printf 'replace example.com/y => ../y\n' >>"$TMP/repo/go.mod"; check "replace directive" fail
fresh; printf 'replace (\n\texample.com/y => ../y\n)\n' >>"$TMP/repo/go.mod"; check "replace block" fail
fresh; echo '{}' >"$TMP/repo/.beads/issues.jsonl"; check "untracked issues.jsonl" pass
fresh; echo '{}' >"$TMP/repo/.beads/issues.jsonl"; git -C "$TMP/repo" add .beads/issues.jsonl; check "tracked issues.jsonl" fail

echo "repo-guards: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]

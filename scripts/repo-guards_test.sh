#!/usr/bin/env bash
# Tests for scripts/repo-guards.sh: a clean repo passes; a replace directive,
# a replace block, a tracked .beads/issues.jsonl, or a tracked .go file gofmt -s
# would rewrite fails; an untracked issues.jsonl (a local bd export), an
# untracked .go file and a testdata fixture pass.
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

# The gofmt -s guard (gt-kc9ck). Only tracked files count, as for the
# issues.jsonl guard, and testdata/ is skipped the way the Go toolchain
# skips it.
fresh; printf 'package x\n' >"$TMP/repo/a.go"; git -C "$TMP/repo" add a.go; check "gofmt-clean go file" pass
fresh; printf 'package x\n\nfunc  f( ) {}\n' >"$TMP/repo/a.go"; git -C "$TMP/repo" add a.go; check "unformatted tracked go file" fail
fresh; printf 'package x\n\nfunc f(s []int) []int { return s[0:len(s)] }\n' >"$TMP/repo/a.go"; git -C "$TMP/repo" add a.go; check "drift only gofmt -s sees" fail
fresh; mkdir -p "$TMP/repo/testdata"; printf 'package x\n\nfunc  f( ) {}\n' >"$TMP/repo/testdata/fixture.go"; git -C "$TMP/repo" add testdata/fixture.go; check "unformatted testdata fixture" pass
fresh; printf 'package x\n\nfunc  f( ) {}\n' >"$TMP/repo/a.go"; check "unformatted untracked go file" pass

echo "repo-guards: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]

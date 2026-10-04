#!/usr/bin/env bash
# Tests for scripts/tier-sweep.sh's summary line: each tier's line ends with
# the elapsed time of that tier, in the daemon log's rounded style (gt-iqzr0).
# Each case builds a throwaway repo holding the script under test and one stub
# shell test, so no real tier runs.
set -uo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/tier-sweep.sh"
FAILS=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

# new_repo BODY prints a repo whose scripts/x_test.sh runs BODY and whose
# scripts/tier-sweep.sh is the real one. The stub names itself so the sweep's
# own dedupe, which reads each tracked test's path back out of SHELL_TESTS,
# runs it once and not twice.
new_repo() {
  local body=$1 r
  r=$(mktemp -d "${TMPDIR:-/tmp}/tier-sweep.XXXXXX")
  git -C "$r" init -q -b main
  git -C "$r" config user.email t@example.com
  git -C "$r" config user.name t
  mkdir -p "$r/scripts"
  cp "$SRC" "$r/scripts/tier-sweep.sh"
  printf '#!/usr/bin/env bash\n# scripts/x_test.sh\nexit %s\n' "$body" > "$r/scripts/x_test.sh"
  git -C "$r" add -A
  git -C "$r" commit -q -m base
  echo "$r"
}

# sweep REPO [TIER...] runs the sweep's shell tier in REPO and prints its
# combined output; the exit code lands in $rc.
run_tier() {
  local r=$1
  shift
  out=$(cd "$r" && SHELL_TESTS=scripts/x_test.sh bash scripts/tier-sweep.sh "$@" 2>&1)
  rc=$?
}

# --- A green tier ends its summary line with the elapsed time ---
R=$(new_repo 0)
run_tier "$R" shell
line=$(grep -E '^tier-sweep: shell (GREEN|RED) ' <<<"$out")
if [ "$rc" = 0 ] && grep -Eq '^tier-sweep: shell GREEN passed=1 failed=0 skipped=0 \(logs [^)]*\) in [0-9]+(h[0-9]+m[0-9]+s|m[0-9]+s|s)$' <<<"$line"; then
  pass "green tier: summary line ends with its elapsed time ($line)"
else
  fail "green tier: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- A red tier carries the time too, after the names and the log marker ---
R=$(new_repo 1)
run_tier "$R" shell
line=$(grep -E '^tier-sweep: shell (GREEN|RED) ' <<<"$out")
if [ "$rc" != 0 ] && grep -Eq '^tier-sweep: shell RED passed=0 failed=1 skipped=0 failed: scripts/x_test\.sh \(logs [^)]*\) in [0-9]+(h[0-9]+m[0-9]+s|m[0-9]+s|s)$' <<<"$line"; then
  pass "red tier: summary line ends with its elapsed time ($line)"
else
  fail "red tier: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- An unknown tier runs nothing, so it prints no summary line at all ---
R=$(new_repo 0)
run_tier "$R" bogus
if [ "$rc" = 2 ] && ! grep -qE '^tier-sweep: bogus ' <<<"$out" && grep -q 'unknown tier bogus' <<<"$out"; then
  pass "unknown tier: no summary line, exit 2"
else
  fail "unknown tier: rc=$rc out=$out"
fi
rm -rf "$R"

[ "$FAILS" = 0 ] || { echo "$FAILS failure(s)"; exit 1; }
echo "tier-sweep_test.sh: all cases passed"

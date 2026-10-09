#!/usr/bin/env bash
# Tests for scripts/post-land-shell.sh: the post-land check runs the shell
# tier only when main changed a shell-test input inside the window (gt-8p8h7).
# Each case builds a throwaway repo whose scripts/tier-sweep.sh is a stub
# that records it ran.
set -uo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/post-land-shell.sh"
FAILS=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

# new_repo prints a repo with the script under test, a stub tier-sweep and
# one base commit dated well outside any window.
new_repo() {
  local r
  r=$(mktemp -d "${TMPDIR:-/tmp}/post-land-shell.XXXXXX")
  git -C "$r" init -q -b main
  git -C "$r" config user.email t@example.com
  git -C "$r" config user.name t
  git -C "$r" config core.hooksPath /dev/null
  mkdir -p "$r/scripts" "$r/internal/x"
  cp "$SRC" "$r/scripts/post-land-shell.sh"
  printf '#!/usr/bin/env bash\necho TIER-RAN "$@"\n' > "$r/scripts/tier-sweep.sh"
  echo base > "$r/internal/x/x.go"
  git -C "$r" add -A
  GIT_COMMITTER_DATE="2020-01-01T00:00:00Z" GIT_AUTHOR_DATE="2020-01-01T00:00:00Z" \
    git -C "$r" commit -q -m base
  echo "$r"
}

# commit_at writes path in repo and commits it at date ("" = now).
commit_at() {
  local r=$1 path=$2 date=$3
  mkdir -p "$r/$(dirname "$path")"
  echo "$RANDOM" >> "$r/$path"
  git -C "$r" add -A
  if [ -n "$date" ]; then
    GIT_COMMITTER_DATE="$date" GIT_AUTHOR_DATE="$date" git -C "$r" commit -q -m "change $path"
  else
    git -C "$r" commit -q -m "change $path"
  fi
}

# --- A Go-only landing skips the tier ---
R=$(new_repo)
commit_at "$R" internal/x/x.go ""
out=$(bash "$R/scripts/post-land-shell.sh" 2>&1); rc=$?
if [ "$rc" = 0 ] && ! grep -q TIER-RAN <<<"$out" && grep -q "shell tier skipped" <<<"$out"; then
  pass "go-only change: skipped"
else
  fail "go-only change: rc=$rc out=$out"
fi
rm -rf "$R"

# --- A recent change to each kind of input runs the tier ---
# internal/cmd/scheduler_integration_test.go is Go but a shell-tier input
# (gt-tqxdd): makefile-gate_test.sh reads schedulerTownSlots out of it.
for path in scripts/install-gt.sh plugins/p/run.sh .githooks/pre-push Makefile internal/testpolicy/docker.txt internal/cmd/scheduler_integration_test.go; do
  R=$(new_repo)
  commit_at "$R" "$path" ""
  out=$(bash "$R/scripts/post-land-shell.sh" 2>&1); rc=$?
  if [ "$rc" = 0 ] && grep -q "TIER-RAN shell" <<<"$out" && grep -q "  $path" <<<"$out"; then
    pass "input $path: tier ran and names it"
  else
    fail "input $path: rc=$rc out=$out"
  fi
  rm -rf "$R"
done

# --- A coalesced landing: the input changed one commit before the tip ---
R=$(new_repo)
commit_at "$R" scripts/a.sh ""
commit_at "$R" internal/x/x.go ""
out=$(bash "$R/scripts/post-land-shell.sh" 2>&1)
grep -q "TIER-RAN shell" <<<"$out" && pass "coalesced: an earlier landing's input still runs the tier" \
  || fail "coalesced: $out"
rm -rf "$R"

# --- An input changed outside the window does not run it ---
R=$(new_repo)
commit_at "$R" scripts/a.sh "2020-06-01T00:00:00Z"
commit_at "$R" internal/x/x.go ""
out=$(bash "$R/scripts/post-land-shell.sh" 2>&1)
! grep -q TIER-RAN <<<"$out" && pass "old input: skipped" || fail "old input: $out"
rm -rf "$R"

# --- The tier's exit code is the script's ---
R=$(new_repo)
printf '#!/usr/bin/env bash\necho TIER-RAN; exit 3\n' > "$R/scripts/tier-sweep.sh"
commit_at "$R" scripts/a.sh ""
bash "$R/scripts/post-land-shell.sh" >/dev/null 2>&1; rc=$?
[ "$rc" = 3 ] && pass "red tier: exit code passes through" || fail "red tier: rc=$rc, want 3"
rm -rf "$R"

if [ "$FAILS" -gt 0 ]; then
  echo "post-land-shell tests: $FAILS failure(s)"
  exit 1
fi
echo "all post-land-shell tests passed"

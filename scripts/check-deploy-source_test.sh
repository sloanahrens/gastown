#!/usr/bin/env bash
# Tests for scripts/check-deploy-source.sh, the "deploy merged code only"
# precondition of the Makefile's install and safe-install targets (gt-o848l).
#
# The defects being guarded: the old check compared HEAD with the branch's
# OWN upstream and skipped entirely on a detached HEAD or a branch with no
# upstream, so a crew branch merely behind main was refused (and
# SKIP_UPDATE_CHECK=1 became routine) while an unmerged branch build with no
# upstream installed town-wide.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CHECK="$SCRIPT_DIR/check-deploy-source.sh"

TMPDIR=""
PASS=0
FAIL=0

cleanup() {
  if [[ -n "$TMPDIR" && -d "$TMPDIR" ]]; then
    rm -rf "$TMPDIR"
  fi
}
trap cleanup EXIT

git_q() { git -c user.email=t@t -c user.name=t -c commit.gpgsign=false "$@" >/dev/null 2>&1; }

# setup: a bare origin with two commits on main, and a clone of it in $WORK.
setup() {
  cleanup
  TMPDIR="$(mktemp -d)"
  git_q init --bare -b main "$TMPDIR/origin.git"
  git_q clone "$TMPDIR/origin.git" "$TMPDIR/seed"
  (cd "$TMPDIR/seed" && git_q checkout -b main &&
    mkdir -p .beads && echo a > file && echo x > .beads/issues.jsonl &&
    git_q add -A && git_q commit -m one &&
    echo b > file && git_q commit -am two && git_q push origin main)
  git_q clone "$TMPDIR/origin.git" "$TMPDIR/work"
  WORK="$TMPDIR/work"
}

# run: execute the check in $WORK with extra env; sets RC and OUT.
run() {
  set +e
  OUT="$(cd "$WORK" && env "$@" bash "$CHECK" 2>&1)"
  RC=$?
  set -e
}

assert_rc() {
  local test_name="$1" expected="$2"
  if [[ "$RC" -eq "$expected" ]]; then
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $test_name (exit $RC, want $expected)"
    echo "$OUT" | sed 's/^/    | /'
    FAIL=$((FAIL + 1))
  fi
}

assert_out() {
  local test_name="$1" needle="$2"
  if grep -qF -- "$needle" <<<"$OUT"; then
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $test_name (output lacks: $needle)"
    echo "$OUT" | sed 's/^/    | /'
    FAIL=$((FAIL + 1))
  fi
}

echo "=== check-deploy-source.sh tests ==="

setup
run X=1
assert_rc "HEAD equal to origin/main passes" 0

setup
(cd "$WORK" && git_q reset --hard HEAD~1)
run X=1
assert_rc "HEAD behind origin/main passes (forward-only guards downgrades)" 0
assert_out "behind origin/main warns" "behind origin/main"

setup
(cd "$WORK" && echo c > file && git_q commit -am unmerged)
run X=1
assert_rc "unmerged local commit is refused" 1
assert_out "refusal names the fix" "ALLOW_UNMERGED=1"

setup
(cd "$WORK" && echo c > file && git_q commit -am unmerged && git_q checkout --detach HEAD)
run X=1
assert_rc "detached HEAD on an unmerged commit is refused (was skipped)" 1

setup
(cd "$WORK" && git_q checkout -b feature --no-track origin/main)
run X=1
assert_rc "branch without upstream at a merged commit passes" 0

setup
(cd "$WORK" && git_q checkout -b feature --no-track && echo c > file && git_q commit -am unmerged)
run X=1
assert_rc "branch without upstream on an unmerged commit is refused (was skipped)" 1

setup
(cd "$WORK" && echo c > file && git_q commit -am unmerged)
run ALLOW_UNMERGED=1
assert_rc "ALLOW_UNMERGED=1 overrides the refusal" 0
assert_out "ALLOW_UNMERGED override is loud" "UNMERGED"

setup
(cd "$WORK" && echo dirty > file)
run X=1
assert_rc "tracked modification is refused" 1

setup
(cd "$WORK" && echo dirty > .beads/issues.jsonl && echo junk > untracked.txt)
run X=1
assert_rc ".beads/ edits and untracked files do not block" 0

setup
(cd "$TMPDIR/seed" && echo c > file && git_q commit -am three && git_q push origin main)
before="$(git -C "$WORK" rev-parse origin/main)"
run SKIP_UPDATE_CHECK=1
after="$(git -C "$WORK" rev-parse origin/main)"
assert_rc "SKIP_UPDATE_CHECK=1 still passes a merged HEAD" 0
if [[ "$before" == "$after" ]]; then
  echo "  PASS: SKIP_UPDATE_CHECK=1 skips the fetch (gt-9jax)"
  PASS=$((PASS + 1))
else
  echo "  FAIL: SKIP_UPDATE_CHECK=1 fetched origin/main"
  FAIL=$((FAIL + 1))
fi

setup
(cd "$WORK" && echo c > file && git_q commit -am unmerged)
run SKIP_UPDATE_CHECK=1
assert_rc "SKIP_UPDATE_CHECK=1 no longer skips the merged check" 1

setup
run DEPLOY_MAIN_REF=origin/nope
assert_rc "missing main ref is refused" 1

echo
echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]

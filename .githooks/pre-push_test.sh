#!/bin/bash
# Test suite for the pre-push hook integration branch guardrails.
# Creates temporary git repos to simulate push scenarios.
#
# Usage: bash .githooks/pre-push_test.sh

set -euo pipefail

# The suite runs from inside a polecat worktree as often as not, and the hook
# now refuses default-branch pushes from a polecat (gt-ibt8) — so clear the
# ambient polecat signals first and let each test state its own context
# explicitly. Without this, every default-branch test below would pass or fail
# depending on who ran the suite.
unset GT_ROLE GT_POLECAT GT_POLECAT_PATH 2>/dev/null || true
unset GT_REFINERY_MERGE GT_DONE_DIRECT_MERGE 2>/dev/null || true

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$SCRIPT_DIR/pre-push"
PASS=0
FAIL=0
TMPDIR=""
DEFAULT_BRANCH=""

cleanup() {
  cd /tmp  # Ensure CWD exists before removing TMPDIR
  if [[ -n "$TMPDIR" && -d "$TMPDIR" ]]; then
    rm -rf "$TMPDIR"
  fi
  TMPDIR=""
}
trap cleanup EXIT

# setup_repos [local_subdir]
# The optional subdir places the "local" clone at a chosen relative path, so a
# test can run the hook from a polecat-shaped worktree
# (<town>/<rig>/polecats/<name>/<repo>) without any env vars set.
setup_repos() {
  local local_rel="${1:-local}"
  TMPDIR=$(mktemp -d)
  # Create a bare "remote" repo
  git init --bare "$TMPDIR/remote.git" >/dev/null 2>&1
  # Clone it as the "local" repo
  mkdir -p "$(dirname "$TMPDIR/$local_rel")"
  git clone "$TMPDIR/remote.git" "$TMPDIR/$local_rel" >/dev/null 2>&1
  cd "$TMPDIR/$local_rel"
  git config user.email "test@test.com"
  git config user.name "Test"
  # Initial commit
  echo "init" > file.txt
  git add file.txt
  git commit -m "initial" >/dev/null 2>&1
  # Detect the default branch name (main or master)
  DEFAULT_BRANCH=$(git branch --show-current)
  git push origin "$DEFAULT_BRANCH" >/dev/null 2>&1
  # Set up origin/HEAD so hook can detect default branch
  git remote set-head origin "$DEFAULT_BRANCH" >/dev/null 2>&1
  # Copy the hook
  cp "$HOOK" "$TMPDIR/$local_rel/.git/hooks/pre-push"
  chmod +x "$TMPDIR/$local_rel/.git/hooks/pre-push"
}

run_hook() {
  # Simulate pre-push stdin: local_ref local_sha remote_ref remote_sha
  local local_ref=$1 local_sha=$2 remote_ref=$3 remote_sha=$4
  echo "$local_ref $local_sha $remote_ref $remote_sha" | bash "$HOOK" "origin" 2>&1
}

run_hook_env() {
  # run_hook with extra environment: run_hook_env "VAR=value VAR2=value" <refs...>
  local envs=$1
  shift
  # shellcheck disable=SC2086
  local local_ref=$1 local_sha=$2 remote_ref=$3 remote_sha=$4
  # shellcheck disable=SC2086
  echo "$local_ref $local_sha $remote_ref $remote_sha" | env $envs bash "$HOOK" "origin" 2>&1
}

# assert_live_block runs a REAL `git push` (not a hand-fed stdin) and fails if
# it was NOT refused. Running the push for real is what makes the polecat
# refusal below a live proof rather than a matcher unit test (gt-ibt8).
assert_live_block() {
  # assert_live_block <name> <envs> <refspec>
  local test_name=$1 envs=$2 refspec=$3 out="" status=0
  # shellcheck disable=SC2086
  out=$(env $envs git push origin "$refspec" 2>&1) || status=$?
  if [[ $status -eq 0 ]]; then
    echo "  FAIL: $test_name (expected live push to be refused, but it succeeded)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: $test_name"
    printf '%s\n' "$out" | sed 's/^/        | /'
    PASS=$((PASS + 1))
  fi
}

assert_live_pass() {
  # assert_live_pass <name> <envs> <refspec>
  local test_name=$1 envs=$2 refspec=$3 out="" status=0
  # shellcheck disable=SC2086
  out=$(env $envs git push origin "$refspec" 2>&1) || status=$?
  if [[ $status -eq 0 ]]; then
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $test_name (expected live push to succeed, but it was refused)"
    printf '%s\n' "$out" | sed 's/^/        | /'
    FAIL=$((FAIL + 1))
  fi
}

get_sha() {
  git rev-parse "$1"
}

assert_pass() {
  local test_name=$1
  shift
  if "$@" >/dev/null 2>&1; then
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $test_name (expected pass, got block)"
    FAIL=$((FAIL + 1))
  fi
}

assert_block() {
  local test_name=$1
  shift
  if "$@" >/dev/null 2>&1; then
    echo "  FAIL: $test_name (expected block, got pass)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  fi
}

echo "=== Pre-push hook test suite ==="
echo ""

# Test 1: Normal push to default branch (no integration content)
echo "Test 1: Normal push to default branch (no integration content)"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "change1" >> file.txt
git add file.txt && git commit -m "normal change" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Normal push allowed" run_hook "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 2: Push to polecat/* branch
echo "Test 2: Push to polecat/* branch"
setup_repos
cd "$TMPDIR/local"
git checkout -b polecat/worker1 >/dev/null 2>&1
echo "polecat work" >> file.txt
git add file.txt && git commit -m "polecat work" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Polecat push allowed" run_hook "refs/heads/polecat/worker1" "$local_sha" "refs/heads/polecat/worker1" "0000000000000000000000000000000000000000"
cleanup

# Test 3: Push to integration/* branch
echo "Test 3: Push to integration/* branch"
setup_repos
cd "$TMPDIR/local"
git checkout -b integration/epic-1 >/dev/null 2>&1
echo "integration work" >> file.txt
git add file.txt && git commit -m "integration work" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Integration branch push allowed" run_hook "refs/heads/integration/epic-1" "$local_sha" "refs/heads/integration/epic-1" "0000000000000000000000000000000000000000"
cleanup

# Test 4: Push to feature/* without upstream remote (blocked)
echo "Test 4: Push to feature/* without upstream remote"
setup_repos
cd "$TMPDIR/local"
git checkout -b feature/thing >/dev/null 2>&1
echo "feature" >> file.txt
git add file.txt && git commit -m "feature" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_block "Feature branch blocked (no upstream)" run_hook "refs/heads/feature/thing" "$local_sha" "refs/heads/feature/thing" "0000000000000000000000000000000000000000"
cleanup

# Test 5: Push to feature/* with upstream remote (allowed)
echo "Test 5: Push to feature/* with upstream remote"
setup_repos
cd "$TMPDIR/local"
git remote add upstream "$TMPDIR/remote.git" >/dev/null 2>&1
git checkout -b feature/thing >/dev/null 2>&1
echo "feature" >> file.txt
git add file.txt && git commit -m "feature" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Feature branch allowed (upstream exists)" run_hook "refs/heads/feature/thing" "$local_sha" "refs/heads/feature/thing" "0000000000000000000000000000000000000000"
cleanup

# Test 6: Push to default branch with integration merge (no env var) — BLOCKED
echo "Test 6: Push to default branch with integration merge (no env var)"
setup_repos
cd "$TMPDIR/local"
# Create and push an integration branch
git checkout -b integration/epic-2 >/dev/null 2>&1
echo "epic work" >> file.txt
git add file.txt && git commit -m "epic work" >/dev/null 2>&1
git push origin integration/epic-2 >/dev/null 2>&1
# Fetch so refs/remotes/origin/integration/epic-2 exists
git fetch origin >/dev/null 2>&1
# Back to default branch, merge the integration branch
git checkout "$DEFAULT_BRANCH" >/dev/null 2>&1
remote_sha=$(get_sha HEAD)
git merge --no-ff integration/epic-2 -m "land integration" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
unset GT_INTEGRATION_LAND 2>/dev/null || true
assert_block "Integration merge blocked (no env var)" run_hook "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 7: Push to default branch with integration merge + GT_INTEGRATION_LAND=1 — ALLOWED
echo "Test 7: Push to default branch with integration merge + GT_INTEGRATION_LAND=1"
setup_repos
cd "$TMPDIR/local"
git checkout -b integration/epic-3 >/dev/null 2>&1
echo "epic work" >> file.txt
git add file.txt && git commit -m "epic work" >/dev/null 2>&1
git push origin integration/epic-3 >/dev/null 2>&1
git fetch origin >/dev/null 2>&1
git checkout "$DEFAULT_BRANCH" >/dev/null 2>&1
remote_sha=$(get_sha HEAD)
git merge --no-ff integration/epic-3 -m "land integration" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
GT_INTEGRATION_LAND=1 assert_pass "Integration merge allowed (env var set)" run_hook "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 8: Push to default branch with non-integration merge — allowed
echo "Test 8: Push to default branch with non-integration merge"
setup_repos
cd "$TMPDIR/local"
# Create a local feature branch and merge it (no need to push to origin)
git checkout -b feature/normal >/dev/null 2>&1
echo "feature work" >> file.txt
git add file.txt && git commit -m "feature work" >/dev/null 2>&1
git checkout "$DEFAULT_BRANCH" >/dev/null 2>&1
remote_sha=$(get_sha HEAD)
git merge --no-ff feature/normal -m "merge feature" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Non-integration merge allowed" run_hook "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 9: Tag push — allowed
echo "Test 9: Tag push"
setup_repos
cd "$TMPDIR/local"
local_sha=$(get_sha HEAD)
assert_pass "Tag push allowed" run_hook "refs/tags/v1.0.0" "$local_sha" "refs/tags/v1.0.0" "0000000000000000000000000000000000000000"
cleanup

# Test 10: Push to default branch with fast-forward integration merge (no merge commit) — BLOCKED
echo "Test 10: Push to default branch with ff integration merge (no merge commit)"
setup_repos
cd "$TMPDIR/local"
git checkout -b integration/epic-4 >/dev/null 2>&1
echo "epic ff work" >> file.txt
git add file.txt && git commit -m "epic ff work" >/dev/null 2>&1
git push origin integration/epic-4 >/dev/null 2>&1
git fetch origin >/dev/null 2>&1
git checkout "$DEFAULT_BRANCH" >/dev/null 2>&1
remote_sha=$(get_sha HEAD)
git merge --ff-only integration/epic-4 >/dev/null 2>&1
local_sha=$(get_sha HEAD)
unset GT_INTEGRATION_LAND 2>/dev/null || true
assert_block "FF integration merge blocked" run_hook "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 11: Off-branch push — HEAD on a session branch, pushing the default branch
# (the classic `git push origin main` from a feature branch). HEAD mismatch — BLOCKED.
echo "Test 11: Off-branch push (HEAD on session branch, pushing default)"
setup_repos
cd "$TMPDIR/local"
git checkout -b session/x >/dev/null 2>&1
echo "session work" >> file.txt
git add file.txt && git commit -m "session work" >/dev/null 2>&1
default_sha=$(get_sha "$DEFAULT_BRANCH")
unset GT_ALLOW_OFFBRANCH_PUSH 2>/dev/null || true
assert_block "Off-branch default push blocked (HEAD mismatch)" run_hook "refs/heads/$DEFAULT_BRANCH" "$default_sha" "refs/heads/$DEFAULT_BRANCH" "$default_sha"
cleanup

# Test 12: Off-branch push with GT_ALLOW_OFFBRANCH_PUSH=1 — ALLOWED (override).
echo "Test 12: Off-branch push with GT_ALLOW_OFFBRANCH_PUSH=1"
setup_repos
cd "$TMPDIR/local"
git checkout -b session/y >/dev/null 2>&1
echo "session work" >> file.txt
git add file.txt && git commit -m "session work" >/dev/null 2>&1
default_sha=$(get_sha "$DEFAULT_BRANCH")
GT_ALLOW_OFFBRANCH_PUSH=1 assert_pass "Off-branch push allowed with override" run_hook "refs/heads/$DEFAULT_BRANCH" "$default_sha" "refs/heads/$DEFAULT_BRANCH" "$default_sha"
cleanup

# Test 13: Off-branch deletion push (zero local sha) — not a HEAD mismatch, ALLOWED.
echo "Test 13: Off-branch deletion push (zero local sha)"
setup_repos
cd "$TMPDIR/local"
git checkout -b session/z >/dev/null 2>&1
default_sha=$(get_sha "$DEFAULT_BRANCH")
unset GT_ALLOW_OFFBRANCH_PUSH 2>/dev/null || true
assert_pass "Off-branch deletion not blocked by HEAD guard" run_hook "refs/heads/$DEFAULT_BRANCH" "0000000000000000000000000000000000000000" "refs/heads/$DEFAULT_BRANCH" "$default_sha"
cleanup

# Test 14: Notes ref push (e.g. refs/notes/om) — allowed
echo "Test 14: Notes ref push"
setup_repos
cd "$TMPDIR/local"
local_sha=$(get_sha HEAD)
assert_pass "Notes ref push allowed" run_hook "refs/notes/om" "$local_sha" "refs/notes/om" "0000000000000000000000000000000000000000"
cleanup

# ---------------------------------------------------------------------------
# Polecat main-push refusal (gt-ibt8)
# ---------------------------------------------------------------------------

# Test 15: polecat (bare GT_ROLE=polecat) pushing the default branch — BLOCKED
echo "Test 15: Polecat push to default branch (GT_ROLE=polecat)"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "polecat change" >> file.txt
git add file.txt && git commit -m "polecat change" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_block "Polecat default push blocked (GT_ROLE=polecat)" run_hook_env "GT_ROLE=polecat" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 16: polecat compound role (the form the session manager sets) — BLOCKED
echo "Test 16: Polecat push to default branch (GT_ROLE=<rig>/polecats/<name>)"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "polecat change" >> file.txt
git add file.txt && git commit -m "polecat change" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_block "Polecat default push blocked (compound GT_ROLE)" run_hook_env "GT_ROLE=gastown/polecats/flint" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 17: detached HEAD, `git push origin HEAD:<default>` — the incident shape
# (granite pushed HEAD:main from a detached HEAD). local_ref is HEAD, not a
# refs/heads/* ref, so the off-branch guard does not cover this form.
echo "Test 17: Detached HEAD, HEAD:<default> refspec, polecat — BLOCKED"
setup_repos
cd "$TMPDIR/local"
echo "detached work" >> file.txt
git add file.txt && git commit -m "detached work" >/dev/null 2>&1
git checkout --detach HEAD >/dev/null 2>&1
remote_sha=$(git rev-parse "origin/$DEFAULT_BRANCH")
local_sha=$(get_sha HEAD)
assert_block "HEAD:default blocked from detached HEAD (polecat)" run_hook_env "GT_ROLE=gastown/polecats/flint" "HEAD" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 18: detached HEAD, `<sha>:<default>` refspec, polecat — BLOCKED
echo "Test 18: Detached HEAD, <sha>:<default> refspec, polecat — BLOCKED"
setup_repos
cd "$TMPDIR/local"
echo "detached work" >> file.txt
git add file.txt && git commit -m "detached work" >/dev/null 2>&1
git checkout --detach HEAD >/dev/null 2>&1
remote_sha=$(git rev-parse "origin/$DEFAULT_BRANCH")
local_sha=$(get_sha HEAD)
assert_block "<sha>:default blocked from detached HEAD (polecat)" run_hook_env "GT_ROLE=polecat" "$local_sha" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 19: polecat pushing its own polecat/* branch — still allowed
echo "Test 19: Polecat push to its own polecat/* branch"
setup_repos
cd "$TMPDIR/local"
git checkout -b polecat/flint/gt-ibt8 >/dev/null 2>&1
echo "polecat work" >> file.txt
git add file.txt && git commit -m "polecat work" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Polecat branch push allowed" run_hook_env "GT_ROLE=gastown/polecats/flint" "refs/heads/polecat/flint/gt-ibt8" "$local_sha" "refs/heads/polecat/flint/gt-ibt8" "0000000000000000000000000000000000000000"
cleanup

# Test 20: Refinery merge path, genuine refinery identity (GT_REFINERY_MERGE=1
# + GT_ROLE=<rig>/refinery) — allowed
echo "Test 20: Default push with GT_REFINERY_MERGE=1 + genuine refinery identity"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "merged MR" >> file.txt
git add file.txt && git commit -m "merged MR" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Refinery merge allowed" run_hook_env "GT_ROLE=gastown/refinery GT_REFINERY_MERGE=1" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 21: gt done direct-merge convoy (GT_DONE_DIRECT_MERGE=1) — allowed
echo "Test 21: Default push with GT_DONE_DIRECT_MERGE=1 (gt done direct convoy)"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "direct merge" >> file.txt
git add file.txt && git commit -m "direct merge" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "gt done direct merge allowed" run_hook_env "GT_ROLE=gastown/polecats/flint GT_DONE_DIRECT_MERGE=1" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 22: cwd alone identifies the polecat worktree (no GT_ROLE at all)
echo "Test 22: Polecat detected from cwd (<town>/<rig>/polecats/<name>/<repo>)"
setup_repos "town/gastown/polecats/onyx/gastown"
cd "$TMPDIR/town/gastown/polecats/onyx/gastown"
remote_sha=$(get_sha HEAD)
echo "cwd-detected change" >> file.txt
git add file.txt && git commit -m "cwd-detected change" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_block "Polecat default push blocked by cwd" run_hook "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 23: crew role still pushes the default branch directly (by design)
echo "Test 23: Crew push to default branch (GT_ROLE=<rig>/crew/<name>)"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "crew change" >> file.txt
git add file.txt && git commit -m "crew change" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "Crew default push allowed" run_hook_env "GT_ROLE=gastown/crew/sloan" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# ---------------------------------------------------------------------------
# LIVE pushes: the same refusals through a real `git push` (gt-ibt8 asks for a
# live refused push, not a hand-fed stdin). Each case commits first, because
# git skips the hook entirely when there is nothing to send.
# ---------------------------------------------------------------------------

# Test 24: live `git push origin HEAD:<default>` from a polecat worktree — refused
echo "Test 24: LIVE push HEAD:$DEFAULT_BRANCH from a polecat worktree (cwd-detected)"
setup_repos "town/gastown/polecats/flint/gastown"
cd "$TMPDIR/town/gastown/polecats/flint/gastown"
echo "live work" >> file.txt
git add file.txt && git commit -m "live work" >/dev/null 2>&1
git checkout --detach HEAD >/dev/null 2>&1
assert_live_block "LIVE HEAD:default refused from polecat worktree" "" "HEAD:$DEFAULT_BRANCH"
cleanup

# Test 25: live `git push origin <sha>:<default>` with GT_ROLE=polecat — refused
echo "Test 25: LIVE push <sha>:$DEFAULT_BRANCH with GT_ROLE=polecat"
setup_repos
cd "$TMPDIR/local"
echo "live sha work" >> file.txt
git add file.txt && git commit -m "live sha work" >/dev/null 2>&1
git checkout --detach HEAD >/dev/null 2>&1
live_sha=$(get_sha HEAD)
assert_live_block "LIVE <sha>:default refused for polecat" "GT_ROLE=polecat" "$live_sha:$DEFAULT_BRANCH"
cleanup

# Test 26: live push of the polecat's own branch — allowed
echo "Test 26: LIVE push of polecat/* branch with GT_ROLE=polecat"
setup_repos
cd "$TMPDIR/local"
git checkout -b polecat/flint/gt-ibt8 >/dev/null 2>&1
echo "live branch work" >> file.txt
git add file.txt && git commit -m "live branch work" >/dev/null 2>&1
assert_live_pass "LIVE polecat branch push allowed" "GT_ROLE=gastown/polecats/flint" "polecat/flint/gt-ibt8"
cleanup

# Test 27: live push to the default branch as crew — allowed (control, proves
# the refusal above is the role check and not a broken test rig)
echo "Test 27: LIVE push HEAD:$DEFAULT_BRANCH as crew — allowed"
setup_repos
cd "$TMPDIR/local"
echo "crew live work" >> file.txt
git add file.txt && git commit -m "crew live work" >/dev/null 2>&1
assert_live_pass "LIVE crew default push allowed" "GT_ROLE=gastown/crew/sloan" "HEAD:$DEFAULT_BRANCH"
cleanup

# ---------------------------------------------------------------------------
# Allow-list identity binding (gt-9tf9): GT_REFINERY_MERGE /
# GT_DONE_DIRECT_MERGE are env vars any process in the shell can set, so
# is_polecat_main_push_allowed must not trust GT_REFINERY_MERGE on its own -
# it has to be corroborated by is_refinery_caller, and contradictory flag
# combinations must fail closed.
# ---------------------------------------------------------------------------

# Test 28: a polecat session (GT_ROLE=polecat) exports GT_REFINERY_MERGE=1
# itself, with no refinery identity signal at all — still BLOCKED. This is
# the exact bypass gt-9tf9 closes: the flag alone used to be enough.
echo "Test 28: Polecat self-granting GT_REFINERY_MERGE=1 (no refinery identity) — BLOCKED"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "spoofed merge" >> file.txt
git add file.txt && git commit -m "spoofed merge" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_block "Polecat GT_REFINERY_MERGE=1 without refinery identity blocked" run_hook_env "GT_ROLE=polecat GT_REFINERY_MERGE=1" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 29: same bypass attempt, but the polecat is detected by cwd alone (no
# GT_ROLE at all) — still BLOCKED.
echo "Test 29: Polecat (cwd-detected) self-granting GT_REFINERY_MERGE=1 — BLOCKED"
setup_repos "town/gastown/polecats/onyx/gastown"
cd "$TMPDIR/town/gastown/polecats/onyx/gastown"
remote_sha=$(get_sha HEAD)
echo "spoofed merge" >> file.txt
git add file.txt && git commit -m "spoofed merge" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_block "cwd-detected polecat GT_REFINERY_MERGE=1 blocked" run_hook_env "GT_REFINERY_MERGE=1" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 30: GT_REFINERY_MERGE=1 corroborated by GT_ROLE=<rig>/refinery even
# though cwd happens to look polecat-shaped (batch.go: "the Refinery may
# itself run inside one") — ALLOWED.
echo "Test 30: GT_REFINERY_MERGE=1 + GT_ROLE=refinery from a polecat-shaped cwd — ALLOWED"
setup_repos "town/gastown/polecats/onyx/gastown"
cd "$TMPDIR/town/gastown/polecats/onyx/gastown"
remote_sha=$(get_sha HEAD)
echo "real merge" >> file.txt
git add file.txt && git commit -m "real merge" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "GT_ROLE=refinery corroborates GT_REFINERY_MERGE=1" run_hook_env "GT_ROLE=gastown/refinery GT_REFINERY_MERGE=1" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 31: GT_REFINERY_MERGE=1 corroborated by GT_REFINERY=1 (the session-spawn
# signal, set once by the Refinery's own session manager) from a
# polecat-shaped cwd — ALLOWED.
echo "Test 31: GT_REFINERY_MERGE=1 + GT_REFINERY=1 from a polecat-shaped cwd — ALLOWED"
setup_repos "town/gastown/polecats/onyx/gastown"
cd "$TMPDIR/town/gastown/polecats/onyx/gastown"
remote_sha=$(get_sha HEAD)
echo "real merge" >> file.txt
git add file.txt && git commit -m "real merge" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_pass "GT_REFINERY=1 corroborates GT_REFINERY_MERGE=1" run_hook_env "GT_REFINERY=1 GT_REFINERY_MERGE=1" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 32: both allow flags set at once (contradictory — no gt code path does
# this) — BLOCKED from a polecat session, fail closed on ambiguity rather
# than picking one, even though GT_REFINERY_MERGE alone would need refinery
# identity and GT_DONE_DIRECT_MERGE alone would be enough on its own.
echo "Test 32: Both GT_REFINERY_MERGE=1 and GT_DONE_DIRECT_MERGE=1 set — BLOCKED (ambiguous)"
setup_repos
cd "$TMPDIR/local"
remote_sha=$(get_sha HEAD)
echo "ambiguous merge" >> file.txt
git add file.txt && git commit -m "ambiguous merge" >/dev/null 2>&1
local_sha=$(get_sha HEAD)
assert_block "Both allow flags set is refused" run_hook_env "GT_ROLE=gastown/polecats/flint GT_REFINERY_MERGE=1 GT_DONE_DIRECT_MERGE=1" "refs/heads/$DEFAULT_BRANCH" "$local_sha" "refs/heads/$DEFAULT_BRANCH" "$remote_sha"
cleanup

# Test 33: LIVE proof of Test 28 — a real `git push` from a polecat worktree
# with GT_REFINERY_MERGE=1 self-granted is still refused (gt-9tf9 asks
# specifically for this live proof, not just a matcher/stdin test).
echo "Test 33: LIVE push HEAD:$DEFAULT_BRANCH from a polecat worktree with GT_REFINERY_MERGE=1 — refused"
setup_repos "town/gastown/polecats/flint/gastown"
cd "$TMPDIR/town/gastown/polecats/flint/gastown"
echo "live spoofed merge" >> file.txt
git add file.txt && git commit -m "live spoofed merge" >/dev/null 2>&1
git checkout --detach HEAD >/dev/null 2>&1
assert_live_block "LIVE HEAD:default refused for polecat despite GT_REFINERY_MERGE=1" "GT_REFINERY_MERGE=1" "HEAD:$DEFAULT_BRANCH"
cleanup

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
if [[ $FAIL -gt 0 ]]; then
  exit 1
fi

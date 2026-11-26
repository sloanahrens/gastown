#!/usr/bin/env bash
# lint-lock-wait.sh WAIT_SECONDS CMD... runs CMD (golangci-lint) and, while it
# fails only because another golangci-lint holds the module lock, re-runs it
# until WAIT_SECONDS have passed (gt-uoppq). A concurrent lint in another
# worktree then delays `make lint` instead of turning it red.
#
# Retrying keeps the evidence that run.allow-serial-runners would hide: every
# contended attempt still prints golangci-lint's "parallel golangci-lint is
# running" line, so a lint that stays contended past the wait exits non-zero
# with that line in its output, and internal/lintlock still reads it as
# "nothing was linted" (gt-kqwu). WAIT_SECONDS=0 runs CMD once.
#
# LINT_LOCK_POLL is the pause between attempts (default 10s); golangci-lint
# already spins 5s on the lock inside each attempt.
#
# LINT_LOCK_CLOCK is a test seam: an executable path that prints whole seconds,
# used instead of the shell's SECONDS so a test can drive the clock without
# waiting (gt-0ygn63). Unset, SECONDS is read.
set -uo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 WAIT_SECONDS CMD..." >&2
  exit 2
fi
wait_s=$1
shift
poll=${LINT_LOCK_POLL:-10}
clock=${LINT_LOCK_CLOCK:-}
marker='parallel golangci-lint is running'

# A seam that cannot run would leave every elapsed time at 0, so a
# set-but-unusable LINT_LOCK_CLOCK is refused before the first attempt.
if [[ -n $clock && ! -x $clock ]]; then
  echo "lint: LINT_LOCK_CLOCK=$clock is not an executable that prints whole seconds" >&2
  exit 2
fi
now() {
  if [[ -n $clock ]]; then
    "$clock"
  else
    echo "$SECONDS"
  fi
}

out=$(mktemp)
trap 'rm -f "$out"' EXIT
start=$(now)
slept=0
while :; do
  "$@" 2>&1 | tee "$out"
  rc=${PIPESTATUS[0]}
  if [[ $rc -eq 0 ]] || ! grep -q -F -- "$marker" "$out"; then
    exit "$rc"
  fi
  # SECONDS is wall-clock whole seconds, so the host clock stepping backward
  # can make the elapsed time negative. The wait never reads less than the
  # pauses already slept, which is never negative, so a backward step counts as
  # 0 and cannot stretch the bound (gt-0ygn63). A zero bound runs the lint once.
  waited=$(( $(now) - start ))
  if (( waited < slept )); then waited=$slept; fi
  if (( waited + poll >= wait_s )); then
    echo "lint: another golangci-lint still held the module lock after ${waited}s (LINT_LOCK_WAIT=${wait_s}s); nothing was linted" >&2
    exit "$rc"
  fi
  echo "lint: another golangci-lint holds the module lock; retrying in ${poll}s (waited ${waited}s of ${wait_s}s)" >&2
  sleep "$poll"
  slept=$(( slept + poll ))
done

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
set -uo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 WAIT_SECONDS CMD..." >&2
  exit 2
fi
wait_s=$1
shift
poll=${LINT_LOCK_POLL:-10}
marker='parallel golangci-lint is running'

out=$(mktemp)
trap 'rm -f "$out"' EXIT
start=$SECONDS
while :; do
  "$@" 2>&1 | tee "$out"
  rc=${PIPESTATUS[0]}
  if [[ $rc -eq 0 ]] || ! grep -q -F -- "$marker" "$out"; then
    exit "$rc"
  fi
  waited=$((SECONDS - start))
  if (( waited + poll >= wait_s )); then
    echo "lint: another golangci-lint still held the module lock after ${waited}s (LINT_LOCK_WAIT=${wait_s}s); nothing was linted" >&2
    exit "$rc"
  fi
  echo "lint: another golangci-lint holds the module lock; retrying in ${poll}s (waited ${waited}s of ${wait_s}s)" >&2
  sleep "$poll"
done

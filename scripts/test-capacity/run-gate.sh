#!/usr/bin/env bash
# run-gate.sh <worktree> <outdir> <label> — one measured container suite.
# The Makefile's `test` go test line plus -json (keep passing packages'
# output) and -count=1 (no cache). Runs under `gt slot run` like any suite.
set -uo pipefail
if [[ "${1:-}" == "--inner" ]]; then
  log="$2"; start=$(date +%s)
  env GOFLAGS=-p=8 GT_TEST_DOCKER=1 go test -json -count=1 -timeout 20m ./... > "$log"
  rc=$?
  echo $(( $(date +%s) - start )) > "$log.wall"
  exit "$rc"
fi
wt="${1:?worktree}"; out="${2:?outdir}"; label="${3:?label}"
role="${GT_CAPACITY_ROLE:-gastown/crew/sloan-yfj}"
self="$(cd "$(dirname "$0")" && pwd)/run-gate.sh"
mkdir -p "$out"; log="$out/$label.json"
{ date '+start %Y-%m-%dT%H:%M:%S%z'; sysctl -n vm.loadavg; gt slot status 2>&1; git -C "$wt" rev-parse HEAD; } > "$out/$label.meta"
( cd "$wt" && exec gt slot run --role "$role" --nice 0 -- bash "$self" --inner "$log" ) 2> "$out/$label.stderr"
rc=$?
echo "$rc" > "$log.exit"
{ date '+end %Y-%m-%dT%H:%M:%S%z'; sysctl -n vm.loadavg; } >> "$out/$label.meta"
echo "run-gate $label: exit=$rc wall=$(cat "$log.wall" 2>/dev/null || echo '?')s log=$log"
exit "$rc"

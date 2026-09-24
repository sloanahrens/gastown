#!/usr/bin/env bash
# paired-run.sh [--check] <outdir> <label> <worktree-a> [<worktree-b>]
# One stage run: one worktree = single-suite run, two = paired run (both
# started within 5 s). --check only evaluates the preconditions.
# Exit: 0 all suites passed, 1 a suite failed, 3 preconditions refused.
set -uo pipefail
check=0; [[ "${1:-}" == "--check" ]] && { check=1; shift; }
out="${1:?outdir}"; label="${2:?label}"; wta="${3:?worktree-a}"; wtb="${4:-}"
here="$(cd "$(dirname "$0")" && pwd)"
role="${GT_CAPACITY_ROLE:-gastown/crew/sloan-yfj}"

refuse() { echo "paired-run: REFUSED: $*" >&2; exit 3; }
preconditions() {
  local st load
  st="$(gt slot status 2>&1)" || refuse "gt slot status failed: $st"
  # Wording from internal/cmd/slot.go: "held by <role> (pid ...)".
  grep -q 'held by gastown/refinery' <<<"$st" && refuse "refinery holds a slot"
  grep -q 'unwrapped container suite' <<<"$st" && refuse "unwrapped containers present"
  grep -q 'Docker daemon unreachable' <<<"$st" && refuse "Docker state unknown"
  load=$(sysctl -n vm.loadavg | awk '{print int($2)}')
  (( load < 20 )) || refuse "load1=$load >= 20"
  # Queue rows start with the MR id (gt-wisp-...); statuses read "ready" etc.
  local mq; mq="$(cd ~/gt/gastown/crew/sloan && gt mq list gastown 2>&1)" || refuse "gt mq list failed: $mq"
  grep -qE '^[[:space:]]+gt-' <<<"$mq" && refuse "gastown merge queue not empty"
  echo "paired-run: preconditions OK (load1=$load)"
}
preconditions
(( check )) && exit 0

mkdir -p "$out"
before="$(docker ps -q --filter label=org.testcontainers.sessionId | sort)"
pids=(); sampler=""; abnormal=0

# Containers whose testcontainers session did not exist at start. On normal
# completion (suites exited on their own) we remove nothing — tests clean up
# their own containers; we only report and record the session id. On
# abnormal exit (we received INT/TERM, or killed the suites ourselves) we
# remove them by exact container id, but only if no other role currently
# holds a slot — an active refinery gate or another suite must not lose its
# containers. Never match by image or name pattern.
sweep_containers() {
  local after new id sess new_ids=() new_sess=()
  after="$(docker ps -q --filter label=org.testcontainers.sessionId | sort)"
  new="$(comm -13 <(echo "$before") <(echo "$after"))"
  [[ -n "$new" ]] || return 0
  for id in $new; do
    sess="$(docker inspect -f '{{index .Config.Labels "org.testcontainers.sessionId"}}' "$id")"
    echo "$sess" >> "$out/$label.sessions"
    new_ids+=("$id"); new_sess+=("$sess")
  done
  if (( ! abnormal )); then
    local i
    for i in "${!new_ids[@]}"; do
      echo "paired-run: new session container ${new_ids[$i]} (session ${new_sess[$i]}), left running (normal completion)"
    done
    return 0
  fi
  local st other
  st="$(gt slot status 2>&1)"
  other="$(grep -o 'held by [^ ]*' <<<"$st" | grep -v -F "held by $role" || true)"
  if [[ -n "$other" ]]; then
    echo "paired-run: left for operator: another holder is active ($(tr '\n' ',' <<<"$other")) — not removing: ${new_ids[*]}"
    return 0
  fi
  local i
  for i in "${!new_ids[@]}"; do
    docker rm -f "${new_ids[$i]}" >/dev/null && echo "paired-run: removed leftover ${new_ids[$i]} (session ${new_sess[$i]})"
  done
}
cleanup() {
  [[ -n "$sampler" ]] && kill "$sampler" 2>/dev/null
  if (( abnormal )); then
    for p in "${pids[@]}"; do kill -TERM -- "-$p" 2>/dev/null; done
    sleep 2
  fi
  sweep_containers
  gt slot status 2>&1 | grep -A5 'unwrapped' || true
}
trap cleanup EXIT
trap 'abnormal=1; exit 130' INT TERM

"$here/sample-capacity.sh" "$out/$label" & sampler=$!
set -m   # each suite in its own process group, so cleanup can kill the group
"$here/run-gate.sh" "$wta" "$out" "$label-a" & pids+=($!)
if [[ -n "$wtb" ]]; then
  sleep 2
  "$here/run-gate.sh" "$wtb" "$out" "$label-b" & pids+=($!)
fi
set +m
rc=0; for p in "${pids[@]}"; do wait "$p" || rc=1; done
pids=()
for s in a b; do
  [[ -s "$out/$label-$s.json" ]] && "$here/dolt-gate-metrics.sh" "$out/$label-$s.json" "$label-$s" >> "$out/$label.md"
done
awk -F'\t' 'NR>1{ if($2>l)l=$2; u=$3-$4; if(u>m)m=u; s=$5-$6; if(s>w)w=s } END{ printf "| %s | peak load %.1f | peak VM used %.1f GiB | peak swap %.2f GiB |\n", lbl, l, m/1048576, w/1048576 }' lbl="$label" "$out/$label/capacity.tsv" >> "$out/$label.md"
cat "$out/$label.md"
exit "$rc"

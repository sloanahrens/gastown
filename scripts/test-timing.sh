#!/usr/bin/env bash
# test-timing.sh — time the unit tier where macOS still scans new executables.
#
# A tmux server that launchd starts is not covered by a Developer Tools
# exemption, which is exactly the town's condition after a reboot. We start
# one via `launchctl submit`, prove its pane is taxed with a 20-script probe,
# then run the unit tier in it and report wall time against TARGET_SECONDS.
set -euo pipefail
[[ "$(uname)" == Darwin ]] || { echo "test-timing: macOS only" >&2; exit 2; }
pkgs=("${@:-./...}")
target=${TARGET_SECONDS:-90}
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
sock="gt-test-timing-$$"
label="com.gastown.test-timing.$$"
tmux_bin=$(command -v tmux)

cleanup() {
  "$tmux_bin" -L "$sock" kill-server 2>/dev/null || true
  launchctl remove "$label" 2>/dev/null || true
}
trap cleanup EXIT

# launchd owns the new-session process; KeepAlive would respawn it, so remove
# the job as soon as the server is up (the server itself has daemonized).
launchctl submit -l "$label" -- "$tmux_bin" -L "$sock" new-session -d -s timing
for _ in $(seq 50); do
  "$tmux_bin" -L "$sock" has-session -t timing 2>/dev/null && break
  sleep 0.2
done
launchctl remove "$label" 2>/dev/null || true
"$tmux_bin" -L "$sock" has-session -t timing

cat > "$work/run.sh" <<EOF
set -u
cd '$repo'
d=\$(mktemp -d)
for i in \$(seq 20); do printf '#!/bin/sh\n:\n' > "\$d/s\$i"; chmod +x "\$d/s\$i"; done
s=\$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
for i in \$(seq 20); do "\$d/s\$i"; done
e=\$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
perl -e "printf \"%d\n\", (\$e-\$s)*1000/20" > '$work/probe'
start=\$(date +%s)
GT_TEST_DOCKER=0 GOFLAGS=-p=8 go run ./internal/testpolicy/cmd/budget -- -count=1 ${pkgs[*]} > '$work/log' 2>&1
echo "\$? \$(( \$(date +%s) - start ))" > '$work/done'
EOF
"$tmux_bin" -L "$sock" send-keys -t timing "bash '$work/run.sh'" Enter

for _ in $(seq 900); do [[ -s "$work/done" ]] && break; sleep 2; done
[[ -s "$work/done" ]] || { echo "test-timing: no result after 30 min; log: $work/log" >&2; exit 2; }

probe=$(cat "$work/probe")
read -r rc secs < "$work/done"
echo "probe: ${probe} ms per new executable (taxed pane expected >= 50)"
echo "unit tier: ${secs}s (target ${target}s), go test exit ${rc}; log: $work/log"
if (( probe < 50 )); then
  echo "test-timing: pane is EXEMPT (probe ${probe} ms); this measurement does not prove the target" >&2
  exit 3
fi
(( rc == 0 )) || exit "$rc"
(( secs <= target )) || { echo "test-timing: over target" >&2; exit 1; }

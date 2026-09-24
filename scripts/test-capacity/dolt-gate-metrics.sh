#!/usr/bin/env bash
# dolt-gate-metrics.sh <go-test-json-log> [label] — per-package Dolt-contention
# metrics from one `go test -json` gate log (gt-elvf4), as markdown table rows.
# grep -a everywhere: a gate log can be classified binary, and plain grep then
# reports "no match" silently. Reads <log>.exit and <log>.wall when present.
set -uo pipefail
log="${1:?usage: dolt-gate-metrics.sh <log.json> [label]}"
label="${2:-$(basename "$log" .json)}"
mod="github.com/steveyegge/gastown"
pkgs=(internal/beads internal/cmd internal/convoy internal/daemon internal/doltserver
      internal/mail internal/polecat internal/refinery internal/testutil)
markers=(
  'bd call against the test Dolt container failed on attempt'  # gastown retry notice
  'refusing to auto-apply'                                     # bd remote-migrate gate
  'could not resolve initial root'                             # Dolt catalog snapshot race
  'timed out after'                                            # bd subprocess budget kill
  'Dolt container setup failed'                                # testutil container start
  'Warning: applying [0-9]* pending schema migration'          # bd resumed under the env var
)
[[ -s "$log" ]] || { echo "dolt-gate-metrics: empty or missing log: $log" >&2; exit 2; }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

counts() { # <file>: the marker counts, " | "-joined
  local f="$1" pat out=""
  for pat in "${markers[@]}"; do
    out+=" | $(grep -a -c -e "$pat" "$f" || true)"
  done
  printf '%s' "${out# | }"
}

echo '| run | package | result | elapsed s | failed tests | retry notices | refusing | init root | bd timeouts | setup failed | resumed migrations |'
echo '|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|'
for p in "${pkgs[@]}"; do
  pf="$tmp/pkg"
  grep -a -F "\"Package\":\"$mod/$p\"" "$log" > "$pf" || true
  if [[ ! -s "$pf" ]]; then
    echo "| $label | $p | absent | - | - | - | - | - | - | - | - |"
    continue
  fi
  final="$(grep -a -E '"Action":"(pass|fail|skip)"' "$pf" | grep -a -v '"Test":' | tail -n 1 || true)"
  result="$(sed -n 's/.*"Action":"\([a-z]*\)".*/\1/p' <<<"$final")"
  elapsed="$(sed -n 's/.*"Elapsed":\([0-9.]*\).*/\1/p' <<<"$final")"
  grep -a -q -F '(cached)' "$pf" && result="cached"
  failed="$(grep -a -F '"Action":"fail"' "$pf" | grep -a -c -F '"Test":' || true)"
  echo "| $label | $p | ${result:-none} | ${elapsed:--} | $failed | $(counts "$pf") |"
done
exit_code="$(cat "$log.exit" 2>/dev/null || echo '?')"
wall="$(cat "$log.wall" 2>/dev/null || echo '?')"
failed="$(grep -a -F '"Action":"fail"' "$log" | grep -a -c -F '"Test":' || true)"
echo "| $label | ALL (exit $exit_code) | wall | $wall | $failed | $(counts "$log") |"

//go:build integration

package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Wiring guards whose production collaborator is a real process: each leaves
// one seam nil and proves the collaborator behind it ran. The unit tier's
// guards that need no process are in wiring_guard_test.go.

// TestIntegrationHostLoadMeasuresTheRealHost guards hostLoad's nil path: the reading is
// the host's own (its CPU count), not a zero value.
func TestIntegrationHostLoadMeasuresTheRealHost(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	if got := d.hostLoad().NumCPU; got != runtime.NumCPU() {
		t.Errorf("hostLoad().NumCPU = %d, want this host's %d: the nil seam must measure the real host", got, runtime.NumCPU())
	}
}

func writeFakeTmux(t *testing.T, dir string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail

cmd=""
skip_next=0
for arg in "$@"; do
  if [[ "$skip_next" -eq 1 ]]; then
    skip_next=0
    continue
  fi
  if [[ "$arg" == "-u" ]]; then
    continue
  fi
  if [[ "$arg" == "-L" ]]; then
    skip_next=1
    continue
  fi
  cmd="$arg"
  break
done

if [[ -n "${TMUX_LOG:-}" ]]; then
  printf "%s %s\n" "$cmd" "$*" >> "$TMUX_LOG"
fi

if [[ "${1:-}" == "-V" ]]; then
  echo "tmux 3.3a"
  exit 0
fi

# Keep session checks simple for this regression repro: no existing boot session.
# TMUX_FAKE_SESSION=alive reports the queried session as live instead, and
# TMUX_FAKE_SESSION_CREATED supplies the creation time the turn-budget guard reads.
if [[ "$cmd" == "has-session" ]]; then
  if [[ "${TMUX_FAKE_SESSION:-}" == "alive" ]]; then
    exit 0
  fi
  exit 1
fi

if [[ "$cmd" == "list-sessions" ]]; then
  if [[ -n "${TMUX_FAKE_SESSION_CREATED:-}" ]]; then
    printf "%s\n" "$TMUX_FAKE_SESSION_CREATED"
  fi
  exit 0
fi

exit 0
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
}

package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSubprocessTimeoutForBudget pins the per-command budgets. Init mints a
// database and installs integrations, so it needs more than the steady-state
// 60s; an explicit GT_BD_TIMEOUT_SEC still wins over both (gt-824d).
func TestSubprocessTimeoutForBudget(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		isolated   bool
		serverPort int
		envVal     string
		envSet     bool
		want       time.Duration
	}{
		{name: "init takes the init budget", args: []string{"init", "--prefix", "gt"}, want: bdInitSubprocessTimeout},
		{name: "list takes the default budget", args: []string{"list", "--json"}, want: bdSubprocessTimeout},
		{name: "override shortens init", args: []string{"init"}, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "override shortens list", args: []string{"list"}, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "invalid override leaves init on the init budget", args: []string{"init"}, envSet: true, envVal: "abc", want: bdInitSubprocessTimeout},
		{name: "create against the test container takes the container budget", args: []string{"create", "--title", "x"}, isolated: true, serverPort: 55491, want: bdContainerSubprocessTimeout},
		{name: "init against the test container keeps the init budget", args: []string{"init", "--prefix", "gt"}, isolated: true, serverPort: 55491, want: bdInitSubprocessTimeout},
		{name: "list against the test container takes the container budget", args: []string{"list", "--json"}, isolated: true, serverPort: 55491, want: bdContainerSubprocessTimeout},
		{name: "override shortens a container call", args: []string{"create", "--title", "x"}, isolated: true, serverPort: 55491, envSet: true, envVal: "2", want: 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv(bdTimeoutEnvVar, tt.envVal)
			} else {
				_ = os.Unsetenv(bdTimeoutEnvVar)
			}
			b := &Beads{isolated: tt.isolated, serverPort: tt.serverPort}
			if got := b.subprocessTimeoutForClient(tt.args); got != tt.want {
				t.Errorf("subprocessTimeoutForClient(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestInitDeadlineIsReportedAsTimeout reproduces gt-824d. bd writes a warning
// to stderr and is then killed at its deadline; the error must name the
// timeout instead of surfacing only the warning, which is what made the
// original failure read as a redirect bug rather than an exhausted budget.
func TestInitDeadlineIsReportedAsTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	t.Setenv(bdTimeoutEnvVar, "1")
	installBlockingBd(t)

	err := NewIsolated(t.TempDir()).Init("gt")
	if err == nil {
		t.Fatal("Init should fail when bd blocks past its deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should wrap context.DeadlineExceeded so callers can detect it, got %v", err)
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error should name the timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "ignoring redirect") {
		t.Errorf("error should keep bd's stderr, got %v", err)
	}
	if !strings.Contains(err.Error(), "after 1s") {
		t.Errorf("error should name the budget that expired, got %v", err)
	}
}

// installBlockingBd puts a fake bd on PATH that answers the --allow-stale
// capability probe immediately and, for every other invocation, writes the
// redirect warning bd emits when a redirect target has no database yet before
// blocking past the subprocess deadline. `exec sleep` replaces the shell so a
// deadline kill reaps the blocking process itself rather than orphaning a
// child.
func installBlockingBd(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    version) echo "bd test-version"; exit 0 ;;
  esac
done
echo "Warning: ignoring redirect from $PWD/.beads to $PWD/mayor/rig/.beads because the target has no database or metadata.json; fix or delete the redirect file" >&2
exec sleep 30
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

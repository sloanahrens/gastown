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
// 60s (gt-824d); a non-init call against the test Dolt container gets 3m,
// because the shared container stalls calls under gate load (gt-elvf4); an
// explicit GT_BD_TIMEOUT_SEC still wins over all of them.
func TestSubprocessTimeoutForBudget(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		container bool
		envVal    string
		envSet    bool
		want      time.Duration
	}{
		{name: "init takes the init budget", args: []string{"init", "--prefix", "gt"}, want: bdInitSubprocessTimeout},
		{name: "list takes the default budget", args: []string{"list", "--json"}, want: bdSubprocessTimeout},
		{name: "override shortens init", args: []string{"init"}, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "override shortens list", args: []string{"list"}, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "invalid override leaves init on the init budget", args: []string{"init"}, envSet: true, envVal: "abc", want: bdInitSubprocessTimeout},
		{name: "container init keeps the init budget", args: []string{"init", "--database", "testdb_0123456789abcdef"}, container: true, want: bdInitSubprocessTimeout},
		{name: "container list takes the container budget", args: []string{"list", "--json"}, container: true, want: bdContainerSubprocessTimeout},
		{name: "container create takes the container budget", args: []string{"create", "--title", "x"}, container: true, want: bdContainerSubprocessTimeout},
		{name: "override shortens a container call", args: []string{"show", "gt-1"}, container: true, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "invalid override leaves a container call on the container budget", args: []string{"show"}, container: true, envSet: true, envVal: "abc", want: bdContainerSubprocessTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv(bdTimeoutEnvVar, tt.envVal)
			} else {
				_ = os.Unsetenv(bdTimeoutEnvVar)
			}
			if got := subprocessTimeoutFor(tt.args, tt.container); got != tt.want {
				t.Errorf("subprocessTimeoutFor(%q, container=%v) = %v, want %v", tt.args, tt.container, got, tt.want)
			}
		})
	}
}

// TestSubprocessBudgetValues pins the three budgets the spec fixes.
func TestSubprocessBudgetValues(t *testing.T) {
	if bdSubprocessTimeout != 60*time.Second {
		t.Errorf("bdSubprocessTimeout = %v, want 60s", bdSubprocessTimeout)
	}
	if bdContainerSubprocessTimeout != 3*time.Minute {
		t.Errorf("bdContainerSubprocessTimeout = %v, want 3m", bdContainerSubprocessTimeout)
	}
	if bdInitSubprocessTimeout != 5*time.Minute {
		t.Errorf("bdInitSubprocessTimeout = %v, want 5m", bdInitSubprocessTimeout)
	}
}

// TestBeadsSubprocessTimeoutScope pins who gets the container budget: only a
// wrapper aimed at the test Dolt container. Real-town wrappers keep 60s.
func TestBeadsSubprocessTimeoutScope(t *testing.T) {
	t.Setenv(bdTimeoutEnvVar, "")
	dir := t.TempDir()
	tests := []struct {
		name     string
		b        *Beads
		wantList time.Duration
	}{
		{name: "test container", b: NewIsolatedWithPort(dir, 45678), wantList: bdContainerSubprocessTimeout},
		{name: "isolated without a port", b: NewIsolated(dir), wantList: bdSubprocessTimeout},
		{name: "real town", b: New(dir), wantList: bdSubprocessTimeout},
		{name: "real town with beads dir", b: NewWithBeadsDir(dir, filepath.Join(dir, ".beads")), wantList: bdSubprocessTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.subprocessTimeout([]string{"list", "--json"}); got != tt.wantList {
				t.Errorf("list budget = %v, want %v", got, tt.wantList)
			}
			if got := tt.b.subprocessTimeout([]string{"init", "--prefix", "gt"}); got != bdInitSubprocessTimeout {
				t.Errorf("init budget = %v, want %v", got, bdInitSubprocessTimeout)
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

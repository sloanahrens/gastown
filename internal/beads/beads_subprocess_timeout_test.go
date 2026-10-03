package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// TestSubprocessTimeoutForBudget pins the per-command budgets. Init mints a
// database and installs integrations, so it needs more than the steady-state
// 60s (gt-824d); a non-init call against the test Dolt container gets 3m,
// because the shared container stalls calls under gate load (gt-elvf4); an
// explicit operational.session.bd_subprocess_timeout wins over all of them.
func TestSubprocessTimeoutForBudget(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		container bool
		session   *config.SessionThresholds
		want      time.Duration
	}{
		{name: "init takes the init budget", args: []string{"init", "--prefix", "gt"}, want: bdInitSubprocessTimeout},
		{name: "list takes the default budget", args: []string{"list", "--json"}, want: bdSubprocessTimeout},
		{name: "override shortens init", args: []string{"init"}, session: sessionWith("2s"), want: 2 * time.Second},
		{name: "override shortens list", args: []string{"list"}, session: sessionWith("2s"), want: 2 * time.Second},
		{name: "invalid override leaves init on the init budget", args: []string{"init"}, session: sessionWith("abc"), want: bdInitSubprocessTimeout},
		{name: "zero override leaves init on the init budget", args: []string{"init"}, session: sessionWith("0s"), want: bdInitSubprocessTimeout},
		{name: "negative override leaves list on the default budget", args: []string{"list"}, session: sessionWith("-1s"), want: bdSubprocessTimeout},
		{name: "container init keeps the init budget", args: []string{"init", "--database", "testdb_0123456789abcdef"}, container: true, want: bdInitSubprocessTimeout},
		{name: "container list takes the container budget", args: []string{"list", "--json"}, container: true, want: bdContainerSubprocessTimeout},
		{name: "container create takes the container budget", args: []string{"create", "--title", "x"}, container: true, want: bdContainerSubprocessTimeout},
		{name: "override shortens a container call", args: []string{"show", "gt-1"}, container: true, session: sessionWith("2s"), want: 2 * time.Second},
		{name: "invalid override leaves a container call on the container budget", args: []string{"show"}, container: true, session: sessionWith("abc"), want: bdContainerSubprocessTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subprocessTimeoutFor(tt.args, tt.session, tt.container); got != tt.want {
				t.Errorf("subprocessTimeoutFor(%q, container=%v) = %v, want %v", tt.args, tt.container, got, tt.want)
			}
		})
	}
}

// sessionWith is a session settings stub carrying only bd_subprocess_timeout.
func sessionWith(bdSubprocessTimeout string) *config.SessionThresholds {
	return &config.SessionThresholds{BdSubprocessTimeout: bdSubprocessTimeout}
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

// TestBeadsSubprocessTimeoutFromTownSettings pins the move off GT_BD_TIMEOUT_SEC
// (gt-y3pgh.2.6): a client in a town whose settings/config.json sets
// operational.session.bd_subprocess_timeout takes that budget for every
// command, and a client in a town without the field keeps the compiled-in
// 60s/5m budgets. The two fixture towns differ only in that key, so a change
// that dropped the read or leaked it across towns fails here.
func TestBeadsSubprocessTimeoutFromTownSettings(t *testing.T) {
	t.Parallel()
	withField := writeFixtureTown(t, `{"type":"town-settings","version":1,"operational":{"session":{"bd_subprocess_timeout":"7s"}}}`)
	withoutField := writeFixtureTown(t, `{"type":"town-settings","version":1,"operational":{"session":{"startup_nudge_max_retries":4}}}`)

	tests := []struct {
		name    string
		workDir string
		args    []string
		want    time.Duration
	}{
		{name: "configured budget governs a list", workDir: withField, args: []string{"list", "--json"}, want: 7 * time.Second},
		{name: "configured budget governs init", workDir: withField, args: []string{"init", "--prefix", "gt"}, want: 7 * time.Second},
		{name: "absent field keeps the steady-state budget", workDir: withoutField, args: []string{"list", "--json"}, want: bdSubprocessTimeout},
		{name: "absent field keeps the init budget", workDir: withoutField, args: []string{"init", "--prefix", "gt"}, want: bdInitSubprocessTimeout},
		{name: "no town at all keeps the steady-state budget", workDir: t.TempDir(), args: []string{"list", "--json"}, want: bdSubprocessTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewIsolated(tt.workDir)
			if got := b.subprocessTimeout(tt.args); got != tt.want {
				t.Errorf("subprocessTimeout(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestCommandTimeoutForTownSettings pins the budget for a caller that runs bd
// through its own exec wiring (gt tail's journal probe): the town's
// bd_command_timeout, or the compiled-in 30s outside a town.
func TestCommandTimeoutForTownSettings(t *testing.T) {
	t.Parallel()
	town := writeFixtureTown(t, `{"type":"town-settings","version":1,"operational":{"session":{"bd_command_timeout":"45s"}}}`)
	blanket := writeFixtureTown(t, `{"type":"town-settings","version":1,"operational":{"session":{"bd_command_timeout":"45s","bd_subprocess_timeout":"12s"}}}`)

	if got := CommandTimeoutFor(town); got != 45*time.Second {
		t.Errorf("CommandTimeoutFor(town) = %v, want 45s", got)
	}
	if got := CommandTimeoutFor(blanket); got != 12*time.Second {
		t.Errorf("CommandTimeoutFor(blanket override) = %v, want the 12s bd_subprocess_timeout", got)
	}
	if got := CommandTimeoutFor(t.TempDir()); got != constants.BdCommandTimeout {
		t.Errorf("CommandTimeoutFor(no town) = %v, want %v", got, constants.BdCommandTimeout)
	}
}

// writeFixtureTown creates the town root FindTownRoot requires (mayor/town.json)
// plus settings/config.json when settings is non-empty, and returns its path.
func writeFixtureTown(t *testing.T, settings string) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{"name":"fixture"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if settings == "" {
		return town
	}
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// TestInitDeadlineIsReportedAsTimeout reproduces gt-824d. bd writes a warning
// to stderr and is then killed at its deadline; the error must name the
// timeout instead of surfacing only the warning, which is what made the
// original failure read as a redirect bug rather than an exhausted budget.
//
// bd is a runner that has written its warning and blocks until the deadline
// kills it. A real stub process with a 1s budget was flaky under host load
// (gt-cx3rt): a freshly written script could take longer than the budget to
// start, so the kill came before the warning and the error had no stderr.
func TestInitDeadlineIsReportedAsTimeout(t *testing.T) {
	t.Parallel()
	const warning = "Warning: ignoring redirect from .beads to mayor/rig/.beads because the target has no database or metadata.json; fix or delete the redirect file"
	r := newRecorder(nil)
	r.allowStale = true
	blocked := func(ctx context.Context, c bdCall) ([]byte, []byte, error) {
		if len(c.args) == 2 && c.args[1] == "version" {
			return r.exec(ctx, c)
		}
		<-ctx.Done()
		return nil, []byte(warning + "\n"), errors.New("signal: killed")
	}
	b := newBeads(beadsFields{workDir: t.TempDir(), isolated: true, exec: blocked, budget: 10 * time.Millisecond})

	err := b.Init("gt")
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
	if !strings.Contains(err.Error(), "after 10ms") {
		t.Errorf("error should name the budget that expired, got %v", err)
	}
}

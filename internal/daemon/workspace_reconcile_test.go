package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/config"
)

// fakeWorkspacePusher stands in for the gateway. Each push sends its call number
// on calls, so a test waits for the push it means before it moves the clock; the
// reconcile loop is the only caller, so n needs no lock.
type fakeWorkspacePusher struct {
	calls chan int
	n     int
	fail  func(call int) error
}

func (f *fakeWorkspacePusher) Push(_ context.Context, _ string) (string, error) {
	f.n++
	f.calls <- f.n
	if f.fail != nil {
		if err := f.fail(f.n); err != nil {
			return "", err
		}
	}
	return "sha", nil
}

// newWorkspaceDaemon builds a daemon whose environment, clock, pusher and log are
// the test's. Its patrol config is nil, so the reconcile takes the default
// interval unless a test sets one.
func newWorkspaceDaemon(t *testing.T, env map[string]string, clk clockwork.Clock, p bdgateway.WorkspacePusher, logs *lockedBuffer, patrols *config.DaemonPatrolConfig) *Daemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Daemon{
		config:          &Config{TownRoot: "/town"},
		logger:          log.New(logs, "", 0),
		ctx:             ctx,
		clock:           clk,
		getenv:          func(key string) string { return env[key] },
		patrolConfig:    patrols,
		workspacePusher: p,
	}
}

// TestWorkspaceReconcile: with a gateway named, the daemon pushes once at start,
// pushes again at each five-minute tick of its clock, and a push that failed is
// retried at the next tick.
func TestWorkspaceReconcile(t *testing.T) {
	t.Parallel()
	clk := newFixedClock()
	pusher := &fakeWorkspacePusher{
		calls: make(chan int, 10),
		fail: func(call int) error {
			if call == 2 {
				return errors.New("gateway unreachable")
			}
			return nil
		},
	}
	logs := &lockedBuffer{}
	env := map[string]string{bdgateway.WorkspaceURLEnv: "http://localhost:7440"}
	d := newWorkspaceDaemon(t, env, clk, pusher, logs, nil)

	d.startWorkspaceReconcile()
	if got := <-pusher.calls; got != 1 {
		t.Fatalf("start push = call %d, want 1", got)
	}

	clk.Advance(config.DefaultWorkspaceReconcileInterval)
	if got := <-pusher.calls; got != 2 {
		t.Fatalf("first tick push = call %d, want 2", got)
	}

	// The failure is logged after Push returns, so the log is read only once
	// the next push has been received: that push starts after the log line.
	clk.Advance(config.DefaultWorkspaceReconcileInterval)
	if got := <-pusher.calls; got != 3 {
		t.Fatalf("second tick push = call %d, want 3 (the retry)", got)
	}
	if n := strings.Count(logs.String(), "was not updated"); n != 1 {
		t.Errorf("failed pushes logged = %d, want 1 (only call 2 failed): %q", n, logs.String())
	}
	if !strings.Contains(logs.String(), "retrying at the next tick") {
		t.Errorf("the failed push was not logged as retried: %q", logs.String())
	}
}

// TestWorkspaceReconcileOff: with no gateway named, the reconcile neither pushes
// nor logs, so the daemon's start is the one it was before.
func TestWorkspaceReconcileOff(t *testing.T) {
	t.Parallel()
	clk := newFixedClock()
	pusher := &fakeWorkspacePusher{calls: make(chan int, 10)}
	logs := &lockedBuffer{}
	d := newWorkspaceDaemon(t, map[string]string{}, clk, pusher, logs, nil)

	d.startWorkspaceReconcile()
	clk.Advance(3 * config.DefaultWorkspaceReconcileInterval)

	if pusher.n != 0 {
		t.Errorf("pushes with no gateway = %d, want 0", pusher.n)
	}
	if logs.String() != "" {
		t.Errorf("log with no gateway = %q, want none", logs.String())
	}
}

// TestWorkspaceReconcileBadInterval: an interval the config refuses stops the
// reconcile before any push, and the log names the setting.
func TestWorkspaceReconcileBadInterval(t *testing.T) {
	t.Parallel()
	patrols := &config.DaemonPatrolConfig{Patrols: &config.PatrolsConfig{
		WorkspaceReconcile: &config.WorkspaceReconcileConfig{IntervalStr: "soon"},
	}}
	clk := newFixedClock()
	pusher := &fakeWorkspacePusher{calls: make(chan int, 10)}
	logs := &lockedBuffer{}
	env := map[string]string{bdgateway.WorkspaceURLEnv: "http://localhost:7440"}
	d := newWorkspaceDaemon(t, env, clk, pusher, logs, patrols)

	d.startWorkspaceReconcile()

	if pusher.n != 0 {
		t.Errorf("pushes with a refused interval = %d, want 0", pusher.n)
	}
	if !strings.Contains(logs.String(), "Workspace reconcile not started") ||
		!strings.Contains(logs.String(), "patrols.workspace_reconcile.interval") {
		t.Errorf("log does not name the refused setting: %q", logs.String())
	}
}

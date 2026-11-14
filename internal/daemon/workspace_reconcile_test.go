package daemon

import (
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/config"
)

// workspaceHangGuard bounds each wait for a push in the reconcile tests. It is a
// hang guard only: the wait ends on the push it means, and a push that never
// comes fails here rather than hanging the test.
const workspaceHangGuard = 10 * time.Second

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

// awaitPush returns the number of the next push the fake gateway receives, or
// fails the test if none comes within the hang guard.
func awaitPush(t *testing.T, calls <-chan int) int {
	t.Helper()
	select {
	case n := <-calls:
		return n
	case <-time.After(workspaceHangGuard): //testpolicy:allow no-sleep — a hang guard only: the push is the event the test waits for, and a reconcile that never pushes fails here rather than hanging the test
		t.Fatalf("no push within %s", workspaceHangGuard)
		return 0
	}
}

// heartbeatStepNames is the daemon's heartbeat patrol list, in order.
func heartbeatStepNames() []string {
	names := make([]string, 0, len(heartbeatSteps))
	for _, s := range heartbeatSteps {
		names = append(names, s.name)
	}
	return names
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
	if got := awaitPush(t, pusher.calls); got != 1 {
		t.Fatalf("start push = call %d, want 1", got)
	}

	clk.Advance(config.DefaultWorkspaceReconcileInterval)
	if got := awaitPush(t, pusher.calls); got != 2 {
		t.Fatalf("first tick push = call %d, want 2", got)
	}

	// The failure is logged after Push returns, so the log is read only once
	// the next push has been received: that push starts after the log line.
	clk.Advance(config.DefaultWorkspaceReconcileInterval)
	if got := awaitPush(t, pusher.calls); got != 3 {
		t.Fatalf("second tick push = call %d, want 3 (the retry)", got)
	}
	if n := strings.Count(logs.String(), "was not updated"); n != 1 {
		t.Errorf("failed pushes logged = %d, want 1 (only call 2 failed): %q", n, logs.String())
	}
	if !strings.Contains(logs.String(), "retrying at the next tick") {
		t.Errorf("the failed push was not logged as retried: %q", logs.String())
	}
}

// TestWorkspaceReconcileOff: with no gateway named, the reconcile installs no
// pusher, so nothing can push, logs nothing, and the daemon's heartbeat patrol
// list is the one it was before the start. A reconcile that did start would push
// on its own goroutine, so the test reads the install, which runs in the start
// call itself, rather than a push that may not have happened yet.
func TestWorkspaceReconcileOff(t *testing.T) {
	t.Parallel()
	clk := newFixedClock()
	logs := &lockedBuffer{}
	d := newWorkspaceDaemon(t, map[string]string{}, clk, nil, logs, nil)
	installs := 0
	d.installWorkspacePusher = func(bdgateway.WorkspacePusher) { installs++ }
	patrolsBefore := heartbeatStepNames()

	d.startWorkspaceReconcile()
	clk.Advance(3 * config.DefaultWorkspaceReconcileInterval)

	if installs != 0 {
		t.Errorf("pusher installs with no gateway = %d, want 0", installs)
	}
	if logs.String() != "" {
		t.Errorf("log with no gateway = %q, want none", logs.String())
	}
	if got := heartbeatStepNames(); !slices.Equal(got, patrolsBefore) {
		t.Errorf("heartbeat patrol list changed with no gateway: got %v, want %v", got, patrolsBefore)
	}
}

// TestWorkspaceReconcileInstallsGatewayPusher: with a gateway and no injected
// pusher, the daemon installs one GatewayPusher for the gateway's URL and the
// token file the environment names, once, at start.
func TestWorkspaceReconcileInstallsGatewayPusher(t *testing.T) {
	t.Parallel()
	const url = "http://localhost:7440"
	const tokenFile = "/run/secrets/bd-gateway-token"
	env := map[string]string{
		bdgateway.WorkspaceURLEnv:   url,
		bdgateway.WorkspaceTokenEnv: tokenFile,
	}
	var installed []bdgateway.WorkspacePusher
	d := newWorkspaceDaemon(t, env, newFixedClock(), nil, &lockedBuffer{}, nil)
	d.installWorkspacePusher = func(p bdgateway.WorkspacePusher) { installed = append(installed, p) }

	d.startWorkspaceReconcile()

	if len(installed) != 1 {
		t.Fatalf("installs = %d, want 1", len(installed))
	}
	gp, ok := installed[0].(bdgateway.GatewayPusher)
	if !ok {
		t.Fatalf("installed pusher = %T, want bdgateway.GatewayPusher", installed[0])
	}
	if gp.URL != url || gp.TokenFile != tokenFile {
		t.Errorf("installed pusher = {URL %q, TokenFile %q}, want {URL %q, TokenFile %q}", gp.URL, gp.TokenFile, url, tokenFile)
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

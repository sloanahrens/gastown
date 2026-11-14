package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/config"
)

// workspaceHangGuard bounds each wait for a push or a timer in the reconcile
// tests. It is a hang guard only: each wait ends on the event it means, and an
// event that never comes fails the test rather than hanging it.
const workspaceHangGuard = 10 * time.Second

// fakeWorkspacePusher stands in for the gateway. The reconcile loop is the only
// caller, so n needs no lock. onCall runs before each call is sent, so a test can
// retune the daemon between two pushes; the send then tells the test the push
// happened, before it moves the clock.
type fakeWorkspacePusher struct {
	calls  chan int
	n      int
	onCall func(call int)
	fail   func(call int) error
}

func (f *fakeWorkspacePusher) Push(_ context.Context, _ string) (string, error) {
	f.n++
	if f.onCall != nil {
		f.onCall(f.n)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	select {
	case n := <-calls:
		return n
	case <-ctx.Done():
		t.Fatalf("no push within %s", workspaceHangGuard)
		return 0
	}
}

// advanceToTick waits until the reconcile is waiting on the clock, then moves the
// clock by d, so that wait ends at its deadline.
func advanceToTick(t *testing.T, clk *clockwork.FakeClock, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("the reconcile is not waiting on the clock: %v", err)
	}
	clk.Advance(d)
}

// gatewayEnv is the environment of a daemon with a bd gateway named.
func gatewayEnv() map[string]string {
	return map[string]string{bdgateway.WorkspaceURLEnv: "http://localhost:7440"}
}

// withPatrols returns a daemon patrol config whose reconcile interval is s.
func withPatrols(s string) *config.DaemonPatrolConfig {
	return &config.DaemonPatrolConfig{Patrols: &config.PatrolsConfig{
		WorkspaceReconcile: &config.WorkspaceReconcileConfig{IntervalStr: s},
	}}
}

// setPatrolConfig swaps the patrol config a daemon reads, as a reload does.
func setPatrolConfig(d *Daemon, cfg *config.DaemonPatrolConfig) {
	d.patrolMu.Lock()
	defer d.patrolMu.Unlock()
	d.patrolConfig = cfg
}

// newWorkspaceDaemon builds a daemon whose environment, clock, pusher and log are
// the test's. Its patrol config is the one given, so nil takes the default
// interval.
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

// TestWorkspaceReconcile: with a gateway named, the daemon pushes at start and at
// each tick of its clock. A push that failed is logged and retried at the next
// tick.
func TestWorkspaceReconcile(t *testing.T) {
	t.Parallel()
	failSecond := func(call int) error {
		if call == 2 {
			return errors.New("gateway unreachable")
		}
		return nil
	}
	tests := []struct {
		name       string
		fail       func(call int) error
		wantFailed int
	}{
		{name: "every push succeeds", fail: nil, wantFailed: 0},
		{name: "a failed push is retried at the next tick", fail: failSecond, wantFailed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := newFixedClock()
			pusher := &fakeWorkspacePusher{calls: make(chan int, 10), fail: tt.fail}
			logs := &lockedBuffer{}
			d := newWorkspaceDaemon(t, gatewayEnv(), clk, pusher, logs, nil)

			if !d.startWorkspaceReconcile() {
				t.Fatal("startWorkspaceReconcile() = false with a gateway named, want true")
			}
			for want := 1; want <= 3; want++ {
				if want > 1 {
					advanceToTick(t, clk, config.DefaultWorkspaceReconcileInterval)
				}
				if got := awaitPush(t, pusher.calls); got != want {
					t.Fatalf("push = call %d, want %d", got, want)
				}
			}

			// A failure is logged after its Push returns and before the next push
			// starts, so the log is complete once push 3 has been received.
			if n := strings.Count(logs.String(), "was not updated"); n != tt.wantFailed {
				t.Errorf("failed pushes logged = %d, want %d: %q", n, tt.wantFailed, logs.String())
			}
			if tt.wantFailed > 0 && !strings.Contains(logs.String(), "retrying at the next tick") {
				t.Errorf("the failed push was not logged as retried: %q", logs.String())
			}
		})
	}
}

// TestWorkspaceReconcileInterval: each wait is the interval the patrol config
// holds when the wait starts. An unset or valid setting is used as given; a
// refused setting is logged by name and the default is used; a setting changed
// between two ticks takes effect at the next wait.
func TestWorkspaceReconcileInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// start is the patrol config at daemon start.
		start *config.DaemonPatrolConfig
		// changeAtPush2 replaces the patrol config during push 2, before the
		// reconcile reads the interval for the wait after it. Nil changes nothing.
		changeAtPush2 *config.DaemonPatrolConfig
		// ticks are the waits: each advances the clock, and the push it ends is
		// the next in order.
		ticks []time.Duration
		// wantLogs are substrings the daemon log must hold. Empty checks nothing.
		wantLogs []string
	}{
		{
			name:  "an unset interval takes the default",
			start: nil,
			ticks: []time.Duration{config.DefaultWorkspaceReconcileInterval},
		},
		{
			name:  "a valid interval is used",
			start: withPatrols("1m"),
			ticks: []time.Duration{time.Minute},
		},
		{
			name:  "a refused interval falls back to the default and is logged",
			start: withPatrols("soon"),
			ticks: []time.Duration{config.DefaultWorkspaceReconcileInterval},
			wantLogs: []string{
				"patrols.workspace_reconcile.interval \"soon\"",
				"using the " + config.DefaultWorkspaceReconcileInterval.String() + " default",
			},
		},
		{
			name:          "a setting changed between ticks applies at the next wait",
			start:         nil,
			changeAtPush2: withPatrols("1m"),
			ticks:         []time.Duration{config.DefaultWorkspaceReconcileInterval, time.Minute},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := newFixedClock()
			pusher := &fakeWorkspacePusher{calls: make(chan int, 10)}
			logs := &lockedBuffer{}
			d := newWorkspaceDaemon(t, gatewayEnv(), clk, pusher, logs, tt.start)
			pusher.onCall = func(call int) {
				if call == 2 && tt.changeAtPush2 != nil {
					setPatrolConfig(d, tt.changeAtPush2)
				}
			}

			d.startWorkspaceReconcile()
			if got := awaitPush(t, pusher.calls); got != 1 {
				t.Fatalf("start push = call %d, want 1", got)
			}
			for i, wait := range tt.ticks {
				advanceToTick(t, clk, wait)
				if got := awaitPush(t, pusher.calls); got != i+2 {
					t.Fatalf("push after a %s wait = call %d, want %d", wait, got, i+2)
				}
			}
			for _, want := range tt.wantLogs {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log does not hold %q: %q", want, logs.String())
				}
			}
		})
	}
}

// TestWorkspaceReconcileRegistration: the reconcile is in the daemon's start-up
// list exactly when a gateway is named. The list is what startWorkspaceReconcile
// reports, and Start calls it. With no gateway it installs nothing and logs
// nothing; with a gateway its first push runs.
func TestWorkspaceReconcileRegistration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		env            map[string]string
		wantRegistered bool
	}{
		{name: "no gateway: the reconcile is not registered", env: map[string]string{}, wantRegistered: false},
		{name: "a gateway named: the reconcile is registered", env: gatewayEnv(), wantRegistered: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := newFixedClock()
			pusher := &fakeWorkspacePusher{calls: make(chan int, 10)}
			logs := &lockedBuffer{}
			installs := 0
			d := newWorkspaceDaemon(t, tt.env, clk, pusher, logs, nil)
			d.installWorkspacePusher = func(bdgateway.WorkspacePusher) { installs++ }

			if got := d.startWorkspaceReconcile(); got != tt.wantRegistered {
				t.Fatalf("startWorkspaceReconcile() = %v, want %v", got, tt.wantRegistered)
			}
			if !tt.wantRegistered {
				if installs != 0 {
					t.Errorf("pusher installs with no gateway = %d, want 0", installs)
				}
				if logs.String() != "" {
					t.Errorf("log with no gateway = %q, want none", logs.String())
				}
				return
			}
			if got := awaitPush(t, pusher.calls); got != 1 {
				t.Errorf("start push = call %d, want 1", got)
			}
		})
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

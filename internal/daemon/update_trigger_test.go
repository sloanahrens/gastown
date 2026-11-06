package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/installpending"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
)

// updateTriggerTestTime is inside a 03:00 window, so the nightly rule alone
// makes an enabled trigger due when nothing stands in its way.
var updateTriggerTestTime = time.Date(2026, 3, 10, 3, 30, 0, 0, time.UTC)

// newUpdateTriggerTestDaemon builds a Daemon whose town is a temp dir and
// whose update block is enabled inside the nightly window. The launcher is a
// counter: nothing starts a process.
func newUpdateTriggerTestDaemon(t *testing.T, cfg *config.UpdateConfig) (*Daemon, *bytes.Buffer, *int, *string) {
	t.Helper()
	town := t.TempDir()
	var logs bytes.Buffer
	launches := 0
	var reason string
	d := &Daemon{
		config: DefaultConfig(town),
		logger: log.New(&logs, "", 0),
		clock:  clockwork.NewFakeClockAt(updateTriggerTestTime),
		updateTrigger: updateTriggerState{
			launch: func(_ context.Context, r string) error {
				launches++
				reason = r
				return nil
			},
		},
	}
	// The marker is read from the test town, never from the host environment's
	// INSTALL_GT_DAEMON_DIR.
	d.rebuildGTPendingFn = func() installpending.Store {
		return installpending.StoreIn(filepath.Join(town, "daemon", installpending.FileName))
	}
	if cfg != nil {
		d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{Update: cfg}}
	}
	return d, &logs, &launches, &reason
}

func TestUpdateTriggerLaunchesWhenDue(t *testing.T) {
	t.Parallel()

	d, logs, launches, reason := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
		Enabled: true,
		Window:  "03:00",
	})
	setUpdateSource(t, d)

	d.triggerUpdate()

	if *launches != 1 {
		t.Fatalf("launches = %d, want exactly one", *launches)
	}
	if !strings.Contains(*reason, "nightly") {
		t.Errorf("launch reason = %q, want it to name the nightly rule", *reason)
	}
	if got := logs.String(); !strings.Contains(got, "update: due (nightly") || !strings.Contains(got, "launching gt update") {
		t.Errorf("log = %q, want the due line naming the rule and the launch", got)
	}
}

func TestUpdateTriggerStaysQuiet(t *testing.T) {
	t.Parallel()

	writeState := func(t *testing.T, town string, st update.State) {
		t.Helper()
		store := update.Store{Dir: update.Dir(town)}
		if err := store.Write(st); err != nil {
			t.Fatalf("write state: %v", err)
		}
	}

	tests := []struct {
		name  string
		setup func(t *testing.T, d *Daemon)
		cfg   *config.UpdateConfig
		// wantReason is the guard the logged reason must name, for a town with
		// a source checkout. Without one the no-checkout short-circuit would
		// answer for every row and hide the guard under test.
		wantReason string
	}{
		{
			name: "unconfigured",
			cfg:  nil,
		},
		{
			name: "disabled",
			cfg:  &config.UpdateConfig{Enabled: false, Window: "03:00"},
		},
		{
			name: "invalid config",
			cfg:  &config.UpdateConfig{Enabled: true, Window: "25:00"},
		},
		{
			name:       "standby",
			cfg:        &config.UpdateConfig{Enabled: true, Window: "03:00"},
			wantReason: "town in standby",
			setup: func(t *testing.T, d *Daemon) {
				setUpdateSource(t, d)
				if err := os.WriteFile(estop.FilePath(d.config.TownRoot), nil, 0o644); err != nil {
					t.Fatalf("write ESTOP: %v", err)
				}
			},
		},
		{
			name:       "fresh running state",
			cfg:        &config.UpdateConfig{Enabled: true, Window: "03:00"},
			wantReason: "an update run is in progress",
			setup: func(t *testing.T, d *Daemon) {
				setUpdateSource(t, d)
				writeState(t, d.config.TownRoot, update.State{
					RunID:       "run-1",
					StartedAt:   updateTriggerTestTime.Add(-10 * time.Minute),
					HeartbeatAt: updateTriggerTestTime.Add(-time.Minute),
					Outcome:     update.OutcomeRunning,
				})
			},
		},
		{
			name:       "install pending",
			cfg:        &config.UpdateConfig{Enabled: true, Window: "03:00"},
			wantReason: "an install marker is pending",
			setup: func(t *testing.T, d *Daemon) {
				setUpdateSource(t, d)
				store := installpending.StoreIn(filepath.Join(d.config.TownRoot, "daemon", installpending.FileName))
				if err := store.Write(updateTriggerTestTime, os.Getpid()); err != nil {
					t.Fatalf("write install marker: %v", err)
				}
				d.rebuildGTPendingFn = func() installpending.Store { return store }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, launches, _ := newUpdateTriggerTestDaemon(t, tt.cfg)
			if tt.setup != nil {
				tt.setup(t, d)
			}

			d.triggerUpdate()
			if tt.name == "invalid config" {
				// A broken block is read as off on every tick, so the decision
				// must stay quiet while the error is reported once.
				d.triggerUpdate()
				if got := strings.Count(logs.String(), "update: config error:"); got != 1 {
					t.Errorf("config error lines = %d, want one per distinct message", got)
				}
			}
			if *launches != 0 {
				t.Fatalf("launches = %d, want none", *launches)
			}
			if tt.wantReason != "" {
				if got := logs.String(); !strings.Contains(got, tt.wantReason) {
					t.Errorf("log = %q, want the guard that held the launch named (%q), not another reason", got, tt.wantReason)
				}
			}
		})
	}

	t.Run("second tick within the suppression window", func(t *testing.T) {
		t.Parallel()
		d, _, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
			Enabled: true,
			Window:  "03:00",
		})
		setUpdateSource(t, d)
		d.triggerUpdate()
		// The fake clock has not moved: this is the next heartbeat, seconds
		// after the launch, before the job's first state could land.
		d.clock = clockwork.NewFakeClockAt(updateTriggerTestTime.Add(time.Minute))
		d.triggerUpdate()
		if *launches != 1 {
			t.Fatalf("launches = %d, want one (the second tick is suppressed)", *launches)
		}
	})
}

func TestUpdateFactsReadTheState(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	d := &Daemon{
		config: DefaultConfig(town),
		logger: log.New(io.Discard, "", 0),
		clock:  clockwork.NewFakeClockAt(updateTriggerTestTime),
		rebuildGTPendingFn: func() installpending.Store {
			return installpending.StoreIn(filepath.Join(town, "daemon", installpending.FileName))
		},
	}
	store := update.Store{Dir: update.Dir(town)}
	started := updateTriggerTestTime.Add(-10 * time.Minute)
	ended := updateTriggerTestTime.Add(-5 * time.Minute)
	st := update.State{
		RunID:       "run-1",
		StartedAt:   started,
		HeartbeatAt: updateTriggerTestTime.Add(-time.Minute),
		EndedAt:     ended,
		Outcome:     update.OutcomeRunning,
	}
	if err := store.Write(st); err != nil {
		t.Fatalf("write state: %v", err)
	}

	facts, err := d.updateFacts(updateTriggerTestTime)
	if err != nil {
		t.Fatalf("updateFacts: %v", err)
	}
	if !facts.Running {
		t.Error("Running = false, want true for a running state with a one-minute-old heartbeat")
	}
	if !facts.LastStarted.Equal(started) || !facts.LastEnded.Equal(ended) {
		t.Errorf("times = (%v, %v), want (%v, %v) from the state file", facts.LastStarted, facts.LastEnded, started, ended)
	}

	st.HeartbeatAt = updateTriggerTestTime.Add(-10 * time.Minute)
	if err := store.Write(st); err != nil {
		t.Fatalf("rewrite state: %v", err)
	}
	facts, err = d.updateFacts(updateTriggerTestTime)
	if err != nil {
		t.Fatalf("updateFacts: %v", err)
	}
	if facts.Running {
		t.Error("Running = true, want false for a running state whose heartbeat is ten minutes old")
	}

	if err := os.Remove(filepath.Join(update.Dir(town), "state.json")); err != nil {
		t.Fatalf("remove state: %v", err)
	}
	facts, err = d.updateFacts(updateTriggerTestTime)
	if err != nil {
		t.Fatalf("updateFacts with no state: %v", err)
	}
	if !facts.LastStarted.IsZero() || !facts.LastEnded.IsZero() {
		t.Errorf("times = (%v, %v), want zero with no state file", facts.LastStarted, facts.LastEnded)
	}
}

// A state file whose outcome names no run outcome is an unreadable fact: the
// facts are refused, so no run is launched on a value it cannot read.
func TestUpdateFactsRefusesAnUnknownOutcome(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	d := &Daemon{
		config: DefaultConfig(town),
		logger: log.New(io.Discard, "", 0),
		clock:  clockwork.NewFakeClockAt(updateTriggerTestTime),
		rebuildGTPendingFn: func() installpending.Store {
			return installpending.StoreIn(filepath.Join(town, "daemon", installpending.FileName))
		},
	}
	store := update.Store{Dir: update.Dir(town)}
	if err := store.Write(update.State{
		RunID:     "run-1",
		StartedAt: updateTriggerTestTime.Add(-time.Hour),
		EndedAt:   updateTriggerTestTime.Add(-30 * time.Minute),
		Outcome:   update.Outcome("bogus"),
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if _, err := d.updateFacts(updateTriggerTestTime); err == nil {
		t.Fatal("updateFacts() error = nil, want an error for an outcome no run writes")
	}
}

// A failed or aborted run on main's current target holds the heartbeat's
// launch until main moves or a run succeeds, for the nightly rule as well.
func TestUpdateTriggerHoldsAFailedRunOnMainsTarget(t *testing.T) {
	t.Parallel()

	const main = "1111111111111111111111111111111111111111"
	const older = "2222222222222222222222222222222222222222"
	tests := []struct {
		name         string
		outcome      update.Outcome
		target       string
		wantLaunches int
	}{
		{"failed on main's target", update.OutcomeFailed, main, 0},
		{"aborted on main's target", update.OutcomeAborted, main, 0},
		{"failed on an older target", update.OutcomeFailed, older, 1},
		{"succeeded on main's target", update.OutcomeOK, main, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
				Enabled: true,
				Window:  "03:00",
			})
			createUpdateSource(t, d.config.TownRoot)
			d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
				return &version.StaleBinaryInfo{RepoCommit: main}
			}
			// The hold reads main's tip from origin, which is main here.
			d.updateRemoteRun = (&updateRemoteFake{output: main + "\trefs/heads/main\n"}).run
			store := update.Store{Dir: update.Dir(d.config.TownRoot)}
			ended := updateTriggerTestTime.Add(-24 * time.Hour)
			if err := store.Write(update.State{
				RunID:     "run-1",
				StartedAt: ended.Add(-time.Minute),
				EndedAt:   ended,
				Outcome:   tt.outcome,
				Target:    update.Target{SHA: tt.target},
			}); err != nil {
				t.Fatalf("write state: %v", err)
			}

			d.triggerUpdate()

			if *launches != tt.wantLaunches {
				t.Fatalf("launches = %d, want %d", *launches, tt.wantLaunches)
			}
		})
	}
}

// updateRemoteFake answers the failed-run hold's ls-remote and counts the calls,
// so a test can see whether the hold read main's tip from origin at all.
type updateRemoteFake struct {
	calls  int
	output string
	exit   int
	err    error
	// deadline is how much of the call's context bound was left when the runner
	// saw it, so a test can check the bound the hold puts on the read.
	deadline time.Duration
}

func (f *updateRemoteFake) run(ctx context.Context, _ string, _ []string, _ string, _ ...string) (string, int, error) {
	f.calls++
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(deadline)
	}
	return f.output, f.exit, f.err
}

// A checkout whose origin/main ref lags main reads the remote tip for a failed
// or aborted run, so the hold compares the run's target with main's real tip
// and the same failed run does not restart (gt-tb5vx).
func TestUpdateFactsRemoteMainSHAForAFailedRun(t *testing.T) {
	t.Parallel()

	const lagging = "1111111111111111111111111111111111111111"
	const tip = "2222222222222222222222222222222222222222"
	tests := []struct {
		name        string
		outcome     update.Outcome
		output      string
		exit        int
		err         error
		wantMainSHA string
		wantCalls   int
	}{
		{"aborted reads the remote tip", update.OutcomeAborted, tip + "\trefs/heads/main\n", 0, nil, tip, 1},
		{"failed reads the remote tip", update.OutcomeFailed, tip + "\trefs/heads/main\n", 0, nil, tip, 1},
		{"ok makes no remote call", update.OutcomeOK, tip + "\trefs/heads/main\n", 0, nil, lagging, 0},
		{"nonzero exit leaves the tip unknown", update.OutcomeAborted, "", 2, nil, "", 1},
		{"runner error leaves the tip unknown", update.OutcomeFailed, "", -1, errors.New("no git"), "", 1},
		{"empty answer leaves the tip unknown", update.OutcomeAborted, "\n", 0, nil, "", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newUpdateTriggerTestDaemon(t, nil)
			createUpdateSource(t, d.config.TownRoot)
			d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
				return &version.StaleBinaryInfo{RepoCommit: lagging}
			}
			remote := &updateRemoteFake{output: tt.output, exit: tt.exit, err: tt.err}
			d.updateRemoteRun = remote.run
			store := update.Store{Dir: update.Dir(d.config.TownRoot)}
			ended := updateTriggerTestTime.Add(-time.Hour)
			if err := store.Write(update.State{
				RunID:     "run-1",
				StartedAt: ended.Add(-time.Minute),
				EndedAt:   ended,
				Outcome:   tt.outcome,
				Target:    update.Target{SHA: lagging},
			}); err != nil {
				t.Fatalf("write state: %v", err)
			}

			facts, err := d.updateFacts(updateTriggerTestTime)
			if err != nil {
				t.Fatalf("updateFacts: %v", err)
			}
			if facts.MainSHA != tt.wantMainSHA {
				t.Errorf("MainSHA = %q, want %q", facts.MainSHA, tt.wantMainSHA)
			}
			if remote.calls != tt.wantCalls {
				t.Errorf("ls-remote calls = %d, want %d", remote.calls, tt.wantCalls)
			}
		})
	}
}

// The failed-run hold holds a failed run on main's real tip even when the
// checkout's own ref lags: the run targeted tip, the checkout still reads an
// older sha, and the trigger launches nothing.
func TestUpdateTriggerHoldsAFailedRunWhenTheCheckoutLags(t *testing.T) {
	t.Parallel()

	const lagging = "1111111111111111111111111111111111111111"
	const tip = "2222222222222222222222222222222222222222"
	d, _, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
		Enabled: true,
		Window:  "03:00",
	})
	createUpdateSource(t, d.config.TownRoot)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		return &version.StaleBinaryInfo{RepoCommit: lagging}
	}
	d.updateRemoteRun = (&updateRemoteFake{output: tip + "\trefs/heads/main\n"}).run
	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	ended := updateTriggerTestTime.Add(-24 * time.Hour)
	if err := store.Write(update.State{
		RunID:     "run-1",
		StartedAt: ended.Add(-time.Minute),
		EndedAt:   ended,
		Outcome:   update.OutcomeAborted,
		Target:    update.Target{SHA: tip},
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	d.triggerUpdate()

	if *launches != 0 {
		t.Fatalf("launches = %d, want 0: the failed run targeted main's tip", *launches)
	}
}

// When main's tip cannot be read after a failed run, the hold stands and names
// the by-hand command: an unknown main is not a reason to launch again.
func TestUpdateFactsHoldsWhenTheRemoteTipIsUnreadable(t *testing.T) {
	t.Parallel()

	const lagging = "1111111111111111111111111111111111111111"
	d, _, _, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
		Enabled: true,
		Window:  "03:00",
	})
	createUpdateSource(t, d.config.TownRoot)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		return &version.StaleBinaryInfo{RepoCommit: lagging}
	}
	d.updateRemoteRun = (&updateRemoteFake{exit: 128}).run
	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	ended := updateTriggerTestTime.Add(-24 * time.Hour)
	if err := store.Write(update.State{
		RunID:     "run-1",
		StartedAt: ended.Add(-time.Minute),
		EndedAt:   ended,
		Outcome:   update.OutcomeFailed,
		Target:    update.Target{SHA: lagging},
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	facts, err := d.updateFacts(updateTriggerTestTime)
	if err != nil {
		t.Fatalf("updateFacts: %v", err)
	}
	if facts.MainSHA != "" {
		t.Fatalf("MainSHA = %q, want empty while main's tip is unread", facts.MainSHA)
	}
	due, reason := updateDue(updateTriggerTestTime, testUpdateSettings, facts)
	if due {
		t.Fatalf("updateDue() due = true (reason %q), want the hold while main's tip is unknown", reason)
	}
	if !strings.Contains(reason, "run gt update by hand") {
		t.Errorf("updateDue() reason = %q, want the by-hand hold named", reason)
	}
}

// newFailedUpdateRunTriggerDaemon builds a daemon whose town has a source
// checkout and an installed build lagging behind main, and whose last update
// run failed on the build's commit, so the failed-run hold reads main's tip
// from origin.
func newFailedUpdateRunTriggerDaemon(t *testing.T) (*Daemon, *clockwork.FakeClock, *bytes.Buffer, *updateRemoteFake) {
	t.Helper()

	const lagging = "1111111111111111111111111111111111111111"
	d, logs, _, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
		Enabled: true,
		Window:  "03:00",
	})
	createUpdateSource(t, d.config.TownRoot)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		return &version.StaleBinaryInfo{RepoCommit: lagging}
	}
	remote := &updateRemoteFake{exit: 128}
	d.updateRemoteRun = remote.run
	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	ended := updateTriggerTestTime.Add(-24 * time.Hour)
	if err := store.Write(update.State{
		RunID:     "run-1",
		StartedAt: ended.Add(-time.Minute),
		EndedAt:   ended,
		Outcome:   update.OutcomeFailed,
		Target:    update.Target{SHA: lagging},
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return d, d.clock.(*clockwork.FakeClock), logs, remote
}

// The failed-run hold's ls-remote is bounded by updateRemoteTimeout, so an
// origin that does not answer cannot hold the update check past that bound
// (gt-jhep9).
func TestUpdateFactsRemoteTimeout(t *testing.T) {
	t.Parallel()

	d, _, _, remote := newFailedUpdateRunTriggerDaemon(t)
	if _, err := d.updateFacts(d.clk().Now()); err != nil {
		t.Fatalf("updateFacts: %v", err)
	}
	if remote.calls != 1 {
		t.Fatalf("ls-remote calls = %d, want one failed read", remote.calls)
	}
	if updateRemoteTimeout != 10*time.Second {
		t.Errorf("updateRemoteTimeout = %v, want the named 10s bound", updateRemoteTimeout)
	}
	if remote.deadline <= 0 || remote.deadline > 10*time.Second {
		t.Errorf("ls-remote context had %v left, want a deadline of at most 10s", remote.deadline)
	}
}

// A failed main-tip read starts updateRemoteBackoff: reads inside it make no
// ls-remote call and leave main unknown, and the first read at the end asks
// origin again (gt-jhep9).
func TestUpdateFactsRemoteBackoff(t *testing.T) {
	t.Parallel()

	d, clk, _, remote := newFailedUpdateRunTriggerDaemon(t)
	facts, err := d.updateFacts(d.clk().Now())
	if err != nil {
		t.Fatalf("updateFacts: %v", err)
	}
	if remote.calls != 1 || facts.MainSHA != "" {
		t.Fatalf("after the first read: calls = %d, MainSHA = %q, want one failed read and an unknown main", remote.calls, facts.MainSHA)
	}

	clk.Advance(updateRemoteBackoff - time.Minute)
	facts, err = d.updateFacts(d.clk().Now())
	if err != nil {
		t.Fatalf("updateFacts inside the backoff: %v", err)
	}
	if remote.calls != 1 {
		t.Errorf("ls-remote calls = %d inside the backoff, want the remembered failure", remote.calls)
	}
	if facts.MainSHA != "" {
		t.Errorf("MainSHA = %q inside the backoff, want the cached unknown main", facts.MainSHA)
	}

	clk.Advance(2 * time.Minute)
	if _, err := d.updateFacts(d.clk().Now()); err != nil {
		t.Fatalf("updateFacts past the backoff: %v", err)
	}
	if remote.calls != 2 {
		t.Errorf("ls-remote calls = %d past the backoff, want a new read", remote.calls)
	}
}

// An unreachable origin logs once when the outage starts and once when a read
// recovers it, not on every heartbeat (gt-jhep9).
func TestUpdateFactsRemoteFailureLogsOnTransitions(t *testing.T) {
	t.Parallel()

	const tip = "2222222222222222222222222222222222222222"
	d, clk, logs, remote := newFailedUpdateRunTriggerDaemon(t)

	for range 3 {
		if _, err := d.updateFacts(d.clk().Now()); err != nil {
			t.Fatalf("updateFacts: %v", err)
		}
	}
	if remote.calls != 1 {
		t.Fatalf("ls-remote calls = %d, want the first failure only inside the backoff", remote.calls)
	}
	if got := strings.Count(logs.String(), "cannot read main's tip"); got != 1 {
		t.Fatalf("outage-start lines = %d, want one; log = %q", got, logs.String())
	}

	// The backoff ends and origin still fails: a read is made, and the
	// continuing outage stays quiet.
	clk.Advance(updateRemoteBackoff + time.Minute)
	if _, err := d.updateFacts(d.clk().Now()); err != nil {
		t.Fatalf("updateFacts after the backoff: %v", err)
	}
	if remote.calls != 2 {
		t.Fatalf("ls-remote calls = %d after the backoff, want a new read", remote.calls)
	}
	if got := strings.Count(logs.String(), "cannot read main's tip"); got != 1 {
		t.Errorf("outage-start lines = %d, want the continuing outage quiet; log = %q", got, logs.String())
	}

	// Origin answers at the next read: the recovery is logged once.
	remote.exit = 0
	remote.output = tip + "\trefs/heads/main\n"
	clk.Advance(updateRemoteBackoff + time.Minute)
	facts, err := d.updateFacts(d.clk().Now())
	if err != nil {
		t.Fatalf("updateFacts after the read recovered: %v", err)
	}
	if facts.MainSHA != tip {
		t.Fatalf("MainSHA = %q after the read recovered, want %q", facts.MainSHA, tip)
	}
	if got := strings.Count(logs.String(), "main's tip reads from origin again"); got != 1 {
		t.Errorf("recovery lines = %d, want one; log = %q", got, logs.String())
	}

	// A later failure is a new outage, logged when it starts.
	remote.exit = 128
	remote.output = ""
	if _, err := d.updateFacts(d.clk().Now()); err != nil {
		t.Fatalf("updateFacts on the new outage: %v", err)
	}
	if got := strings.Count(logs.String(), "cannot read main's tip"); got != 2 {
		t.Errorf("outage-start lines = %d, want the new outage logged; log = %q", got, logs.String())
	}
}

// A town with no gt source checkout has no main to read and no build to
// install, so the trigger launches nothing even inside the nightly window: the
// missing checkout is the reason, not a failed run holding an unknown main.
// The reason is logged once, so an operator sees the hold without a line on
// every heartbeat.
func TestUpdateTriggerDoesNotLaunchWithoutASource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state *update.State
	}{
		{name: "no run has happened"},
		{
			name: "the last run failed",
			state: &update.State{
				RunID:     "run-1",
				StartedAt: updateTriggerTestTime.Add(-54 * time.Hour),
				EndedAt:   updateTriggerTestTime.Add(-53 * time.Hour),
				Outcome:   update.OutcomeFailed,
				Target:    update.Target{SHA: strings.Repeat("a", 40)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
				Enabled: true,
				Window:  "03:00",
			})
			if tt.state != nil {
				store := update.Store{Dir: update.Dir(d.config.TownRoot)}
				if err := store.Write(*tt.state); err != nil {
					t.Fatalf("write state: %v", err)
				}
			}

			d.triggerUpdate()
			// The next heartbeat reads the same missing checkout: the reason
			// is reported once, not once per heartbeat.
			d.triggerUpdate()

			if *launches != 0 {
				t.Fatalf("launches = %d, want none: the town has no checkout to read main from or install onto", *launches)
			}
			if got := strings.Count(logs.String(), "no gt source checkout"); got != 1 {
				t.Errorf("no-checkout lines = %d, want one per standing hold; log = %q", got, logs.String())
			}
		})
	}
}

// A quiet reason is reported once while it stands, and again if it returns
// after a run was due in between.
func TestUpdateTriggerReportsARepeatedHoldAfterALaunch(t *testing.T) {
	t.Parallel()

	d, logs, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
		Enabled: true,
		Window:  "03:00",
	})
	setUpdateSource(t, d)
	// Noon is outside the 03:00 window and main is current: nothing is due.
	noon := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	d.clock = clockwork.NewFakeClockAt(noon)
	d.triggerUpdate()
	d.triggerUpdate()
	if got := strings.Count(logs.String(), "nothing due"); got != 1 {
		t.Fatalf("quiet lines = %d, want one for a reason that stands; log = %q", got, logs.String())
	}

	// The window opens and the launch retires the quiet reason.
	d.clock = clockwork.NewFakeClockAt(updateTriggerTestTime)
	d.triggerUpdate()
	if *launches != 1 {
		t.Fatalf("launches = %d, want one inside the nightly window", *launches)
	}

	// The same hold returns that day and is reported again.
	d.clock = clockwork.NewFakeClockAt(noon)
	d.triggerUpdate()
	if got := strings.Count(logs.String(), "nothing due"); got != 2 {
		t.Errorf("quiet lines = %d, want the hold reported again after a launch; log = %q", got, logs.String())
	}
}

func TestUpdateIsRegisteredAsALifecycleStep(t *testing.T) {
	t.Parallel()

	want := reflect.ValueOf((*Daemon).triggerUpdate).Pointer()
	for _, step := range heartbeatSteps {
		if step.name != "update" {
			continue
		}
		if !step.lifecycle {
			t.Error("the update step is not a lifecycle step, so the town E-stop would not hold it")
		}
		if got := reflect.ValueOf(step.run).Pointer(); got != want {
			t.Error("the update step does not run (*Daemon).triggerUpdate")
		}
		return
	}
	t.Fatal("heartbeatSteps has no step named \"update\"")
}

// setUpdateSource gives d's town a gt source checkout whose build is current
// with main, so a launch is the nightly rule's decision alone.
func setUpdateSource(t *testing.T, d *Daemon) {
	t.Helper()
	createUpdateSource(t, d.config.TownRoot)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		return &version.StaleBinaryInfo{RepoCommit: strings.Repeat("a", 40)}
	}
}

// createUpdateSource makes town/gastown look like a gt source checkout, which
// is what makes updateFacts reach the staleness and landing-record reads.
func createUpdateSource(t *testing.T, town string) string {
	t.Helper()
	source := filepath.Join(town, "gastown")
	dir := filepath.Join(source, "cmd", "gt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create source tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write source main: %v", err)
	}
	return source
}

func TestUpdateTriggerRefusesUnreadableInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, d *Daemon, town string)
	}{
		{
			name: "state cannot be parsed",
			setup: func(t *testing.T, d *Daemon, town string) {
				path := filepath.Join(update.Dir(town), "state.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("create update dir: %v", err)
				}
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatalf("write broken state: %v", err)
				}
			},
		},
		{
			name: "state outcome is not a run outcome",
			setup: func(t *testing.T, d *Daemon, town string) {
				store := update.Store{Dir: update.Dir(town)}
				if err := store.Write(update.State{
					RunID:     "run-1",
					StartedAt: updateTriggerTestTime.Add(-time.Hour),
					EndedAt:   updateTriggerTestTime.Add(-30 * time.Minute),
					Outcome:   update.Outcome("bogus"),
				}); err != nil {
					t.Fatalf("write state: %v", err)
				}
			},
		},
		{
			name: "install marker cannot be parsed",
			setup: func(t *testing.T, d *Daemon, town string) {
				path := filepath.Join(town, "daemon", installpending.FileName)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("create daemon dir: %v", err)
				}
				if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
					t.Fatalf("write broken marker: %v", err)
				}
			},
		},
		{
			name: "staleness cannot be read",
			setup: func(t *testing.T, d *Daemon, town string) {
				createUpdateSource(t, town)
				d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return nil }
			},
		},
		{
			name: "the installed build commit is unknown",
			setup: func(t *testing.T, d *Daemon, town string) {
				createUpdateSource(t, town)
				d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
					return &version.StaleBinaryInfo{IsStale: true}
				}
			},
		},
		{
			name: "the landings file cannot be read",
			setup: func(t *testing.T, d *Daemon, town string) {
				createUpdateSource(t, town)
				d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
					return &version.StaleBinaryInfo{IsStale: true, BinaryCommit: strings.Repeat("a", 40)}
				}
				// A directory where the landings file belongs: opening it
				// succeeds, reading it does not.
				if err := os.MkdirAll(filepath.Join(town, ".runtime", "landings", "gastown.jsonl"), 0o700); err != nil {
					t.Fatalf("create landings directory: %v", err)
				}
			},
		},
		{
			name: "a landing commit cannot be checked",
			setup: func(t *testing.T, d *Daemon, town string) {
				createUpdateSource(t, town)
				d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
					return &version.StaleBinaryInfo{IsStale: true, BinaryCommit: strings.Repeat("a", 40)}
				}
				dir := filepath.Join(town, ".runtime", "landings")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("create landings dir: %v", err)
				}
				rec := land.LandingRecord{BeadID: "gt-1", Rig: "gastown", LandedCommit: strings.Repeat("b", 40)}
				line, err := json.Marshal(rec)
				if err != nil {
					t.Fatalf("marshal landing record: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "gastown.jsonl"), append(line, '\n'), 0o600); err != nil {
					t.Fatalf("write landings file: %v", err)
				}
				useGitfake(t, d)
			},
		},
	}

	// Each row is driven through the heartbeat's update block: an unreadable
	// input must refuse the launch the enabled, due trigger would otherwise make.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
				Enabled: true,
				Window:  "03:00",
			})
			tt.setup(t, d, d.config.TownRoot)

			d.triggerUpdate()

			if *launches != 0 {
				t.Fatalf("launches = %d, want none: an unreadable input is unknown, not a reason to launch", *launches)
			}
			if got := logs.String(); !strings.Contains(got, "not launching") {
				t.Errorf("log = %q, want the refusal logged", got)
			}
		})
	}
}

// A refused launch waits out updateLaunchSuppression before the trigger tries
// again, so a launcher that keeps refusing is not retried on every heartbeat.
func TestUpdateTriggerBacksOffARefusedLaunch(t *testing.T) {
	t.Parallel()

	d, _, _, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
		Enabled: true,
		Window:  "03:00",
	})
	attempts := 0
	d.updateTrigger.launch = func(context.Context, string) error {
		attempts++
		return errors.New("refused")
	}
	setUpdateSource(t, d)

	d.triggerUpdate()
	// The next heartbeat is seconds later, with the launcher still refusing.
	d.clock = clockwork.NewFakeClockAt(updateTriggerTestTime.Add(time.Minute))
	d.triggerUpdate()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want one: the second heartbeat is inside the backoff", attempts)
	}

	// Once the window has passed, the trigger tries the launcher again.
	d.clock = clockwork.NewFakeClockAt(updateTriggerTestTime.Add(updateLaunchSuppression))
	d.triggerUpdate()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want two once the backoff has passed", attempts)
	}
}

// A source checkout outside a rig directory has no landings file to count:
// the read fails rather than reporting nothing waiting.
func TestUpdateLandingsWaitingRefusesAnUnknownRig(t *testing.T) {
	t.Parallel()

	d := &Daemon{config: DefaultConfig(t.TempDir()), logger: log.New(io.Discard, "", 0)}
	_, err := d.updateLandingsWaiting(filepath.Join(d.config.TownRoot, "reviews"), strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("updateLandingsWaiting accepted a checkout with no rig; want the error")
	}
}

// unknownCommit names no commit of any checkout: a landing the rig checkout has
// never fetched.
var unknownCommit = strings.Repeat("1", 40)

// updateWaitingFixture is a rig checkout whose installed build is its main tip
// after a landing, and the rig's landings file. The checkout is a gitfake world,
// so nothing runs git.
type updateWaitingFixture struct {
	d        *Daemon
	git      *gitfake.Fake
	repo     string
	build    string
	deployed string
	landings *land.LandingsFile
}

func newUpdateWaitingFixture(t *testing.T) *updateWaitingFixture {
	t.Helper()
	town := t.TempDir()
	d := &Daemon{config: DefaultConfig(town), logger: log.New(io.Discard, "", 0)}
	f := useGitfake(t, d)
	repo := filepath.Join(town, "gastown", "mayor", "rig")
	f.InitRepo(t, repo)
	deployed := f.Commit(t, repo, "main", "landed", map[string]string{"a.txt": "a\n"})
	build := f.Commit(t, repo, "main", "installed build", map[string]string{"b.txt": "b\n"})
	lf, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	return &updateWaitingFixture{d: d, git: f, repo: repo, build: build, deployed: deployed, landings: lf}
}

// commitAfterBuild makes a commit on main past the installed build: a landing
// the build does not contain.
func (fx *updateWaitingFixture) commitAfterBuild(t *testing.T) string {
	t.Helper()
	return fx.git.Commit(t, fx.repo, "main", "landed after the build", map[string]string{"c.txt": "c\n"})
}

// appendLandings records shas on the rig's landings file, oldest first.
func (fx *updateWaitingFixture) appendLandings(t *testing.T, shas ...string) {
	t.Helper()
	for _, sha := range shas {
		if err := fx.landings.Append(land.LandingRecord{BeadID: sha, LandedCommit: sha, LandedAt: updateTriggerTestTime}); err != nil {
			t.Fatalf("appending a landing record: %v", err)
		}
	}
}

// A landing the checkout has never fetched is not in the installed build, so it
// is waiting to deploy: the walk counts it and keeps going to the older landings.
func TestUpdateLandingsWaitingUnknownCommitCountsAsWaiting(t *testing.T) {
	t.Parallel()

	fx := newUpdateWaitingFixture(t)
	later := fx.commitAfterBuild(t)
	fx.appendLandings(t, fx.deployed, later, unknownCommit)

	got, err := fx.d.updateLandingsWaiting(fx.repo, fx.build)
	if err != nil {
		t.Fatalf("updateLandingsWaiting: %v; want the unknown landing counted as waiting", err)
	}
	if got != 2 {
		t.Errorf("waiting = %d, want 2 (the unknown landing and the one after the build)", got)
	}
}

// Any git error other than an unknown commit still fails the read: here the
// repository cannot be read, so nothing is counted as waiting.
func TestUpdateLandingsWaitingOtherGitErrorStillFails(t *testing.T) {
	t.Parallel()

	fx := newUpdateWaitingFixture(t)
	fx.appendLandings(t, fx.deployed, unknownCommit)
	fx.git.RemoveRepo(t, fx.repo)

	got, err := fx.d.updateLandingsWaiting(fx.repo, fx.build)
	if err == nil {
		t.Fatalf("updateLandingsWaiting = %d, nil; want an error for an unreadable repository", got)
	}
}

// Every landing older than the first one the build contains is in the build
// too, so the walk stops there and an unknown landing below it is never read.
func TestUpdateLandingsWaitingStopsAtFirstDeployed(t *testing.T) {
	t.Parallel()

	fx := newUpdateWaitingFixture(t)
	later := fx.commitAfterBuild(t)
	fx.appendLandings(t, unknownCommit, fx.deployed, later)

	got, err := fx.d.updateLandingsWaiting(fx.repo, fx.build)
	if err != nil {
		t.Fatalf("updateLandingsWaiting: %v", err)
	}
	if got != 1 {
		t.Errorf("waiting = %d, want 1 (only the landing after the build)", got)
	}
}

func TestUpdateLauncherRefusesAnUnknownExecutable(t *testing.T) {
	t.Parallel()

	_, _, err := buildUpdateLaunch(t.TempDir(), updateTriggerTestTime, func() (string, error) {
		return "", errors.New("no executable")
	})
	if err == nil {
		t.Fatal("buildUpdateLaunch succeeded with no executable; want the error")
	}
}

func TestUpdateLauncherRefusesAnUnusableUpdateDirectory(t *testing.T) {
	t.Parallel()

	// A town path that is a regular file cannot hold .runtime/update.
	file := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, _, err := buildUpdateLaunch(file, updateTriggerTestTime, os.Executable)
	if err == nil {
		t.Fatal("buildUpdateLaunch succeeded under a file town root; want the mkdir error")
	}
}

func TestUpdateLauncherRefusesASecondLogInTheSameSecond(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	_, firstLog, err := buildUpdateLaunch(town, updateTriggerTestTime, os.Executable)
	if err != nil {
		t.Fatalf("first buildUpdateLaunch: %v", err)
	}
	defer func() { _ = firstLog.Close() }()

	if _, _, err := buildUpdateLaunch(town, updateTriggerTestTime, os.Executable); err == nil {
		t.Fatal("a second launch log for the same second opened; want the exclusive-create error")
	}
}

func TestUpdateLauncherRefusesACanceledContext(t *testing.T) {
	t.Parallel()

	d := &Daemon{config: DefaultConfig(t.TempDir()), logger: log.New(io.Discard, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.launchUpdate(ctx, "test"); err == nil {
		t.Fatal("launchUpdate started under a canceled context; want the context error")
	}
}

func TestUpdateLauncherReportsAnUnusableUpdateDirectory(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	d := &Daemon{config: DefaultConfig(file), logger: log.New(io.Discard, "", 0)}
	if err := d.launchUpdate(context.Background(), "test"); err == nil {
		t.Fatal("launchUpdate succeeded with an unusable update directory; want the build error")
	}
}

func TestUpdateLauncherBuildsADetachedCommand(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	cmd, logFile, err := buildUpdateLaunch(town, updateTriggerTestTime, os.Executable)
	if err != nil {
		t.Fatalf("buildUpdateLaunch: %v", err)
	}
	defer func() { _ = logFile.Close() }()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if cmd.Path != exe {
		t.Errorf("cmd.Path = %q, want the daemon's executable %q", cmd.Path, exe)
	}
	if want := []string{exe, "update", "--from-daemon"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd.Args = %q, want %q", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Errorf("SysProcAttr = %+v, want Setsid so the job outlives the daemon's session", cmd.SysProcAttr)
	}
	if cmd.Process != nil {
		t.Error("the command was started; buildUpdateLaunch must only build it")
	}

	wantLog := filepath.Join(update.Dir(town), "launch-20260310T033000Z.log")
	if logFile.Name() != wantLog {
		t.Errorf("launch log = %q, want %q", logFile.Name(), wantLog)
	}
	if cmd.Stdout != logFile || cmd.Stderr != logFile {
		t.Error("stdout and stderr do not both go to the launch log")
	}
	info, err := os.Stat(wantLog)
	if err != nil {
		t.Fatalf("stat launch log: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("launch log mode = %o, want 600", perm)
	}
}

// TestStartUpdateProcessReportsAStartFailure drives the default starter's
// refusal: a command that cannot start yields the error and no wait to run.
func TestStartUpdateProcessReportsAStartFailure(t *testing.T) {
	t.Parallel()

	// A command whose program does not exist: Start fails without a process.
	cmd := &exec.Cmd{Path: filepath.Join(t.TempDir(), "no-such-update-job")}
	wait, err := startUpdateProcess(cmd)
	if err == nil {
		t.Fatal("startUpdateProcess started a missing executable; want the Start error")
	}
	if wait != nil {
		t.Error("startUpdateProcess returned a wait for a command that never started")
	}
}

// TestLaunchUpdateReapsTheChild: the launched job exits on its own, so the
// daemon waits for it from a goroutine of its own — an unreaped child stays a
// zombie until the daemon restarts — and the wait never holds the launch.
func TestLaunchUpdateReapsTheChild(t *testing.T) {
	t.Parallel()

	d := &Daemon{config: DefaultConfig(t.TempDir()), logger: log.New(io.Discard, "", 0)}
	waitStarted := make(chan struct{})
	release := make(chan struct{})
	waitDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseWait := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWait)
	// The starter never touches cmd: it hands back a wait that parks until the
	// test releases it, so a wait run inline would keep the launch from
	// returning.
	d.updateStarter = func(*exec.Cmd) (func(), error) {
		return func() {
			close(waitStarted)
			<-release
			close(waitDone)
		}, nil
	}

	launched := make(chan error, 1)
	go func() { launched <- d.launchUpdate(context.Background(), "test") }()

	if err := within(t, time.Second, "launchUpdate", launched); err != nil {
		t.Fatalf("launchUpdate: %v", err)
	}
	within(t, time.Second, "the reap to start", waitStarted)
	releaseWait()
	within(t, time.Second, "the reap to finish", waitDone)
}

// TestLaunchUpdateReportsAStarterFailure: a starter that cannot start the job
// fails the launch, and no wait is run for a child that does not exist.
func TestLaunchUpdateReportsAStarterFailure(t *testing.T) {
	t.Parallel()

	d := &Daemon{config: DefaultConfig(t.TempDir()), logger: log.New(io.Discard, "", 0)}
	want := errors.New("start refused")
	d.updateStarter = func(*exec.Cmd) (func(), error) { return nil, want }

	if err := d.launchUpdate(context.Background(), "test"); !errors.Is(err, want) {
		t.Fatalf("launchUpdate = %v, want the starter error", err)
	}
}

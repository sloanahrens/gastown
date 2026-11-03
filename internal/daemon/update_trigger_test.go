package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/installpending"
	"github.com/steveyegge/gastown/internal/update"
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
		updateLaunch: func(_ context.Context, r string) error {
			launches++
			reason = r
			return nil
		},
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
			name: "standby",
			cfg:  &config.UpdateConfig{Enabled: true, Window: "03:00"},
			setup: func(t *testing.T, d *Daemon) {
				if err := os.WriteFile(estop.FilePath(d.config.TownRoot), nil, 0o644); err != nil {
					t.Fatalf("write ESTOP: %v", err)
				}
			},
		},
		{
			name: "fresh running state",
			cfg:  &config.UpdateConfig{Enabled: true, Window: "03:00"},
			setup: func(t *testing.T, d *Daemon) {
				writeState(t, d.config.TownRoot, update.State{
					RunID:       "run-1",
					StartedAt:   updateTriggerTestTime.Add(-10 * time.Minute),
					HeartbeatAt: updateTriggerTestTime.Add(-time.Minute),
					Outcome:     update.OutcomeRunning,
				})
			},
		},
		{
			name: "install pending",
			cfg:  &config.UpdateConfig{Enabled: true, Window: "03:00"},
			setup: func(t *testing.T, d *Daemon) {
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
		})
	}

	t.Run("second tick within the suppression window", func(t *testing.T) {
		t.Parallel()
		d, _, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
			Enabled: true,
			Window:  "03:00",
		})
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

func TestUpdateLauncherBuildsADetachedCommand(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	cmd, logFile, err := buildUpdateLaunch(town, updateTriggerTestTime)
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

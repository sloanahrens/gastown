package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		updateLaunch: func(_ context.Context, r string) error {
			launches++
			reason = r
			return nil
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

func TestUpdateFactsRefusesUnreadableInputs(t *testing.T) {
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			tt.setup(t, d, town)

			if _, err := d.updateFacts(updateTriggerTestTime); err == nil {
				t.Fatal("updateFacts read an unreadable input as an answer; want the error")
			}
		})
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

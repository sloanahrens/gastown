package cmd

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/update"
)

// fakeSeatProbeEnv is a town the seat probe can read: fixed rigs, polecat
// names, prefixes and heartbeats, with an injected error per reading.
type fakeSeatProbeEnv struct {
	rigs         []string
	rigsErr      error
	polecats     map[string][]string
	polecatErr   map[string]error
	prefixes     map[string]string
	heartbeats   map[string]*polecat.SessionHeartbeat
	heartbeatErr map[string]error
}

func (e fakeSeatProbeEnv) env() seatProbeEnv {
	return seatProbeEnv{
		Rigs: func() ([]string, error) {
			return e.rigs, e.rigsErr
		},
		Polecats: func(rig string) ([]string, error) {
			return e.polecats[rig], e.polecatErr[rig]
		},
		Prefix: func(rig string) string {
			if prefix, ok := e.prefixes[rig]; ok {
				return prefix
			}
			return "gt"
		},
		Heartbeat: func(session string) (*polecat.SessionHeartbeat, error) {
			if err, ok := e.heartbeatErr[session]; ok {
				return nil, err
			}
			return e.heartbeats[session], nil
		},
	}
}

// TestUpdateSeatProbeReadsHeartbeats pins the seat a drain sees for each
// heartbeat state: a v1 heartbeat (no state) and a reported "working" one are
// working, an exiting one is not, and a missing one is not working and has
// never been active. Names carry the rig, and the session name is built from
// that rig's prefix.
func TestUpdateSeatProbeReadsHeartbeats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	env := fakeSeatProbeEnv{
		rigs:     []string{"gastown", "beads"},
		polecats: map[string][]string{"gastown": {"nux", "toast", "amber"}, "beads": {"rictus"}},
		prefixes: map[string]string{"gastown": "gt", "beads": "bd"},
		heartbeats: map[string]*polecat.SessionHeartbeat{
			"gt-nux":    {Timestamp: now, State: polecat.HeartbeatWorking},
			"gt-toast":  {Timestamp: now.Add(-time.Minute), State: polecat.HeartbeatExiting},
			"bd-rictus": {Timestamp: now.Add(-2 * time.Minute)},
		},
	}

	seats, err := newSeatProbe(env.env()).Seats(context.Background())
	if err != nil {
		t.Fatalf("Seats() error = %v, want nil", err)
	}
	want := []update.Seat{
		{Name: "gastown/nux", Working: true, LastActive: now},
		{Name: "gastown/toast", LastActive: now.Add(-time.Minute)},
		{Name: "gastown/amber"},
		{Name: "beads/rictus", Working: true, LastActive: now.Add(-2 * time.Minute)},
	}
	if !slices.Equal(seats, want) {
		t.Errorf("Seats() = %+v, want %+v", seats, want)
	}
}

// TestUpdateSeatProbeErrorsFailClosed pins the drain's only source of "who is
// working": a list it could not read is an error, never an empty seat list,
// while a rig that never had a polecats directory has no seats.
func TestUpdateSeatProbeErrorsFailClosed(t *testing.T) {
	t.Parallel()

	t.Run("a rigs error is returned", func(t *testing.T) {
		t.Parallel()
		seats, err := newSeatProbe(fakeSeatProbeEnv{rigsErr: errors.New("routes.jsonl is unreadable")}.env()).Seats(context.Background())
		if err == nil {
			t.Fatal("Seats() error = nil, want the rigs error")
		}
		if !strings.Contains(err.Error(), "routes.jsonl is unreadable") {
			t.Errorf("Seats() error = %v, want it to name the rigs error", err)
		}
		if seats != nil {
			t.Errorf("Seats() = %+v with an error, want no seats", seats)
		}
	})

	t.Run("a polecats error for one rig is returned", func(t *testing.T) {
		t.Parallel()
		env := fakeSeatProbeEnv{
			rigs:       []string{"gastown", "beads"},
			polecats:   map[string][]string{"gastown": {"nux"}},
			polecatErr: map[string]error{"beads": fs.ErrPermission},
			heartbeats: map[string]*polecat.SessionHeartbeat{
				"gt-nux": {Timestamp: time.Now(), State: polecat.HeartbeatWorking},
			},
		}
		seats, err := newSeatProbe(env.env()).Seats(context.Background())
		if err == nil {
			t.Fatal("Seats() error = nil, want the polecats error")
		}
		if !strings.Contains(err.Error(), "beads") {
			t.Errorf("Seats() error = %v, want it to name the rig", err)
		}
		if seats != nil {
			t.Errorf("Seats() = %+v with an error, want no seats (not the rigs read before it)", seats)
		}
	})

	t.Run("a rig without a polecats directory has no seats", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "polecats", "nux"), 0o755); err != nil {
			t.Fatalf("creating the polecats directory: %v", err)
		}
		now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		env := fakeSeatProbeEnv{
			rigs: []string{"gastown", "empty"},
			heartbeats: map[string]*polecat.SessionHeartbeat{
				"gt-nux": {Timestamp: now, State: polecat.HeartbeatWorking},
			},
		}.env()
		// The real reading: a rig with no polecats directory returns the
		// ReadDir error, which the probe must read as "no polecats".
		env.Polecats = func(rig string) ([]string, error) {
			return polecatDirs(filepath.Join(townRoot, rig, "polecats"))
		}
		seats, err := newSeatProbe(env).Seats(context.Background())
		if err != nil {
			t.Fatalf("Seats() error = %v, want nil for a rig with no polecats directory", err)
		}
		want := []update.Seat{{Name: "gastown/nux", Working: true, LastActive: now}}
		if !slices.Equal(seats, want) {
			t.Errorf("Seats() = %+v, want %+v", seats, want)
		}
	})

	t.Run("a rig whose polecats path is not a directory is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(townRoot, "gastown"), 0o755); err != nil {
			t.Fatalf("creating the rig directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(townRoot, "gastown", "polecats"), []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("writing the file where polecats should be: %v", err)
		}
		env := fakeSeatProbeEnv{rigs: []string{"gastown"}}.env()
		env.Polecats = func(rig string) ([]string, error) {
			return polecatDirs(filepath.Join(townRoot, rig, "polecats"))
		}
		seats, err := newSeatProbe(env).Seats(context.Background())
		if err == nil {
			t.Fatal("Seats() error = nil, want the unreadable-directory error")
		}
		if !strings.Contains(err.Error(), "gastown") {
			t.Errorf("Seats() error = %v, want it to name the rig", err)
		}
		if seats != nil {
			t.Errorf("Seats() = %+v with an error, want no seats", seats)
		}
	})

	t.Run("a cancelled context is returned", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		seats, err := newSeatProbe(fakeSeatProbeEnv{rigs: []string{"gastown"}}.env()).Seats(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Seats() error = %v, want context.Canceled", err)
		}
		if seats != nil {
			t.Errorf("Seats() = %+v with a cancelled context, want no seats", seats)
		}
	})
}

// TestUpdateSeatProbeUnreadableHeartbeatIsAnError pins the difference between
// a polecat that wrote no heartbeat and one whose heartbeat cannot be read:
// the first is a seat that is not working, the second is an error naming the
// seat, so the drain fails closed instead of installing over a working
// polecat. Both readings come from the production file reader.
func TestUpdateSeatProbeUnreadableHeartbeatIsAnError(t *testing.T) {
	t.Parallel()

	t.Run("an unparseable heartbeat is an error naming the seat", func(t *testing.T) {
		t.Parallel()
		townRoot := townWithOnePolecat(t)
		writeHeartbeatFile(t, townRoot, "gt-nux", `{"timestamp": "not a time"}`)

		seats, err := newSeatProbe(townSeatProbeEnv(townRoot)).Seats(context.Background())
		if err == nil {
			t.Fatalf("Seats() error = nil, want the unparseable-heartbeat error")
		}
		if !strings.Contains(err.Error(), "gastown/nux") {
			t.Errorf("Seats() error = %v, want it to name the seat", err)
		}
		if seats != nil {
			t.Errorf("Seats() = %+v with an error, want no seats", seats)
		}
	})

	t.Run("an unreadable heartbeat is an error naming the seat", func(t *testing.T) {
		t.Parallel()
		townRoot := townWithOnePolecat(t)
		// A directory where the file should be: it exists, and reading it
		// fails with a reason other than "missing".
		if err := os.MkdirAll(filepath.Join(townRoot, ".runtime", "heartbeats", "gt-nux.json"), 0o755); err != nil {
			t.Fatalf("creating the unreadable heartbeat path: %v", err)
		}

		seats, err := newSeatProbe(townSeatProbeEnv(townRoot)).Seats(context.Background())
		if err == nil {
			t.Fatalf("Seats() error = nil, want the unreadable-heartbeat error")
		}
		if !strings.Contains(err.Error(), "gastown/nux") {
			t.Errorf("Seats() error = %v, want it to name the seat", err)
		}
		if seats != nil {
			t.Errorf("Seats() = %+v with an error, want no seats", seats)
		}
	})

	t.Run("a missing heartbeat is a seat that is not working", func(t *testing.T) {
		t.Parallel()
		seats, err := newSeatProbe(townSeatProbeEnv(townWithOnePolecat(t))).Seats(context.Background())
		if err != nil {
			t.Fatalf("Seats() error = %v, want nil when no heartbeat file exists", err)
		}
		want := []update.Seat{{Name: "gastown/nux"}}
		if !slices.Equal(seats, want) {
			t.Errorf("Seats() = %+v, want %+v", seats, want)
		}
	})

	t.Run("a heartbeat polecat writes is read", func(t *testing.T) {
		t.Parallel()
		townRoot := townWithOnePolecat(t)
		polecat.TouchSessionHeartbeat(townRoot, "gt-nux")

		seats, err := newSeatProbe(townSeatProbeEnv(townRoot)).Seats(context.Background())
		if err != nil {
			t.Fatalf("Seats() error = %v, want the heartbeat polecat wrote", err)
		}
		want := []update.Seat{{Name: "gastown/nux", Working: true}}
		if len(seats) != len(want) || seats[0].Name != want[0].Name || seats[0].Working != want[0].Working {
			t.Errorf("Seats() = %+v, want %+v", seats, want)
		}
		if seats[0].LastActive.IsZero() {
			t.Errorf("Seats() = %+v, want the written heartbeat's timestamp", seats)
		}
	})
}

// townWithOnePolecat is a town with one rig, gastown, holding polecat nux and
// no heartbeat files.
func townWithOnePolecat(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "polecats", "nux"), 0o755); err != nil {
		t.Fatalf("creating the polecat directory: %v", err)
	}
	return townRoot
}

// townSeatProbeEnv reads townRoot through the production readings the update
// command wires: the polecat directories and the heartbeat files.
func townSeatProbeEnv(townRoot string) seatProbeEnv {
	return seatProbeEnv{
		Rigs: func() ([]string, error) {
			return []string{"gastown"}, nil
		},
		Polecats: func(rig string) ([]string, error) {
			return polecatDirs(filepath.Join(townRoot, rig, "polecats"))
		},
		Prefix: func(string) string {
			return "gt"
		},
		Heartbeat: heartbeatReader(townRoot),
	}
}

// writeHeartbeatFile writes body as session's heartbeat file, at the path the
// production reader looks for.
func writeHeartbeatFile(t *testing.T, townRoot, session, body string) {
	t.Helper()
	path := filepath.Join(townRoot, ".runtime", "heartbeats", session+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating the heartbeat directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the heartbeat file: %v", err)
	}
}

// fakeExitCode is a command error carrying an exit code, the shape exec gives
// a process that exits nonzero.
type fakeExitCode int

func (e fakeExitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }

func (e fakeExitCode) ExitCode() int { return int(e) }

// TestRealRunFuncExitCodes pins the runner's contract through an injected
// process seam: a nonzero exit is a status carrying the output, a command
// that cannot start is the only error, and the command runs in the caller's
// directory with the caller's env appended rather than replacing the process
// environment.
func TestRealRunFuncExitCodes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	envEntry := "UPDATE_EFFECTS_RESULT=appended"

	t.Run("a nonzero exit is a status with its output", func(t *testing.T) {
		t.Parallel()
		run := newRunFunc(func(*exec.Cmd) ([]byte, error) {
			return []byte("result=appended\n"), fakeExitCode(3)
		})

		out, exit, err := run(context.Background(), dir, []string{envEntry}, "installer")
		if err != nil {
			t.Fatalf("run(exit 3) error = %v, want nil", err)
		}
		if exit != 3 {
			t.Errorf("run(exit 3) exit = %d, want 3", exit)
		}
		if !strings.Contains(out, "result=appended") {
			t.Errorf("run(exit 3) output = %q, want the command's output", out)
		}
	})

	t.Run("a command that cannot start is the only error", func(t *testing.T) {
		t.Parallel()
		run := newRunFunc(func(*exec.Cmd) ([]byte, error) {
			return nil, exec.ErrNotFound
		})

		if _, _, err := run(context.Background(), dir, nil, "gt-update-effects-no-such-command"); !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("run(missing command) error = %v, want exec.ErrNotFound in its chain", err)
		}
	})

	t.Run("the command runs in dir with the caller's env appended", func(t *testing.T) {
		t.Parallel()
		wantEnvLen := len(os.Environ()) + 1
		var got *exec.Cmd
		run := newRunFunc(func(cmd *exec.Cmd) ([]byte, error) {
			got = cmd
			return nil, nil
		})

		if _, _, err := run(context.Background(), dir, []string{envEntry}, "installer", "--sha", "abc"); err != nil {
			t.Fatalf("run() error = %v, want nil", err)
		}
		if got == nil {
			t.Fatal("run() started no command")
		}
		if got.Dir != dir {
			t.Errorf("cmd.Dir = %q, want %q", got.Dir, dir)
		}
		if !slices.Contains(got.Env, envEntry) {
			t.Errorf("cmd.Env = %q, want it to hold %q", got.Env, envEntry)
		}
		if len(got.Env) != wantEnvLen {
			t.Errorf("len(cmd.Env) = %d, want %d: the caller's env is appended to the process environment, not given instead of it", len(got.Env), wantEnvLen)
		}
		if want := []string{"installer", "--sha", "abc"}; !slices.Equal(got.Args, want) {
			t.Errorf("cmd.Args = %q, want %q", got.Args, want)
		}
	})
}

// TestRealRunFuncReportsCancellation pins what a cancelled context yields: the
// context's error, never a -1 exit code that reads as a finished command.
func TestRealRunFuncReportsCancellation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("an already cancelled context starts nothing", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		started := false
		run := newRunFunc(func(*exec.Cmd) ([]byte, error) {
			started = true
			return nil, nil
		})

		_, _, err := run(ctx, dir, nil, "installer")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("run(cancelled context) error = %v, want context.Canceled", err)
		}
		if started {
			t.Error("run(cancelled context) started a command, want none")
		}
	})

	t.Run("a process stopped by its context returns the context error", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := newRunFunc(func(*exec.Cmd) ([]byte, error) {
			// The signal arrives while the command runs; the process dies
			// with exit code -1 and no error of its own.
			cancel()
			return []byte("killed"), fakeExitCode(-1)
		})

		out, exit, err := run(ctx, dir, nil, "installer")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("run(stopped process) error = %v, want context.Canceled", err)
		}
		if exit != -1 {
			t.Errorf("run(stopped process) exit = %d, want -1", exit)
		}
		if out != "killed" {
			t.Errorf("run(stopped process) output = %q, want the output written before it stopped", out)
		}
	})
}

// TestNewExecInstallerRepoRootAndTrimEnv pins what the update job's installer
// is wired to: the town's own gt source checkout, the tier sweep's trim entry
// when that patrol is on, and an error rather than an installer for a town
// with no checkout or a context no container engine can name.
func TestNewExecInstallerRepoRootAndTrimEnv(t *testing.T) {
	t.Parallel()
	run := func(context.Context, string, []string, string, ...string) (string, int, error) {
		return "ran", 0, nil
	}
	sweep := func(context string) *daemon.DaemonPatrolConfig {
		return &daemon.DaemonPatrolConfig{Patrols: &daemon.PatrolsConfig{TierSweep: &daemon.TierSweepConfig{
			Enabled: true,
			Runner:  &config.TierSweepRunnerConfig{Backend: config.TierSweepRunnerDocker, Context: context},
		}}}
	}

	t.Run("repo root and the trim entry", func(t *testing.T) {
		t.Parallel()
		townRoot := townWithGtSource(t)
		inst, err := newExecInstaller(townRoot, run, sweep("colima-gt-control"))
		if err != nil {
			t.Fatalf("newExecInstaller() error = %v, want nil", err)
		}
		ex, ok := inst.(*update.ExecInstaller)
		if !ok {
			t.Fatalf("newExecInstaller() = %T, want *update.ExecInstaller", inst)
		}
		if want := filepath.Join(townRoot, "gastown"); ex.RepoRoot != want {
			t.Errorf("RepoRoot = %q, want %q", ex.RepoRoot, want)
		}
		want := []string{"POLECAT_IMAGE_TRIM_CONTEXT=colima-gt-control"}
		if !slices.Equal(ex.ExtraEnv, want) {
			t.Errorf("ExtraEnv = %q, want %q", ex.ExtraEnv, want)
		}
		if ex.Run == nil {
			t.Fatal("Run = nil, want the runner the constructor was given")
		}
		if out, _, err := ex.Run(context.Background(), "", nil, "probe"); err != nil || out != "ran" {
			t.Errorf("Run() = (%q, %v), want the injected runner's answer", out, err)
		}
	})

	t.Run("no trim entry without an active sweep", func(t *testing.T) {
		t.Parallel()
		inst, err := newExecInstaller(townWithGtSource(t), run, nil)
		if err != nil {
			t.Fatalf("newExecInstaller() error = %v, want nil", err)
		}
		if ex := inst.(*update.ExecInstaller); len(ex.ExtraEnv) != 0 {
			t.Errorf("ExtraEnv = %q, want none", ex.ExtraEnv)
		}
	})

	t.Run("an invalid context is returned", func(t *testing.T) {
		t.Parallel()
		inst, err := newExecInstaller(townWithGtSource(t), run, sweep("not/a/context"))
		if err == nil {
			t.Fatal("newExecInstaller() error = nil, want the invalid-context error")
		}
		if inst != nil {
			t.Errorf("newExecInstaller() = %T with an error, want nil", inst)
		}
	})

	t.Run("a town without a gt source checkout is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		inst, err := newExecInstaller(townRoot, run, nil)
		if err == nil {
			t.Fatal("newExecInstaller() error = nil, want the missing-checkout error")
		}
		if inst != nil {
			t.Errorf("newExecInstaller() = %T with an error, want nil", inst)
		}
	})
}

// townWithGtSource is a town whose gastown rig is a gt source checkout, the
// tree version.GetRepoRootForTown looks for.
func townWithGtSource(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	mainPath := filepath.Join(townRoot, "gastown", "cmd", "gt", "main.go")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o755); err != nil {
		t.Fatalf("creating the gt source tree: %v", err)
	}
	if err := os.WriteFile(mainPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("writing the gt source file: %v", err)
	}
	return townRoot
}

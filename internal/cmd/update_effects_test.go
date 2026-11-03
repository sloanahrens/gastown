package cmd

import (
	"context"
	"errors"
	"fmt"
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
	rigs       []string
	rigsErr    error
	polecats   map[string][]string
	polecatErr map[string]error
	prefixes   map[string]string
	heartbeats map[string]*polecat.SessionHeartbeat
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
		Heartbeat: func(session string) *polecat.SessionHeartbeat {
			return e.heartbeats[session]
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
			return polecatDirNames(filepath.Join(townRoot, rig, "polecats"))
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

// polecatDirNames lists the directory names under dir, the reading the update
// command gives the seat probe. A missing directory keeps its ReadDir error,
// so the test drives the probe's own ErrNotExist branch.
func polecatDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// TestRealRunFuncExitCodes pins the runner's contract: a nonzero exit is a
// status carrying the output, a command that cannot start is the only error,
// and the caller's env is added to the process environment rather than
// replacing it. The exit comes from re-executing this test binary, which the
// unit tier permits where it refuses a shell.
func TestRealRunFuncExitCodes(t *testing.T) {
	t.Parallel()
	run := realRunFunc()
	dir := t.TempDir()

	out, exit, err := run(context.Background(), dir,
		[]string{"UPDATE_EFFECTS_EXIT_CODE=3", "UPDATE_EFFECTS_RESULT=appended"},
		os.Args[0], "-test.run=^TestRealRunFuncHelperProcess$")
	if err != nil {
		t.Fatalf("run(exit 3) error = %v, want nil", err)
	}
	if exit != 3 {
		t.Errorf("run(exit 3) exit = %d, want 3", exit)
	}
	if !strings.Contains(out, "result=appended") {
		t.Errorf("run(exit 3) output = %q, want the caller's env entry to reach the process and its output", out)
	}

	_, _, err = run(context.Background(), dir, nil, "gt-update-effects-no-such-command")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("run(missing command) error = %v, want exec.ErrNotFound in its chain", err)
	}
}

// TestRealRunFuncHelperProcess is the process TestRealRunFuncExitCodes runs:
// the environment names the exit code and a value to print, so the parent
// reads both back through realRunFunc. A normal package run sets neither.
func TestRealRunFuncHelperProcess(t *testing.T) {
	t.Parallel()
	codeText := os.Getenv("UPDATE_EFFECTS_EXIT_CODE")
	if codeText == "" {
		return
	}
	fmt.Printf("result=%s\n", os.Getenv("UPDATE_EFFECTS_RESULT"))
	code, err := strconv.Atoi(codeText)
	if err != nil {
		os.Exit(2)
	}
	os.Exit(code)
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

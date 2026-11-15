package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

// townValuesPair is the pair the fixture's --images file holds.
func townValuesPair() imagevalues.Values {
	return imagevalues.Values{Daemon: podSwapDigestRef, Polecat: podPairPolecatRef, Commit: rollbackNewCommit}
}

// townValuesRecord is the writer a town-values test injects: it records each
// call and answers with err.
type townValuesRecord struct {
	calls  int
	path   string
	values imagevalues.Values
	err    error
}

func (r *townValuesRecord) write(path string, v imagevalues.Values) error {
	r.calls++
	r.path = path
	r.values = v
	return r.err
}

// townValuesCommand is gt update over seams a test can inspect: the town and
// the --images file are under a temporary directory, and every effect past the
// orchestrator is a fake.
type townValuesCommand struct {
	seams    updateSeams
	record   *townValuesRecord
	townRoot string
	images   string
}

// newTownValuesCommand builds the command over run, the orchestrator stand-in.
func newTownValuesCommand(t *testing.T, run func(context.Context, update.Deps, update.Options) (update.State, error)) townValuesCommand {
	t.Helper()
	townRoot := t.TempDir()
	images := filepath.Join(townRoot, "images.yaml")
	if err := imagevalues.Write(images, townValuesPair()); err != nil {
		t.Fatalf("write the --images file: %v", err)
	}
	record := &townValuesRecord{}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{
				Store:     &update.Store{Dir: update.Dir(townRoot)},
				Clock:     clock,
				Sleep:     clock.sleep,
				Installer: &podHostInstaller{},
			}, nil
		},
		Run:       run,
		ImageSwap: func(string) (update.ImageSwap, error) { return &rollbackTestSwap{}, nil },
		PodRun: func(context.Context, string, []string, string, ...string) (string, int, error) {
			return "", 0, nil
		},
		Notifier:        func(string) notify.Notifier { return notifyfake.New() },
		Images:          podPairImages(podPairManifest),
		WriteTownValues: record.write,
	}
	return townValuesCommand{seams: seams, record: record, townRoot: townRoot, images: images}
}

// townValuesPath is where the town reads the pair a roll installed.
func (c townValuesCommand) townValuesPath() string {
	return filepath.Join(c.townRoot, "gastown", imagevalues.DefaultRelPath)
}

// podRunArgs names a pod target with c's values file.
func (c townValuesCommand) podRunArgs() []string {
	return []string{"--images", c.images, "--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace}
}

// TestUpdateTownValuesWritesOnAnOKPodRun: a pod roll that ends ok leaves the
// town's own values file at the pair the --images file names, written through
// the injected writer, so no overseer copies it by hand.
func TestUpdateTownValuesWritesOnAnOKPodRun(t *testing.T) {
	t.Parallel()
	c := newTownValuesCommand(t, okUpdateRun)

	code, stdout, stderr := runUpdateCommand(t, context.Background(), c.seams, c.podRunArgs()...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if c.record.calls != 1 {
		t.Fatalf("town values written %d times, want 1", c.record.calls)
	}
	if want := c.townValuesPath(); c.record.path != want {
		t.Errorf("writer path = %q, want %q", c.record.path, want)
	}
	if want := townValuesPair(); c.record.values != want {
		t.Errorf("writer values = %+v, want the loaded values %+v", c.record.values, want)
	}
	if !strings.Contains(stdout, "outcome: "+string(update.OutcomeOK)) {
		t.Errorf("report does not print the ok outcome:\n%s", stdout)
	}
}

// TestUpdateTownValuesSkipsOtherOutcomes: only an ok roll writes the town's
// file; every other ending leaves it as it was.
func TestUpdateTownValuesSkipsOtherOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome update.Outcome
		want    int
	}{
		{"partial", update.OutcomePartial, 3},
		{"failed", update.OutcomeFailed, 1},
		{"aborted", update.OutcomeAborted, 4},
		{"interrupted", update.OutcomeInterrupted, 130},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTownValuesCommand(t, func(context.Context, update.Deps, update.Options) (update.State, error) {
				return update.State{V: 1, RunID: "run-" + tc.name, Outcome: tc.outcome}, nil
			})

			code, _, stderr := runUpdateCommand(t, context.Background(), c.seams, c.podRunArgs()...)
			if code != tc.want {
				t.Fatalf("exit = %d for outcome %q, want %d; stderr:\n%s", code, tc.outcome, tc.want, stderr)
			}
			if c.record.calls != 0 {
				t.Errorf("town values written %d times for outcome %q, want none", c.record.calls, tc.outcome)
			}
		})
	}
}

// TestUpdateTownValuesSkipsRunsWithoutPodTargetOrImages: only a pod roll given
// --images has a pair to record, so every other run leaves the town's file
// alone. A dry run changes nothing, so it does not write it either.
func TestUpdateTownValuesSkipsRunsWithoutPodTargetOrImages(t *testing.T) {
	t.Parallel()

	t.Run("no pod target", func(t *testing.T) {
		t.Parallel()
		c := newTownValuesCommand(t, okUpdateRun)
		code, _, stderr := runUpdateCommand(t, context.Background(), c.seams, "--images", c.images)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
		}
		if c.record.calls != 0 {
			t.Errorf("town values written %d times without a pod target, want none", c.record.calls)
		}
	})

	t.Run("no --images", func(t *testing.T) {
		t.Parallel()
		c := newTownValuesCommand(t, okUpdateRun)
		code, _, stderr := runUpdateCommand(t, context.Background(), c.seams,
			"--pod-context", podSwapContext, "--pod-namespace", podSwapNamespace, "--skip", "install")
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
		}
		if c.record.calls != 0 {
			t.Errorf("town values written %d times without --images, want none", c.record.calls)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		t.Parallel()
		c := newTownValuesCommand(t, okUpdateRun)
		args := append(c.podRunArgs(), "--dry-run")
		code, _, stderr := runUpdateCommand(t, context.Background(), c.seams, args...)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
		}
		if c.record.calls != 0 {
			t.Errorf("town values written %d times on a dry run, want none", c.record.calls)
		}
	})
}

// TestUpdateTownValuesFailedWriteExitsNonZero: a write that fails is reported
// with its path and fails the command, and the run's recorded outcome is still
// the ok one it reported.
func TestUpdateTownValuesFailedWriteExitsNonZero(t *testing.T) {
	t.Parallel()
	c := newTownValuesCommand(t, func(context.Context, update.Deps, update.Options) (update.State, error) {
		return update.State{V: 1, RunID: "run-write", Outcome: update.OutcomeOK}, nil
	})
	c.record.err = errors.New("disk full")

	code, stdout, stderr := runUpdateCommand(t, context.Background(), c.seams, c.podRunArgs()...)
	if code != 1 {
		t.Fatalf("exit = %d for a failed town values write, want 1; stderr:\n%s", code, stderr)
	}
	if c.record.calls != 1 {
		t.Fatalf("town values written %d times, want the one failed write", c.record.calls)
	}
	if !strings.Contains(stderr, c.townValuesPath()) {
		t.Errorf("stderr does not name the path %s:\n%s", c.townValuesPath(), stderr)
	}
	if !strings.Contains(stderr, "disk full") {
		t.Errorf("stderr does not name the write failure:\n%s", stderr)
	}
	if !strings.Contains(stdout, "outcome: "+string(update.OutcomeOK)) {
		t.Errorf("a failed write changed the recorded outcome; report:\n%s", stdout)
	}
}

// TestUpdateTownValuesUnwiredSeamWritesThroughTheDefault: a command built
// without the writer still writes the town's copy, so a missing seam cannot
// quietly skip it.
func TestUpdateTownValuesUnwiredSeamWritesThroughTheDefault(t *testing.T) {
	t.Parallel()
	c := newTownValuesCommand(t, okUpdateRun)
	c.seams.WriteTownValues = nil

	code, _, stderr := runUpdateCommand(t, context.Background(), c.seams, c.podRunArgs()...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	got, err := imagevalues.Load(c.townValuesPath())
	if err != nil {
		t.Fatalf("the town's values file was not written: %v", err)
	}
	if want := townValuesPair(); got != want {
		t.Errorf("town values = %+v, want %+v", got, want)
	}
}

// TestUpdateTownValuesProductionWiresTheWriter: the production command writes
// the town's copy through imagevalues.Write.
func TestUpdateTownValuesProductionWiresTheWriter(t *testing.T) {
	t.Parallel()
	if productionUpdateSeams().WriteTownValues == nil {
		t.Error("production update seams wire no town values writer")
	}
}

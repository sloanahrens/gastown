package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// updateStatusTestNow is the fixed time a heartbeat's age is measured against.
var updateStatusTestNow = time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)

// updateStatusSeamsFor answers the given town root, or its error, at the fixed
// clock.
func updateStatusSeamsFor(townRoot string, err error) updateStatusSeams {
	return updateStatusSeams{
		FindTownRoot: func() (string, error) { return townRoot, err },
		Now:          func() time.Time { return updateStatusTestNow },
	}
}

// writeUpdateStatusState writes st through the real store under a fresh town
// root and returns that town root.
func writeUpdateStatusState(t *testing.T, st update.State) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := (&update.Store{Dir: update.Dir(townRoot)}).Write(st); err != nil {
		t.Fatalf("writing the fixture state: %v", err)
	}
	return townRoot
}

// runUpdateStatusCommand executes a fresh gt update status over seams and
// returns the status the production mapping gives its error, plus both streams.
func runUpdateStatusCommand(t *testing.T, seams updateStatusSeams, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	c := newUpdateStatusCmd(seams)
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	c.SetContext(context.Background())
	err := c.Execute()
	if err != nil {
		code = exitCodeForError(err)
	}
	return code, out.String(), errOut.String()
}

// runningUpdateStatusState is a run still working through its steps.
func runningUpdateStatusState() update.State {
	return update.State{
		RunID:       "20261103T195300Z-1a2b3c4d",
		Source:      update.SourceManual,
		StartedAt:   updateStatusTestNow.Add(-7 * time.Minute),
		HeartbeatAt: updateStatusTestNow.Add(-90 * time.Second),
		Target:      update.Target{SHA: "0123456789abcdef", InstalledSHA: "fedcba9876543210"},
		Current:     update.StepInstall,
		Outcome:     update.OutcomeRunning,
		Steps: []update.StepRecord{
			{Name: update.StepDrain, Result: update.ResultOK, Detail: "3 seats drained"},
			{Name: update.StepInstall, Result: update.ResultRunning, Detail: "make gt-installer"},
		},
	}
}

// TestUpdateStatusTextForARunningRun proves a running record prints its
// heartbeat age instead of an end, the truncated target and every step with its
// detail.
func TestUpdateStatusTextForARunningRun(t *testing.T) {
	t.Parallel()
	townRoot := writeUpdateStatusState(t, runningUpdateStatusState())

	code, stdout, stderr := runUpdateStatusCommand(t, updateStatusSeamsFor(townRoot, nil))
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	want := `run 20261103T195300Z-1a2b3c4d (manual): running
started 2026-11-03T19:53:00Z, last heartbeat 1m30s ago
target 01234567 (installed fedcba98)
  drain: ok - 3 seats drained
  install: running - make gt-installer
`
	if stdout != want {
		t.Errorf("status report:\n%s\nwant:\n%s", stdout, want)
	}
}

// TestUpdateStatusTextForAFinishedRun proves a finished record prints its end
// time instead of a heartbeat, and prints the hold line only for a hold it
// owns.
func TestUpdateStatusTextForAFinishedRun(t *testing.T) {
	t.Parallel()
	finished := func(hold update.HoldRecord) update.State {
		return update.State{
			RunID:     "20261103T195300Z-9f8e7d6c",
			Source:    update.SourceDaemon,
			StartedAt: updateStatusTestNow.Add(-9 * time.Minute),
			EndedAt:   updateStatusTestNow.Add(-2 * time.Minute),
			Target:    update.Target{SHA: "0123456789abcdef", InstalledSHA: "dev"},
			Outcome:   update.OutcomeOK,
			Hold:      hold,
			Steps: []update.StepRecord{
				{Name: update.StepVerify, Result: update.ResultOK},
			},
		}
	}

	t.Run("owned hold", func(t *testing.T) {
		t.Parallel()
		townRoot := writeUpdateStatusState(t, finished(update.HoldRecord{
			Owned: true,
			Note:  "gt update run 20261103T195300Z-9f8e7d6c",
		}))

		code, stdout, stderr := runUpdateStatusCommand(t, updateStatusSeamsFor(townRoot, nil))
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
		}
		want := `run 20261103T195300Z-9f8e7d6c (daemon): ok
ended 2026-11-03T19:58:00Z
target 01234567 (installed dev)
hold: gt update run 20261103T195300Z-9f8e7d6c
  verify: ok
`
		if stdout != want {
			t.Errorf("status report:\n%s\nwant:\n%s", stdout, want)
		}
	})

	t.Run("foreign hold", func(t *testing.T) {
		t.Parallel()
		// A hold someone else set is not the run's own, so it is not the run's
		// to report.
		townRoot := writeUpdateStatusState(t, finished(update.HoldRecord{
			Owned: false,
			Note:  "operator hold",
		}))

		code, stdout, stderr := runUpdateStatusCommand(t, updateStatusSeamsFor(townRoot, nil))
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
		}
		if strings.Contains(stdout, "hold:") {
			t.Errorf("foreign hold printed as the run's own:\n%s", stdout)
		}
		if !strings.Contains(stdout, "ended 2026-11-03T19:58:00Z\n") {
			t.Errorf("finished run did not print its end time:\n%s", stdout)
		}
	})
}

// TestUpdateStatusJSONRoundTrips proves --json prints the recorded state whole,
// so a reader can parse it back into the same State.
func TestUpdateStatusJSONRoundTrips(t *testing.T) {
	t.Parallel()
	fixture := runningUpdateStatusState()
	townRoot := writeUpdateStatusState(t, fixture)

	code, stdout, stderr := runUpdateStatusCommand(t, updateStatusSeamsFor(townRoot, nil), "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	var got update.State
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not the state JSON: %v\n%s", err, stdout)
	}
	want := fixture
	want.V = 1 // the store stamps the record version on write
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded state = %+v\nwant %+v", got, want)
	}
}

// TestUpdateStatusWithNoStateFile proves a town with no record is not an error,
// while a record that cannot be read is one.
func TestUpdateStatusWithNoStateFile(t *testing.T) {
	t.Parallel()

	t.Run("no record", func(t *testing.T) {
		t.Parallel()
		code, stdout, stderr := runUpdateStatusCommand(t, updateStatusSeamsFor(t.TempDir(), nil))
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
		}
		if stdout != "no gt update has run\n" {
			t.Errorf("stdout = %q, want the no-run line", stdout)
		}
	})

	t.Run("unreadable record", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		// A directory where state.json belongs makes the read fail with
		// something other than "no record".
		if err := os.MkdirAll(filepath.Join(update.Dir(townRoot), "state.json"), 0o700); err != nil {
			t.Fatalf("making state.json a directory: %v", err)
		}

		code, stdout, stderr := runUpdateStatusCommand(t, updateStatusSeamsFor(townRoot, nil))
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stdout:\n%s", code, stdout)
		}
		if strings.Contains(stdout, "no gt update has run") {
			t.Errorf("unreadable record reported as no run:\n%s", stdout)
		}
		if !strings.Contains(stderr, "reading the update state") {
			t.Errorf("stderr = %q, want the read error named", stderr)
		}
	})

	t.Run("version 2 record", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := update.Dir(townRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("creating the state directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"v": 2}`), 0o600); err != nil {
			t.Fatalf("writing the version-2 record: %v", err)
		}

		code, stdout, stderr := runUpdateStatusCommand(t, updateStatusSeamsFor(townRoot, nil))
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stdout:\n%s", code, stdout)
		}
		if strings.Contains(stdout, "no gt update has run") {
			t.Errorf("version-2 record reported as no run:\n%s", stdout)
		}
		if !strings.Contains(stderr, "unsupported version 2") {
			t.Errorf("stderr = %q, want the version named", stderr)
		}
	})
}

// TestUpdateStatusTownRootError proves a town lookup failure is reported, not
// read as a town with no record.
func TestUpdateStatusTownRootError(t *testing.T) {
	t.Parallel()
	seams := updateStatusSeamsFor("", errors.New("not in a town"))

	code, stdout, stderr := runUpdateStatusCommand(t, seams)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout:\n%s", code, stdout)
	}
	if !strings.Contains(stderr, "not in a town") {
		t.Errorf("stderr = %q, want the town lookup error named", stderr)
	}
}

// TestUpdateStatusIsAttachedToUpdate proves `gt update status` is reachable
// from the update command, not only as a standalone cobra command.
func TestUpdateStatusIsAttachedToUpdate(t *testing.T) {
	t.Parallel()
	parent := newUpdateCmdWith(updateTestSeams(t.TempDir(), updateTestDeps(t), nil))
	child, _, err := parent.Find([]string{"status"})
	if err != nil {
		t.Fatalf("finding the status subcommand: %v", err)
	}
	if child == parent {
		t.Fatal("gt update has no status subcommand")
	}
	if child.Name() != "status" {
		t.Errorf("subcommand name = %q, want status", child.Name())
	}
}

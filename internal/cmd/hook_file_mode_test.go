package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/taskfile"
)

// hookSeatFixture returns a task path whose directory is a fresh temp dir
// holding the file-mode marker and, when task is non-empty, TASK.md.
func hookSeatFixture(t *testing.T, task string) string {
	t.Helper()
	taskDir := t.TempDir()
	writeFile(t, filepath.Join(taskDir, taskfile.ModeFile), taskfile.ModeFiles)
	taskPath := filepath.Join(taskDir, "TASK.md")
	if task != "" {
		writeFile(t, taskPath, task)
	}
	return taskPath
}

// hookSeatDepsFor is the display's deps over a test writer and a bead status
// that records every run, so a file-mode display that reached the bead read is
// caught (gt-5d9lft).
func hookSeatDepsFor(t *testing.T, taskPath string) (hookSeatDeps, *strings.Builder, *int) {
	t.Helper()
	var out strings.Builder
	beadStatusRuns := 0
	deps := hookSeatDeps{
		taskPath: taskPath,
		out:      &out,
		beadStatus: func() error {
			beadStatusRuns++
			return nil
		},
	}
	return deps, &out, &beadStatusRuns
}

// A seat in file mode prints its task from TASK.md and runs no bead status; a
// dolt-mode seat still runs it, which is what makes the zero runs above mean
// something (gt-5d9lft).
func TestNoBeadAccessHookFileModePrintsFromTaskFile(t *testing.T) {
	t.Parallel()
	const task = "# TASK gt-5d9lft: file mode\n\n## Goal\n\nPrint this from TASK.md.\n"

	t.Run("file mode prints the task and runs no bead status", func(t *testing.T) {
		t.Parallel()
		taskPath := hookSeatFixture(t, task)
		deps, out, beadStatusRuns := hookSeatDepsFor(t, taskPath)

		if err := runHookStatusWith(deps); err != nil {
			t.Fatalf("runHookStatusWith = %v, want the task printed", err)
		}
		if got := out.String(); !strings.HasSuffix(got, task) {
			t.Errorf("hook output does not end with the TASK.md text %q:\n%s", task, got)
		}
		if !strings.Contains(out.String(), taskPath) {
			t.Errorf("hook output does not name %s, so a seat whose output was cut short cannot find the file:\n%s", taskPath, out.String())
		}
		if *beadStatusRuns != 0 {
			t.Errorf("file-mode hook ran the bead status %d time(s); it must read no bead", *beadStatusRuns)
		}
	})

	t.Run("a task without a final newline is printed whole", func(t *testing.T) {
		t.Parallel()
		taskPath := hookSeatFixture(t, "a task with no final newline")
		deps, out, _ := hookSeatDepsFor(t, taskPath)

		if err := runHookStatusWith(deps); err != nil {
			t.Fatalf("runHookStatusWith = %v, want the task printed", err)
		}
		if !strings.HasSuffix(out.String(), "a task with no final newline\n") {
			t.Errorf("hook output does not end with the task and a newline:\n%q", out.String())
		}
	})

	t.Run("dolt mode keeps the bead status", func(t *testing.T) {
		t.Parallel()
		taskDir := t.TempDir() // no file-mode marker
		deps, out, beadStatusRuns := hookSeatDepsFor(t, filepath.Join(taskDir, "TASK.md"))

		if err := runHookStatusWith(deps); err != nil {
			t.Fatalf("runHookStatusWith = %v, want the bead status", err)
		}
		if *beadStatusRuns != 1 {
			t.Errorf("dolt-mode hook ran the bead status %d time(s), want once", *beadStatusRuns)
		}
		if out.String() != "" {
			t.Errorf("dolt-mode hook printed %q of its own; the bead status is the whole display", out.String())
		}
	})
}

// A file-mode seat whose TASK.md is missing or unreadable is an error naming
// the path, never a fall back to the bead status (gt-5d9lft).
func TestNoBeadAccessHookFileModeMissingTaskNamesThePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, taskPath string)
	}{
		{name: "missing TASK.md", setup: func(*testing.T, string) {}},
		{name: "unreadable TASK.md", setup: func(t *testing.T, taskPath string) {
			// A directory at the task path is present but cannot be read as a file.
			if err := os.Mkdir(taskPath, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			taskPath := hookSeatFixture(t, "") // marker only
			tc.setup(t, taskPath)
			deps, out, beadStatusRuns := hookSeatDepsFor(t, taskPath)

			err := runHookStatusWith(deps)
			if err == nil {
				t.Fatalf("runHookStatusWith = nil, want an error naming %s", taskPath)
			}
			if !strings.Contains(err.Error(), taskPath) {
				t.Errorf("runHookStatusWith error = %q, want it to name %s", err, taskPath)
			}
			if !strings.Contains(err.Error(), "in file mode") {
				t.Errorf("runHookStatusWith error = %q, want it to say the seat is in file mode", err)
			}
			if *beadStatusRuns != 0 {
				t.Errorf("file-mode hook ran the bead status %d time(s) instead of failing; it must fall back to no bead", *beadStatusRuns)
			}
			if out.String() != "" {
				t.Errorf("runHookStatusWith printed %q over a missing task; it must print nothing", out.String())
			}
		})
	}
}

// A marker that is there but cannot be read leaves the mode unknown: the seat
// may have no route to Dolt, so the hook display refuses instead of reading a
// bead on the guess that it is dolt mode (gt-5d9lft).
func TestNoBeadAccessHookUnknownModeRefusesTheBeadStore(t *testing.T) {
	t.Parallel()
	taskDir := t.TempDir()
	// A directory at the marker's path is present but unreadable as a file.
	markerPath := filepath.Join(taskDir, taskfile.ModeFile)
	if err := os.Mkdir(markerPath, 0o755); err != nil {
		t.Fatal(err)
	}
	deps, _, beadStatusRuns := hookSeatDepsFor(t, filepath.Join(taskDir, "TASK.md"))

	err := runHookStatusWith(deps)
	if err == nil {
		t.Fatal("runHookStatusWith = nil over an unreadable marker; want the mode to be refused")
	}
	if !strings.Contains(err.Error(), "bead access is unknown") {
		t.Errorf("runHookStatusWith error = %q, want it to refuse the unknown mode", err)
	}
	if !strings.Contains(err.Error(), markerPath) {
		t.Errorf("runHookStatusWith error = %q, want it to name the marker %s", err, markerPath)
	}
	if *beadStatusRuns != 0 {
		t.Errorf("hook ran the bead status %d time(s) on an unknown mode; it must read no bead", *beadStatusRuns)
	}
}

// A task directory that cannot be reached at all (a parent that is a file) is
// a marker that cannot be read, not a seat the display may guess is in dolt
// mode: the mode is unknown, so the display refuses instead of opening the bead
// store (gt-5d9lft).
func TestNoBeadAccessHookUnreachableTaskDirectoryRefusesTheBeadStore(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	writeFile(t, parent, "a file where the task directory's parent should be")
	taskPath := filepath.Join(parent, "task", "TASK.md")
	markerPath := filepath.Join(parent, "task", taskfile.ModeFile)
	deps, out, beadStatusRuns := hookSeatDepsFor(t, taskPath)

	err := runHookStatusWith(deps)
	if err == nil {
		t.Fatal("runHookStatusWith over an unreachable task directory = nil; want the mode refused")
	}
	if !strings.Contains(err.Error(), "bead access is unknown") {
		t.Errorf("runHookStatusWith error = %q, want it to refuse the unknown mode", err)
	}
	if !strings.Contains(err.Error(), markerPath) {
		t.Errorf("runHookStatusWith error = %q, want it to name the marker %s", err, markerPath)
	}
	if *beadStatusRuns != 0 {
		t.Errorf("hook ran the bead status %d time(s) over an unreachable task directory; it must read no bead", *beadStatusRuns)
	}
	if out.String() != "" {
		t.Errorf("runHookStatusWith printed %q over an unreachable task directory; it must print nothing", out.String())
	}
}

// A file-mode seat cannot answer a request for another agent's hook or for
// JSON: both need the bead store, so each is refused with the path named,
// never answered with the seat's own task (gt-5d9lft).
func TestNoBeadAccessHookFileModeRefusesRequestsItCannotAnswer(t *testing.T) {
	t.Parallel()
	const task = "# TASK gt-5d9lft: file mode\n"

	cases := []struct {
		name string
		deps func(*hookSeatDeps)
		want string
	}{
		{name: "another agent's hook", deps: func(d *hookSeatDeps) { d.target = "gastown/polecats/other" }, want: "another agent's hook"},
		{name: "json", deps: func(d *hookSeatDeps) { d.json = true }, want: "as JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			taskPath := hookSeatFixture(t, task)
			deps, out, beadStatusRuns := hookSeatDepsFor(t, taskPath)
			tc.deps(&deps)

			err := runHookStatusWith(deps)
			if err == nil {
				t.Fatalf("runHookStatusWith = nil, want a refusal of the %s request", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("runHookStatusWith error = %q, want it to refuse the %s request", err, tc.name)
			}
			if !strings.Contains(err.Error(), taskPath) {
				t.Errorf("runHookStatusWith error = %q, want it to name %s", err, taskPath)
			}
			if *beadStatusRuns != 0 {
				t.Errorf("file-mode hook ran the bead status %d time(s); it must read no bead", *beadStatusRuns)
			}
			if out.String() != "" {
				t.Errorf("file-mode hook printed its own task over a refused request:\n%s", out.String())
			}
		})
	}
}

// A write failure while printing a file-mode task is returned, never swallowed:
// a run that printed nothing must not read as success (gt-5d9lft).
func TestNoBeadAccessHookFileModeReturnsTheWriteFailure(t *testing.T) {
	t.Parallel()
	taskPath := hookSeatFixture(t, "# TASK gt-5d9lft: file mode\n")
	beadStatusRuns := 0
	deps := hookSeatDeps{
		taskPath:   taskPath,
		out:        failingHookWriter{},
		beadStatus: func() error { beadStatusRuns++; return nil },
	}

	if err := runHookStatusWith(deps); err == nil {
		t.Fatal("runHookStatusWith = nil over a failed write; want the write error")
	}
	if beadStatusRuns != 0 {
		t.Errorf("hook ran the bead status %d time(s) over a failed write; it must read no bead", beadStatusRuns)
	}
}

// Each case drives the production deps builder over a test seat and a bead
// status that records every run, then the display, so a builder that dropped
// the target, the JSON flag or the bead status wires the display to the wrong
// seat and turns a row red (gt-5d9lft).
func TestNoBeadAccessHookSeatDepsDriveTheDisplay(t *testing.T) {
	t.Parallel()
	const task = "# TASK gt-5d9lft: file mode\n"

	cases := []struct {
		name string
		// fileMode builds the seat's task path with a marker and this task, or
		// without a marker when false.
		fileMode bool
		args     []string
		json     bool
		// wantErr is what the refusal must say; "" means the task is printed.
		wantErr      string
		wantBeadRuns int
	}{
		{name: "no argument prints the seat's task", fileMode: true},
		{name: "dolt mode runs the bead status", wantBeadRuns: 1},
		{name: "the target argument reaches the display", fileMode: true, args: []string{"gastown/polecats/other"}, wantErr: "another agent's hook"},
		{name: "the json flag reaches the display", fileMode: true, json: true, wantErr: "as JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var taskPath string
			if tc.fileMode {
				taskPath = hookSeatFixture(t, task)
			} else {
				taskPath = filepath.Join(t.TempDir(), "TASK.md") // no marker
			}
			var out strings.Builder
			beadStatusRuns := 0
			deps := hookSeatDisplayDeps(tc.args, taskPath, &out, tc.json, func() error {
				beadStatusRuns++
				return nil
			})

			err := runHookStatusWith(deps)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("runHookStatusWith = nil, want a refusal of the %s request", tc.name)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("runHookStatusWith error = %q, want it to refuse the %s request", err, tc.name)
				}
				if out.String() != "" {
					t.Errorf("runHookStatusWith printed %q over a refused request; it must print nothing", out.String())
				}
			} else {
				if err != nil {
					t.Fatalf("runHookStatusWith = %v, want the display to answer", err)
				}
				if tc.fileMode != strings.HasSuffix(out.String(), task) {
					t.Errorf("hook output (file mode %v) does not print the task as wired:\n%s", tc.fileMode, out.String())
				}
			}
			if beadStatusRuns != tc.wantBeadRuns {
				t.Errorf("hook ran the bead status %d time(s), want %d", beadStatusRuns, tc.wantBeadRuns)
			}
		})
	}
}

// failingHookWriter is a writer every write fails on.
type failingHookWriter struct{}

func (failingHookWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

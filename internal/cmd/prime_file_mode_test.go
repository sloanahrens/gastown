package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// noBeadAccessOpener records every open of the bead store. A file-mode seat has
// no route to Dolt, so the opens it makes must be none (gt-w1mzol).
type noBeadAccessOpener struct {
	t      *testing.T
	opened []string
}

func (o *noBeadAccessOpener) open(dir string) beads.Client {
	o.opened = append(o.opened, dir)
	return noBeadAccessClient{t: o.t}
}

// noBeadAccessClient fails the test on any bead read: a file-mode prime that
// reaches one is the regression the opener records.
type noBeadAccessClient struct {
	beads.Client
	t *testing.T
}

func (c noBeadAccessClient) Show(id string) (*beads.Issue, error) {
	c.t.Errorf("file-mode prime read bead %q; it must open no bead", id)
	return nil, nil
}

func (c noBeadAccessClient) ListAssignedIssueStatuses(assignee string, _ ...beads.IssueStatus) ([]*beads.Issue, error) {
	c.t.Errorf("file-mode prime listed %q's beads; it must open no bead", assignee)
	return nil, nil
}

// A seat in file mode prints its task from TASK.md and opens no bead store; a
// dolt-mode seat still reads its hooked bead through the same door, which is
// what makes the zero opens above mean something (gt-w1mzol).
func TestNoBeadAccessPrimeFileModePrintsFromTaskFile(t *testing.T) {
	t.Parallel()
	const task = "# TASK gt-w1mzol: file mode\n\n## Goal\n\nPrint this from TASK.md.\n"
	ctx := RoleContext{Role: RolePolecat, Rig: "gastown", Polecat: "dag"}

	t.Run("file mode prints the task and opens no bead", func(t *testing.T) {
		t.Parallel()
		taskDir := t.TempDir()
		writeFile(t, filepath.Join(taskDir, taskfile.ModeFile), taskfile.ModeFiles)
		taskPath := filepath.Join(taskDir, "TASK.md")
		writeFile(t, taskPath, task)

		opener := &noBeadAccessOpener{t: t}
		work, err := primeSeatWork(ctx, primeSeatDeps{taskPath: taskPath, open: opener.open, clock: fakeSeatClock()})
		if err != nil {
			t.Fatalf("primeSeatWork = %v, want the TASK.md text", err)
		}
		if !work.fileMode || work.task != task {
			t.Fatalf("primeSeatWork = %+v, want the TASK.md text in file mode", work)
		}
		if len(opener.opened) != 0 {
			t.Errorf("file-mode prime opened the bead store at %q; it must open none", opener.opened)
		}

		out := primeFileModePayload(ctx, work, "", false).render(0)
		if !strings.Contains(out, task) {
			t.Errorf("prime output does not carry the TASK.md text:\n%s", out)
		}
		if strings.Contains(out, "bd show") {
			t.Errorf("prime output tells the seat to run bd show, which file mode cannot do:\n%s", out)
		}
	})

	t.Run("dolt mode keeps the bead lookup", func(t *testing.T) {
		t.Parallel()
		taskDir := t.TempDir() // no file-mode marker
		opened := 0
		open := func(string) beads.Client {
			opened++
			return hookWorkBD{
				show: func(string) (*beads.Issue, error) { return nil, errors.New("no issue found") },
				assigned: func() ([]*beads.Issue, error) {
					return []*beads.Issue{{ID: "gt-w1mzol", Status: beads.StatusHooked}}, nil
				},
			}
		}
		clock := fakeSeatClock()

		work, err := primeSeatWork(ctx, primeSeatDeps{taskPath: filepath.Join(taskDir, "TASK.md"), open: open, clock: clock})
		if err != nil {
			t.Fatalf("primeSeatWork = %v, want the hooked bead", err)
		}
		if work.fileMode || work.bead == nil || work.bead.ID != "gt-w1mzol" {
			t.Fatalf("primeSeatWork = %+v, want the hooked bead", work)
		}
		if opened == 0 {
			t.Error("dolt-mode prime opened no bead store; the door must be the one the opener records")
		}
		// The retry sleeps on the injected clock, never the host's: a first
		// attempt that answers has nothing to wait for.
		if len(clock.slept) != 0 {
			t.Errorf("dolt-mode prime slept %v; the first attempt answered", clock.slept)
		}
	})
}

// A file-mode seat whose TASK.md is missing or unreadable is an error naming
// the path, never a fall back to the bead store (gt-w1mzol).
func TestNoBeadAccessPrimeFileModeMissingTaskNamesThePath(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RolePolecat, Rig: "gastown", Polecat: "dag"}

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
			taskDir := t.TempDir()
			writeFile(t, filepath.Join(taskDir, taskfile.ModeFile), taskfile.ModeFiles)
			taskPath := filepath.Join(taskDir, "TASK.md")
			tc.setup(t, taskPath)

			opener := &noBeadAccessOpener{t: t}
			work, err := primeSeatWork(ctx, primeSeatDeps{taskPath: taskPath, open: opener.open, clock: fakeSeatClock()})
			if err == nil {
				t.Fatalf("primeSeatWork = nil, want an error naming %s", taskPath)
			}
			if !strings.Contains(err.Error(), taskPath) {
				t.Errorf("primeSeatWork error = %q, want it to name %s", err, taskPath)
			}
			if len(opener.opened) != 0 {
				t.Errorf("file-mode prime opened the bead store at %q instead of failing; it must fall back to no bead", opener.opened)
			}
			// The refusal rides the file-mode path, so prime hands the error
			// back as the run's error instead of printing an empty task.
			if !work.fileMode {
				t.Fatalf("primeSeatWork = %+v, want the refusal on the file-mode path", work)
			}
			if got := runPrimeFileMode(io.Discard, ctx, work, err); !errors.Is(got, err) {
				t.Errorf("runPrimeFileMode = %v, want the seat's own refusal %v", got, err)
			}
		})
	}
}

// A marker that is there but cannot be read leaves the mode unknown: the seat
// may have no route to Dolt, so prime refuses instead of opening the bead store
// on the guess that it is dolt mode (gt-w1mzol).
func TestNoBeadAccessPrimeUnknownModeRefusesTheBeadStore(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RolePolecat, Rig: "gastown", Polecat: "dag"}
	taskDir := t.TempDir()
	// A directory at the marker's path is present but unreadable as a file.
	if err := os.Mkdir(filepath.Join(taskDir, taskfile.ModeFile), 0o755); err != nil {
		t.Fatal(err)
	}

	opener := &noBeadAccessOpener{t: t}
	work, err := primeSeatWork(ctx, primeSeatDeps{taskPath: filepath.Join(taskDir, "TASK.md"), open: opener.open, clock: fakeSeatClock()})
	if err == nil {
		t.Fatal("primeSeatWork = nil over an unreadable marker; want the mode to be refused")
	}
	if !work.fileMode {
		t.Fatalf("primeSeatWork = %+v, want the refusal on the file-mode path so prime returns it", work)
	}
	if !strings.Contains(err.Error(), "bead access is unknown") {
		t.Errorf("primeSeatWork error = %q, want it to refuse the unknown mode", err)
	}
	if want := filepath.Join(taskDir, taskfile.ModeFile); !strings.Contains(err.Error(), want) {
		t.Errorf("primeSeatWork error = %q, want it to name the marker %s", err, want)
	}
	if len(opener.opened) != 0 {
		t.Errorf("prime opened the bead store at %q on an unknown mode; it must open none", opener.opened)
	}
}

// fakeSeatClock is a clock whose Sleep returns at once and records the wait, so
// the dolt-mode retry never touches the host's clock.
func fakeSeatClock() *sleepRecorder {
	return &sleepRecorder{Clock: clockwork.NewFakeClockAt(time.Unix(0, 0))}
}

// A file-mode run that cannot read the role directives fails before printing,
// as a dolt-mode run does: the seat must not start without a policy it was
// meant to have (gt-w1mzol).
func TestNoBeadAccessPrimeFileModeUnreadableRoleDirectives(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RolePolecat, Rig: "gastown", Polecat: "dag", TownRoot: t.TempDir()}
	// A directory at the shared directive's path is present but unreadable as a
	// file, so the render fails rather than starting the seat without it.
	if err := os.MkdirAll(filepath.Join(ctx.TownRoot, "directives", "_common.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	work := primeWork{fileMode: true, task: "task\n", taskPath: filepath.Join(t.TempDir(), "TASK.md")}
	if err := runPrimeFileMode(io.Discard, ctx, work, nil); err == nil {
		t.Fatal("runPrimeFileMode = nil over an unreadable directive; want the render's refusal")
	}
}

// A write failure while printing a file-mode task is returned, never swallowed:
// a run that printed nothing must not read as success (gt-w1mzol).
func TestNoBeadAccessPrimeFileModeReturnsTheWriteFailure(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RolePolecat, Rig: "gastown", Polecat: "dag", TownRoot: t.TempDir()}
	work := primeWork{fileMode: true, task: "task\n", taskPath: filepath.Join(t.TempDir(), "TASK.md")}
	err := runPrimeFileMode(failingPrimeWriter{}, ctx, work, nil)
	if err == nil {
		t.Fatal("runPrimeFileMode = nil over a failed write; want the write error")
	}
}

// failingPrimeWriter is a writer every write fails on.
type failingPrimeWriter struct{}

func (failingPrimeWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// gt prime's whole run path — not only the seat read it delegates to — prints a
// file-mode seat's task from TASK.md and opens no bead store; a missing TASK.md
// on that path is an error naming the path. Deleting the file-mode branch in
// runPrimeWith turns both rows red: the dolt path opens deps.open and prints no
// task (gt-w1mzol).
//
//testpolicy:allow parallel — the test sets GT_CALLER_CWD, GT_ROLE and prime's dry-run and session-ID flag variables, which the whole process shares
func TestNoBeadAccessPrimeRunPathFileMode(t *testing.T) {
	townRoot := t.TempDir()
	writeTownRoot(t, townRoot)
	worktree := filepath.Join(townRoot, "gastown", "polecats", "dag", "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	writeGitMetadata(t, worktree)
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test: which seat this run answers for
	t.Setenv(callercwd.EnvCallerCwd, worktree)
	//testpolicy:allow no-env — GT_ROLE would override the role this run must detect from the worktree
	t.Setenv("GT_ROLE", "")
	wasDryRun := primeDryRun
	//testpolicy:allow no-global-swap — runPrime reads the --dry-run flag variable; it is not an argument
	primeDryRun = true
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the flag variable swapped above
		primeDryRun = wasDryRun
	})
	wasHookSession := primeHookSessionID
	//testpolicy:allow no-global-swap — runPrime reads the session ID the hook would set; it is not an argument
	primeHookSessionID = "w1mzol-run"
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the session ID swapped above
		primeHookSessionID = wasHookSession
	})

	const task = "# TASK gt-w1mzol\n\nPrint this from TASK.md.\n"
	cases := []struct {
		name      string
		writeTask bool
		wantErr   bool
	}{
		{name: "prints the task from TASK.md and opens no bead", writeTask: true},
		{name: "a missing TASK.md is an error naming the path", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taskDir := filepath.Join(t.TempDir(), "task")
			if err := os.Mkdir(taskDir, 0o755); err != nil {
				t.Fatalf("mkdir task directory: %v", err)
			}
			writeFile(t, filepath.Join(taskDir, taskfile.ModeFile), taskfile.ModeFiles)
			taskPath := filepath.Join(taskDir, "TASK.md")
			if tc.writeTask {
				writeFile(t, taskPath, task)
			}

			var out strings.Builder
			opener := &noBeadAccessOpener{t: t}
			err := runPrimeWith(&cobra.Command{}, primeSeatDeps{
				taskPath: taskPath,
				open:     opener.open,
				clock:    fakeSeatClock(),
				out:      &out,
			})
			if len(opener.opened) != 0 {
				t.Errorf("file-mode prime opened the bead store at %q; it must open none", opener.opened)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("runPrime in file mode = nil, want an error naming %s", taskPath)
				}
				if !strings.Contains(err.Error(), taskPath) {
					t.Errorf("runPrime error = %q, want it to name %s", err, taskPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("runPrime in file mode = %v, want the task printed", err)
			}
			if !strings.Contains(out.String(), task) {
				t.Errorf("prime output does not carry the TASK.md text:\n%s", out.String())
			}
			// The hooked-bead framing tells the seat to look its bead up; a
			// file-mode seat has none to look up. (The role text may name bd
			// show as a command in general, so match the instruction itself.)
			if strings.Contains(out.String(), "IMMEDIATELY run: `bd show") {
				t.Errorf("prime printed the hooked-bead instruction, which file mode cannot follow:\n%s", out.String())
			}
		})
	}
}

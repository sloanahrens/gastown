package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// TestRunawayBeadAddedLinesReadsTheSeatsHandle: the production seam opens the
// seat's git handle through overrunSeatGit, the same selection the save, dirty
// check, push and reset run over, and answers the lines that handle adds over
// origin/main. A host seat is counted through its host worktree's handle and a
// sandbox seat through the handle on its clone, so a seam that skipped the
// selection, or picked the other worktree, reports something other than the
// 120 the seat's own handle counts (gt-4t7h8m).
func TestRunawayBeadAddedLinesReadsTheSeatsHandle(t *testing.T) {
	t.Parallel()
	t.Run("host seat", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		seedHostWorktreeStub(t, d)
		handle := &addedLinesStubGit{lines: 120}
		var opened []string
		d.openGitFn = func(dir string) daemonGit {
			opened = append(opened, dir)
			return handle
		}

		got, err := d.overrunProdSeams().addedLines(stopSeat)
		if err != nil {
			t.Fatalf("addedLines = (%d, %v), want the host worktree handle's count", got, err)
		}
		if got != 120 {
			t.Errorf("addedLines = %d, want 120 from the host worktree handle", got)
		}
		wantDir := filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
		if want := []string{wantDir}; !slices.Equal(opened, want) {
			t.Errorf("host git opened for %q, want the seat's worktree %q", opened, want)
		}
		if want := []string{"origin/main"}; !slices.Equal(handle.bases, want) {
			t.Errorf("handle counted added lines over %q, want %q", handle.bases, want)
		}
	})
	t.Run("sandbox seat", func(t *testing.T) {
		t.Parallel()
		const clone = "/sandbox/gt/gastown/polecats/capable/gastown"
		f := newOverrunSeatGitFixture(t, overrunStopSandboxRecord(clone), nil, "")
		var calls []overrunSandboxGitCall
		f.d.sandboxGitRunnerFn = func(string, session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
			return func(dir string, args ...string) ([]byte, error) {
				calls = append(calls, overrunSandboxGitCall{dir: dir, args: append([]string(nil), args...)})
				switch {
				case len(args) > 0 && args[0] == "diff":
					return []byte("120\t0\tinternal/foo.go\n"), nil
				case len(args) > 0 && args[0] == "ls-files":
					return nil, nil
				}
				return nil, fmt.Errorf("unexpected in-sandbox git call %q", args)
			}, nil
		}

		got, err := f.d.overrunProdSeams().addedLines(stopSeat)
		if err != nil {
			t.Fatalf("addedLines = (%d, %v), want the sandbox clone handle's count", got, err)
		}
		if got != 120 {
			t.Errorf("addedLines = %d, want 120 from the sandbox clone handle", got)
		}
		if len(calls) == 0 {
			t.Fatal("no in-sandbox git call, so the count did not come from the seat's clone")
		}
		for _, c := range calls {
			if c.dir != clone {
				t.Errorf("in-sandbox git ran in %q, want the seat's clone %q", c.dir, clone)
			}
		}
	})
}

// TestRunawayBeadAddedLinesRefusesAHandleItCannotBuild: when the seat's handle
// cannot be opened, or the handle it returns cannot count, the production seam
// returns the error and no count. A zero with a nil error would read as a seat
// that added nothing (gt-4t7h8m).
func TestRunawayBeadAddedLinesRefusesAHandleItCannotBuild(t *testing.T) {
	t.Parallel()
	errOpen := errors.New("git: cannot open the seat's repository")
	for _, tc := range []struct {
		name   string
		handle daemonGit
		err    error
	}{
		{name: "no worktree", err: errOverrunNoWorktree},
		{name: "handle cannot be opened", err: errOpen},
		{name: "handle cannot count", handle: &pushStubGit{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			d.overrunSeatGitFn = func(overrunSeat) (daemonGit, error) {
				return tc.handle, tc.err
			}

			got, err := d.overrunProdSeams().addedLines(stopSeat)
			if err == nil {
				t.Fatalf("addedLines = (%d, nil), want an error and no count", got)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("addedLines error = %v, want the handle seam's %v", err, tc.err)
			}
			if got != 0 {
				t.Errorf("addedLines = %d with an error; want 0, never a count", got)
			}
		})
	}
}

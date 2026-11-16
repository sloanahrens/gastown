package daemon

import (
	"errors"
	"slices"
	"testing"
)

// TestRunawayBeadAddedLinesReadsTheSeatsHandle: the production seam opens the
// seat's git handle through overrunSeatGit and answers the lines it adds over
// origin/main. The fake handle counts 120, so a seam that reported anything
// else (a zero, another worktree's count) fails (gt-4t7h8m).
func TestRunawayBeadAddedLinesReadsTheSeatsHandle(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	handle := &addedLinesStubGit{lines: 120}
	var asked []overrunSeat
	d.overrunSeatGitFn = func(seat overrunSeat) (daemonGit, error) {
		asked = append(asked, seat)
		return handle, nil
	}

	got, err := d.overrunProdSeams().addedLines(stopSeat)
	if err != nil {
		t.Fatalf("addedLines = (%d, %v), want the seat's handle count", got, err)
	}
	if got != 120 {
		t.Errorf("addedLines = %d, want 120 from the seat's handle", got)
	}
	if len(asked) != 1 || asked[0] != stopSeat {
		t.Errorf("handle opened for %+v, want one for the seat %+v", asked, stopSeat)
	}
	if want := []string{"origin/main"}; !slices.Equal(handle.bases, want) {
		t.Errorf("handle counted added lines over %q, want %q", handle.bases, want)
	}
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

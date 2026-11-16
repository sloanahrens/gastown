package daemon

import (
	"errors"
	"testing"
)

// addedLinesStubGit is the handle a labeled seat handle wraps in these tests:
// it answers AddedLinesSince with a fixed count and records the bases it was
// asked, so a test can tell the count came through the label.
type addedLinesStubGit struct {
	daemonGit
	lines int
	err   error
	bases []string
}

func (g *addedLinesStubGit) AddedLinesSince(base string) (int, error) {
	g.bases = append(g.bases, base)
	return g.lines, g.err
}

// TestRunawayBeadLabeledSeatHandleForwardsAddedLinesSince: a labeled seat
// handle whose wrapped Git counts 120 lines answers 120 through
// overrunSeatAddedLines, asking the wrapped handle for the caller's base
// (gt-3y4bol).
func TestRunawayBeadLabeledSeatHandleForwardsAddedLinesSince(t *testing.T) {
	t.Parallel()
	const base = "origin/main"
	for _, tc := range []struct {
		name  string
		label func(daemonGit) daemonGit
	}{
		{name: "host handle", label: labelOverrunHostGit},
		{name: "sandbox handle", label: labelOverrunSandboxGit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrapped := &addedLinesStubGit{lines: 120}
			got, err := overrunSeatAddedLines(tc.label(wrapped), base)
			if err != nil {
				t.Fatalf("overrunSeatAddedLines = (%d, %v), want the wrapped handle's count", got, err)
			}
			if got != 120 {
				t.Errorf("overrunSeatAddedLines = %d, want 120 through the labeled handle", got)
			}
			if len(wrapped.bases) != 1 || wrapped.bases[0] != base {
				t.Errorf("wrapped handle asked for bases %q, want one %q", wrapped.bases, base)
			}
		})
	}
}

// TestRunawayBeadLabeledSeatHandleRefusesAddedLinesItCannotCount: a handle
// that cannot count added lines returns an error and no count, never a zero a
// caller would read as a seat that added nothing (gt-3y4bol).
func TestRunawayBeadLabeledSeatHandleRefusesAddedLinesItCannotCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		label func(daemonGit) daemonGit
	}{
		{name: "host handle", label: labelOverrunHostGit},
		{name: "sandbox handle", label: labelOverrunSandboxGit},
		{name: "unlabeled handle", label: func(g daemonGit) daemonGit { return g }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := overrunSeatAddedLines(tc.label(&pushStubGit{}), "origin/main")
			if err == nil {
				t.Fatalf("overrunSeatAddedLines = %d, nil; want an error for a handle that cannot count added lines", got)
			}
			if got != 0 {
				t.Errorf("overrunSeatAddedLines = %d with an error; want 0, never a count", got)
			}
		})
	}
}

// TestRunawayBeadLabeledSeatHandleKeepsTheWrappedCountsError: the wrapped
// handle's own failure reaches the caller and comes with no count, so a failed
// git command is never read as a seat that added nothing (gt-3y4bol).
func TestRunawayBeadLabeledSeatHandleKeepsTheWrappedCountsError(t *testing.T) {
	t.Parallel()
	errDiff := errors.New("git diff --numstat: exit status 128")
	wrapped := &addedLinesStubGit{err: errDiff}
	got, err := overrunSeatAddedLines(labelOverrunHostGit(wrapped), "origin/main")
	if !errors.Is(err, errDiff) {
		t.Fatalf("overrunSeatAddedLines = (%d, %v), want the wrapped handle's %v", got, err, errDiff)
	}
	if got != 0 {
		t.Errorf("overrunSeatAddedLines = %d with an error; want 0, never a count", got)
	}
}

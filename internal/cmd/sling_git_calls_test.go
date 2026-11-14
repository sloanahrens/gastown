package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

func TestSlingNoteShowsGitCallsOnTheStepThatMadeThem(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000_000, 0)
	now := func() time.Time { return clock }
	trace := session.NewSandboxTrace(now)
	// The spawn sites hand trace.CountGit to git.WithRunObserver, so the
	// callback here is the one a git process calls.
	observe := trace.CountGit

	if err := trace.Phase("seat decision", func() error {
		clock = clock.Add(time.Second)
		observe()
		observe()
		return nil
	}); err != nil {
		t.Fatalf("seat decision phase: %v", err)
	}
	if err := trace.Phase("worktree", func() error {
		clock = clock.Add(2 * time.Second)
		observe()
		return nil
	}); err != nil {
		t.Fatalf("worktree phase: %v", err)
	}

	want := "sling gt-a seat box: seat decision 1s [bd 0, git 2], worktree 2s [bd 0, git 1], total 3s; outcome: started"
	if got := specSlingTraceLine("gt-a", "box", trace, nil); got != want {
		t.Fatalf("specSlingTraceLine() = %q, want %q: each step shows the git calls it made", got, want)
	}
}

func TestSlingCallsOfTwoSlingsCountSeparately(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000_000, 0)
	now := func() time.Time { return clock }
	first := session.NewSandboxTrace(now)
	second := session.NewSandboxTrace(now)
	observeFirst, observeSecond := first.CountGit, second.CountGit

	// The two slings' calls interleave, as two dispatches running together do.
	observeFirst()
	observeSecond()
	observeFirst()
	observeFirst()
	for _, tr := range []*session.SandboxTrace{first, second} {
		if err := tr.Phase("seat", func() error { return nil }); err != nil {
			t.Fatalf("seat phase: %v", err)
		}
	}

	if got, want := first.Summary(), "seat 0s [bd 0, git 3], total 0s"; got != want {
		t.Fatalf("first sling Summary() = %q, want %q", got, want)
	}
	if got, want := second.Summary(), "seat 0s [bd 0, git 1], total 0s"; got != want {
		t.Fatalf("second sling Summary() = %q, want %q: the other sling's calls must not land here", got, want)
	}
}

func TestSlingCallsNilTraceCountsNothingAndDoesNotPanic(t *testing.T) {
	t.Parallel()
	var trace *session.SandboxTrace
	// A spawn with no trace (a hand-run gt sling) passes this callback.
	observe := trace.CountGit
	observe()
	observe()

	if got := trace.Summary(); got != "" {
		t.Fatalf("nil trace Summary() = %q, want empty: a sling with no trace logs nothing", got)
	}
}

package cmd

import "testing"

// TestDrainSessionNudgesNoTmuxSession verifies drainSessionNudges degrades to
// a no-op (nil, no error) when the caller isn't inside a tmux pane, which is
// the path any non-interactive test process takes. It must not shell out or
// fail just because TMUX_PANE is unset.
func TestDrainSessionNudgesNoTmuxSession(t *testing.T) {
	t.Setenv("TMUX_PANE", "")

	got := drainSessionNudges(t.TempDir())
	if got != nil {
		t.Errorf("expected nil with no tmux pane, got %v", got)
	}
}

// TestNudgeInjectionOutputNoTmuxSession verifies the shared helper used by
// gt mq list, gt mq next, and gt mail inbox (gt-dekkl) degrades to an empty
// string — never a panic or shell-out — when the caller isn't inside a tmux
// pane, same as drainSessionNudges itself.
func TestNudgeInjectionOutputNoTmuxSession(t *testing.T) {
	t.Setenv("TMUX_PANE", "")

	got := nudgeInjectionOutput(t.TempDir())
	if got != "" {
		t.Errorf("expected empty output with no tmux pane, got %q", got)
	}
}

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

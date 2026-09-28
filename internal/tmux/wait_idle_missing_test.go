package tmux

import (
	"errors"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// TestWaitForIdle_VanishedSessionEndsWait: real tmux answers capture-pane on
// a missing target with "can't find pane: X" (tmux 3.7c), not "can't find
// session". WaitForIdle must treat that as the session being gone and return
// at once instead of polling out its whole timeout.
func TestWaitForIdle_VanishedSessionEndsWait(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"capture-pane": fail("can't find pane: gt-x")}))
	tm := unitTmux(s, clockwork.NewFakeClock())
	err := returnsWithoutClock(t, func() error { return tm.WaitForIdle("gt-x", time.Minute) })
	if !errors.Is(err, ErrPaneNotFound) {
		t.Fatalf("WaitForIdle(missing) = %v, want ErrPaneNotFound", err)
	}
}

// TestWrapErrorPaneNotFoundKeepsTmuxText: the typed error still carries
// tmux's own words, which callers such as crew_at match on.
func TestWrapErrorPaneNotFoundKeepsTmuxText(t *testing.T) {
	t.Parallel()
	err := unitTmux(newScripted(nil), nil).wrapError(exitError(1), "can't find pane: %5", []string{"respawn-pane"})
	if !errors.Is(err, ErrPaneNotFound) {
		t.Fatalf("err = %v, want ErrPaneNotFound", err)
	}
	if got := err.Error(); got != "tmux respawn-pane: can't find pane: %5" {
		t.Fatalf("err text = %q", got)
	}
}

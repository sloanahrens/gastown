//go:build integration

package tmux

import (
	"errors"
	"testing"
	"time"
)

// TestIntegrationWaitForIdle_MissingSessionReturnsPromptly pins the vanished-
// session exit against real tmux: it must not poll out the timeout.
func TestIntegrationWaitForIdle_MissingSessionReturnsPromptly(t *testing.T) {
	tm := newTestTmux(t)
	start := time.Now()
	err := tm.WaitForIdle("gt-test-idle-missing-"+t.Name(), 30*time.Second)
	if !errors.Is(err, ErrPaneNotFound) && !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("WaitForIdle(missing) = %v, want a not-found error", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("WaitForIdle(missing) took %s: it polled out the timeout", elapsed)
	}
}

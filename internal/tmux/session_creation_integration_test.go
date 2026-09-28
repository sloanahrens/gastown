//go:build integration

package tmux

import "testing"

// TestIntegrationNewSessionWithCommand_Concurrent verifies multiple sessions
// can be created concurrently on one real server without errors.
func TestIntegrationNewSessionWithCommand_Concurrent(t *testing.T) {
	tm := newTestTmux(t)
	n := 5
	base := "gt-test-concurrent-"

	for i := 0; i < n; i++ {
		_ = tm.KillSession(base + string(rune('a'+i)))
	}
	defer func() {
		for i := 0; i < n; i++ {
			_ = tm.KillSession(base + string(rune('a'+i)))
		}
	}()

	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			errs <- tm.NewSessionWithCommand(base+string(rune('a'+idx)), "", "sleep 5")
		}(i)
	}

	var failures int
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			failures++
			t.Logf("concurrent create %d: %v", i, err)
		}
	}
	if failures > 0 {
		t.Errorf("%d/%d concurrent session creations failed", failures, n)
	}
}

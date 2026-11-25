package daemon

import "testing"

func TestEscalationCloseTick(t *testing.T) {
	t.Parallel()

	t.Run("not shutting down runs the trigger once", func(t *testing.T) {
		t.Parallel()
		triggers := 0
		escalationCloseTick(func() bool { return false }, func() { triggers++ })
		if triggers != 1 {
			t.Fatalf("trigger ran %d times, want 1 when the daemon is not shutting down", triggers)
		}
	})

	t.Run("shutting down runs no trigger", func(t *testing.T) {
		t.Parallel()
		triggers := 0
		escalationCloseTick(func() bool { return true }, func() { triggers++ })
		if triggers != 0 {
			t.Fatalf("trigger ran %d times, want 0 while the daemon is shutting down", triggers)
		}
	})
}

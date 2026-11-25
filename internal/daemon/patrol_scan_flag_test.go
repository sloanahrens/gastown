package daemon

import (
	"sync/atomic"
	"testing"
)

func TestRunUnderPatrolScanFlag(t *testing.T) {
	t.Parallel()

	t.Run("free flag runs the func, returns true and releases the flag", func(t *testing.T) {
		t.Parallel()
		var flag atomic.Bool
		runs := 0
		if got := runUnderPatrolScanFlag(&flag, func() {
			runs++
			if !flag.Load() {
				t.Errorf("flag = false while run executes, want held")
			}
		}); !got {
			t.Fatalf("runUnderPatrolScanFlag = false, want true for a free flag")
		}
		if runs != 1 {
			t.Fatalf("run executed %d times, want 1", runs)
		}
		if flag.Load() {
			t.Fatalf("flag still held after run returned, want released")
		}
	})

	t.Run("held flag skips the func, returns false and leaves the flag held", func(t *testing.T) {
		t.Parallel()
		var flag atomic.Bool
		flag.Store(true)
		runs := 0
		if got := runUnderPatrolScanFlag(&flag, func() { runs++ }); got {
			t.Fatalf("runUnderPatrolScanFlag = true, want false for a held flag")
		}
		if runs != 0 {
			t.Fatalf("run executed %d times under a held flag, want 0", runs)
		}
		if !flag.Load() {
			t.Fatalf("flag released by a caller that never took it, want still held")
		}
	})

	t.Run("panic in the func still releases the flag and propagates", func(t *testing.T) {
		t.Parallel()
		var flag atomic.Bool
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			runUnderPatrolScanFlag(&flag, func() { panic("boom") })
		}()
		if recovered != "boom" {
			t.Fatalf("recovered = %v, want the panic to propagate as \"boom\"", recovered)
		}
		if flag.Load() {
			t.Fatalf("flag still held after a panic, want released")
		}
	})
}

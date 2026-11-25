package daemon

import (
	"fmt"
	"sync/atomic"
	"testing"
)

func TestRunEscalationClosePass(t *testing.T) {
	t.Parallel()

	const skipLine = "escalation_close: skipped, a patrol_scan tick is running"

	t.Run("patrol active and flag free runs the pass, returns true and releases the flag", func(t *testing.T) {
		t.Parallel()
		var flag atomic.Bool
		runs := 0
		var logs []string
		got := runEscalationClosePass(&flag,
			func() bool { return true },
			func() {
				runs++
				if !flag.Load() {
					t.Errorf("flag = false while the pass runs, want held")
				}
			},
			func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		)
		if !got {
			t.Fatalf("runEscalationClosePass = false, want true for an active patrol and a free flag")
		}
		if runs != 1 {
			t.Fatalf("pass ran %d times, want 1", runs)
		}
		if flag.Load() {
			t.Fatalf("flag still held after the pass, want released")
		}
		if len(logs) != 0 {
			t.Fatalf("logged %q, want nothing", logs)
		}
	})

	t.Run("patrol inactive runs nothing, logs nothing and returns false", func(t *testing.T) {
		t.Parallel()
		var flag atomic.Bool
		runs := 0
		var logs []string
		got := runEscalationClosePass(&flag,
			func() bool { return false },
			func() { runs++ },
			func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		)
		if got {
			t.Fatalf("runEscalationClosePass = true, want false for an inactive patrol")
		}
		if runs != 0 {
			t.Fatalf("pass ran %d times for an inactive patrol, want 0", runs)
		}
		if len(logs) != 0 {
			t.Fatalf("logged %q for an inactive patrol, want nothing", logs)
		}
		if flag.Load() {
			t.Fatalf("flag taken by an inactive patrol, want free")
		}
	})

	t.Run("flag held logs the skip, runs nothing and returns false", func(t *testing.T) {
		t.Parallel()
		var flag atomic.Bool
		flag.Store(true)
		runs := 0
		var logs []string
		got := runEscalationClosePass(&flag,
			func() bool { return true },
			func() { runs++ },
			func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		)
		if got {
			t.Fatalf("runEscalationClosePass = true, want false for a held flag")
		}
		if runs != 0 {
			t.Fatalf("pass ran %d times under a held flag, want 0", runs)
		}
		if len(logs) != 1 || logs[0] != skipLine {
			t.Fatalf("logs = %q, want exactly [%q]", logs, skipLine)
		}
		if !flag.Load() {
			t.Fatalf("flag released by a caller that never took it, want still held")
		}
	})
}

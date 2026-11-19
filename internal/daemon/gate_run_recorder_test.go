package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// recorderLog captures the recorder's log lines. The recorder logs on the
// goroutine the test calls it on, so no lock is needed.
type recorderLog struct{ lines []string }

func (l *recorderLog) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recorderLog) contains(sub string) bool {
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// gateRunRecorderRoot is a town root a row is appended under: t.TempDir is 0777
// less the umask, and the gate runs it as 000, so the root is made private.
func gateRunRecorderRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	//testpolicy:allow no-exec-files — a directory, not a file: its owner search bit lets the append reach the row file
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("make the town root private: %v", err)
	}
	return root
}

// gateRunRecorderRows reads the rows the recorder appended.
func gateRunRecorderRows(t *testing.T, root string) []land.GateRun {
	t.Helper()
	runs, unparsed, err := land.ReadGateRuns(root)
	if err != nil {
		t.Fatalf("read the gate runs: %v", err)
	}
	if unparsed != 0 {
		t.Fatalf("the gate run log holds %d unparsed line(s)", unparsed)
	}
	return runs
}

func assertUnmeasured(t *testing.T, runs []land.GateRun) {
	t.Helper()
	if len(runs) != 1 {
		t.Fatalf("the recorder appended %d row(s), want 1", len(runs))
	}
	if p, th, us := runs[0].RunnerPeriods, runs[0].RunnerThrottled, runs[0].RunnerThrottledUsec; p != -1 || th != -1 || us != -1 {
		t.Errorf("counters = (%d, %d, %d), want all -1 for an unmeasured reading", p, th, us)
	}
}

// TestGateRunRecorderAppends: one recorded run appends one row carrying the
// throttle counters, and the run's own fields survive.
func TestGateRunRecorderAppends(t *testing.T) {
	t.Parallel()
	root := gateRunRecorderRoot(t)
	var log recorderLog
	rec := NewGateRunRecorder(func(context.Context, ...string) (string, error) {
		return "nr_periods 40\nnr_throttled 7\nthrottled_usec 3100\n", nil
	}, root, log.logf)
	rec(land.GateRun{Rig: "gastown", Bead: "gt-a", SHA: "c0ffee", RunID: "bob/42", Passed: true})
	runs := gateRunRecorderRows(t, root)
	if len(runs) != 1 {
		t.Fatalf("the recorder appended %d row(s), want 1", len(runs))
	}
	got := runs[0]
	if p, th, us := got.RunnerPeriods, got.RunnerThrottled, got.RunnerThrottledUsec; p != 40 || th != 7 || us != 3100 {
		t.Errorf("counters = (%d, %d, %d), want (40, 7, 3100)", p, th, us)
	}
	if got.Bead != "gt-a" || got.SHA != "c0ffee" || got.RunID != "bob/42" || !got.Passed {
		t.Errorf("the row lost the run's fields: %+v", got)
	}
	if len(log.lines) != 0 {
		t.Errorf("a successful record logged: %v", log.lines)
	}
}

// TestGateRunRecorderBlockedRead: a reader that never returns is abandoned at
// the bound, logged, and the row is appended with all three counters at -1.
func TestGateRunRecorderBlockedRead(t *testing.T) {
	t.Parallel()
	root := gateRunRecorderRoot(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var log recorderLog
	rec := newGateRunRecorder(func(context.Context, ...string) (string, error) {
		<-release
		return "", errors.New("the read was released")
	}, root, log.logf, time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec(land.GateRun{Bead: "gt-a"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second): //testpolicy:allow no-sleep — a hang guard only: the reader never returns, so only the recorder's own bound can end it, and one with none fails here
		t.Fatal("the recorder did not return at its throttle bound")
	}
	if !log.contains("abandoned") {
		t.Errorf("the abandoned read was not logged: %v", log.lines)
	}
	assertUnmeasured(t, gateRunRecorderRows(t, root))
}

// TestGateRunRecorderFailures: a failed read appends the row with the counters
// at -1 and logs; a failed append logs and returns.
func TestGateRunRecorderFailures(t *testing.T) {
	t.Parallel()
	okExec := func(context.Context, ...string) (string, error) {
		return "nr_periods 4\nnr_throttled 1\nthrottled_usec 20\n", nil
	}
	notADir := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed a file where the town root goes: %v", err)
	}
	cases := []struct {
		name     string
		exec     func(context.Context, ...string) (string, error)
		townRoot string
		wantLog  string
	}{
		{"read fails", func(context.Context, ...string) (string, error) { return "", errors.New("cpu.stat is unreadable") }, "", "cpu.stat is unreadable"},
		{"append fails", okExec, notADir, "appending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := tc.townRoot
			if root == "" {
				root = gateRunRecorderRoot(t)
			}
			var log recorderLog
			newGateRunRecorder(tc.exec, root, log.logf, time.Second)(land.GateRun{Bead: "gt-a"})
			if !log.contains(tc.wantLog) {
				t.Errorf("the failure was not logged as %q: %v", tc.wantLog, log.lines)
			}
			if tc.townRoot != "" {
				return // the root is a file, so no row could be appended
			}
			assertUnmeasured(t, gateRunRecorderRows(t, root))
		})
	}
}

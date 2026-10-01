package daemon

import (
	"fmt"
	"testing"
)

// TestLogLatchLogsOnChangeOnly is the gt tail noise fix: a steady state the
// daemon re-checks every pass ("Convoy X: 3 tracked issues, 0 ready",
// "Skipping crash detection for gastown/opal: ...") is logged when it starts
// or changes, not on every pass, and logged again once it ends and recurs.
func TestLogLatchLogsOnChangeOnly(t *testing.T) {
	t.Parallel()
	var got []string
	logf := func(format string, args ...interface{}) { got = append(got, fmt.Sprintf(format, args...)) }
	var l logLatch

	l.logf(logf, "a", "a: %d", 1)
	l.logf(logf, "a", "a: %d", 1) // same state: quiet
	l.logf(logf, "b", "b: %d", 1) // another key: its own state
	l.logf(logf, "a", "a: %d", 2) // changed: logged
	l.forget("a")                 // the state ended
	l.logf(logf, "a", "a: %d", 2) // recurred: logged again

	want := []string{"a: 1", "b: 1", "a: 2", "a: 2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("logged %q, want %q", got, want)
	}
}

// TestLogLatchKeepOnlyForgetsTheRest: a scan that no longer reports a key
// (the convoy left the stranded list) forgets it, so a later return logs.
func TestLogLatchKeepOnlyForgetsTheRest(t *testing.T) {
	t.Parallel()
	var n int
	logf := func(string, ...interface{}) { n++ }
	var l logLatch
	l.logf(logf, "x", "x")
	l.logf(logf, "y", "y")
	l.keepOnly(map[string]bool{"y": true})
	l.logf(logf, "x", "x") // forgotten: logs
	l.logf(logf, "y", "y") // kept: quiet
	if n != 3 {
		t.Fatalf("logged %d lines, want 3", n)
	}
}

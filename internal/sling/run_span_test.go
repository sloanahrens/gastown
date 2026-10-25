package sling

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestRunStartsTheSpanAfterTheSession: the attempt is the session's time on the
// bead, so the start is written when the session is up — a dispatch that
// measured from the sling call would charge the spawn to the polecat, and the
// spawn takes about a minute and varies with load.
func TestRunStartsTheSpanAfterTheSession(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{}

	if _, err := Run(context.Background(), f.deps(), holdTestOptions()); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if want := []string{"session", "span"}; !reflect.DeepEqual(f.order, want) {
		t.Fatalf("calls = %v, want %v: the span must start once, after the session", f.order, want)
	}
	want := holdTestTownRoot + " " + holdTestBeadID + " gastown/onyx"
	if len(f.spanStarts) != 1 || f.spanStarts[0] != want {
		t.Errorf("span starts = %v, want exactly [%q]", f.spanStarts, want)
	}
}

// TestRunFailingSpanStartStillDispatches: the span is a measurement, not a
// gate — a log that cannot be written is reported and the seat keeps its work.
func TestRunFailingSpanStartStillDispatches(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{spanErr: errors.New("read-only file system")}
	deps := f.deps()
	var out bytes.Buffer
	deps.Out = &out

	result, err := Run(context.Background(), deps, holdTestOptions())
	if err != nil {
		t.Fatalf("dispatch failed on a span write: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("result = %+v, want the dispatch to succeed", result)
	}
	if len(f.spanStarts) != 1 {
		t.Errorf("span starts = %v, want the write attempted once", f.spanStarts)
	}
	if !strings.Contains(out.String(), "Could not record the polecat span start") {
		t.Errorf("dispatch output %q does not report the failed span write", out.String())
	}
}

// TestRunNoSpanWhenTheSessionFails: no session means no attempt, and a start
// recorded for one would leave the span log claiming work that never began.
func TestRunNoSpanWhenTheSessionFails(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{startErr: errors.New("tmux: no server")}

	if _, err := Run(context.Background(), f.deps(), holdTestOptions()); err == nil {
		t.Fatal("a dispatch whose session failed to start must fail")
	}
	if len(f.spanStarts) != 0 {
		t.Errorf("span starts = %v, want none for a session that never started", f.spanStarts)
	}
}

package landworker

import (
	"context"
	"testing"
)

// TestReviewPoolWorkerPoolDefaultsNil pins that the harness Worker leaves Pool
// nil and that a pass over two ready beads still lands both with today's
// report: the field is storage for later beads, and a nil pool changes nothing
// (gt-8vfrwi).
func TestReviewPoolWorkerPoolDefaultsNil(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Pool != nil {
		t.Fatalf("harness Worker Pool = %#v, want nil", h.w.Pool)
	}
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	rep := h.w.Pass(context.Background())
	if want := (Report{Landed: 2}); rep != want {
		t.Fatalf("pass report = %+v, want %+v", rep, want)
	}
}

// TestReviewPoolWorkerStoredDefaultsNil pins that the harness Worker leaves
// Stored nil and that a pass over two ready beads still lands both with
// today's report: the field is storage for later beads, and a nil hint source
// changes nothing (gt-5q8ayw).
func TestReviewPoolWorkerStoredDefaultsNil(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Stored != nil {
		t.Fatalf("harness Worker Stored = %#v, want nil", h.w.Stored)
	}
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	rep := h.w.Pass(context.Background())
	if want := (Report{Landed: 2}); rep != want {
		t.Fatalf("pass report = %+v, want %+v", rep, want)
	}
}

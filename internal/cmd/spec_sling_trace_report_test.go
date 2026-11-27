package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// TestSpecSlingTraceReportWritesPhaseToWriter asserts a phase on the trace
// newSpecSlingTrace returns reaches the writer as it ends (gt-ojc5t3). The
// clock is stepped inside the phase, so the duration is exact and no test sleeps.
func TestSpecSlingTraceReportWritesPhaseToWriter(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	var buf strings.Builder
	trace := newSpecSlingTrace(func() time.Time { return at }, &buf)
	if err := trace.Phase("spawn", func() error { at = at.Add(1500 * time.Millisecond); return nil }); err != nil {
		t.Fatalf("Phase failed: %v", err)
	}
	if want := session.PhaseLinePrefix + "spawn 1.5s\n"; buf.String() != want {
		t.Fatalf("trace wrote %q, want %q", buf.String(), want)
	}
}

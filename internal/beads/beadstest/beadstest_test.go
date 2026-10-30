package beadstest

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// Counting forwards each read to the wrapped client and keeps its count and
// cutoff.
func TestCountingCountsListCalls(t *testing.T) {
	t.Parallel()
	counting := &Counting{Client: beadsfake.New()}
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	for i := 0; i < 2; i++ {
		if _, err := counting.List(beads.ListOptions{Status: "all", CreatedAfter: cutoff}); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if counting.Lists != 2 {
		t.Errorf("Lists = %d, want 2", counting.Lists)
	}
	if len(counting.Cutoffs) != 2 || !counting.Cutoffs[0].Equal(cutoff) {
		t.Errorf("Cutoffs = %v, want the read %s twice", counting.Cutoffs, cutoff)
	}
}

// Failing fails the read it is asked for, so a caller's failure path can be
// driven.
func TestFailingFailsReads(t *testing.T) {
	t.Parallel()
	if _, err := (Failing{}).List(beads.ListOptions{Status: "all"}); err == nil {
		t.Fatal("Failing.List returned no error, want the read failure")
	}
}

package convoy

import (
	"context"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestStrandedScanExcludesStagedConvoys verifies that findStrandedConvoys
// lists only open convoys, so a staged convoy (status "staged_ready" or
// "staged_warnings") is never reported (gt-csl.5.2), even though it tracks
// nothing and an open one in its place would be stranded.
func TestStrandedScanExcludesStagedConvoys(t *testing.T) {
	t.Parallel()
	db := townDB()
	seedConvoy(t, db, beads.Issue{ID: "hq-cv-staged1", Title: "Staged convoy", Status: "staged_ready"})
	seedConvoy(t, db, beads.Issue{ID: "hq-cv-staged2", Title: "Staged with warnings", Status: "staged_warnings"})

	stranded, err := testTown(townWithBeads(t, ""), db, nil).findStrandedWith(context.Background(), noBlockers)
	if err != nil {
		t.Fatalf("findStrandedConvoys() error: %v", err)
	}
	if len(stranded) != 0 {
		t.Errorf("stranded = %+v, want none: a staged convoy leaked into the scan", stranded)
	}
}

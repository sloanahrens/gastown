package convoy

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestGetTrackingConvoys_FiltersByTracksType(t *testing.T) {
	t.Parallel()
	source, cleanup := setupTestStore(t)
	defer cleanup()

	// Create convoy and tracked issue
	source.seed(
		beads.Issue{ID: "hq-cv-test1", Title: "Test Convoy", Status: "open", Priority: 2, Type: "task"},
		beads.Issue{ID: "gt-tracked1", Title: "Tracked", Status: "open", Priority: 2, Type: "task"},
		beads.Issue{ID: "hq-cv-other", Title: "Other Convoy", Status: "open", Priority: 2, Type: "task"},
	)

	// The convoy tracks the issue; the other convoy merely blocks on it, and
	// a "blocks" edge must not read as tracking.
	source.link("hq-cv-test1", "gt-tracked1", "tracks")
	source.link("hq-cv-other", "gt-tracked1", "blocks")

	convoyIDs := getTrackingConvoys(source, t.TempDir(), "gt-tracked1", nil)
	if len(convoyIDs) != 1 || convoyIDs[0] != "hq-cv-test1" {
		t.Errorf("getTrackingConvoys = %v, want [hq-cv-test1]", convoyIDs)
	}
}

// TestGetTrackingConvoys_MatchesExternalTargets pins the tracking read through
// an external:<prefix>:<id> edge: a town convoy tracks a rig bead in that form
// (gt convoy's trackingDependsOnID), bd's join drops it, and the raw read has
// to match it all the same (gt-3y3rl, gt-j02xy).
func TestGetTrackingConvoys_MatchesExternalTargets(t *testing.T) {
	t.Parallel()
	source, cleanup := setupTestStore(t)
	defer cleanup()

	townRoot := setupTownRoot(t) // test- routes to the testrig rig
	source.seed(beads.Issue{ID: "hq-cv1", Title: "Convoy", Status: "open", Type: "convoy"})
	source.link("hq-cv1", "external:test:test-work", "tracks")

	convoyIDs := getTrackingConvoys(source, townRoot, "test-work", nil)
	if len(convoyIDs) != 1 || convoyIDs[0] != "hq-cv1" {
		t.Errorf("getTrackingConvoys = %v, want [hq-cv1]", convoyIDs)
	}
}

func TestIsConvoyClosed_ReturnsCorrectStatus(t *testing.T) {
	t.Parallel()
	source, cleanup := setupTestStore(t)
	defer cleanup()

	source.seed(
		beads.Issue{ID: "hq-cv-open", Title: "Open", Status: "open", Priority: 2, Type: "task"},
		beads.Issue{ID: "hq-cv-closed", Title: "Closed", Status: "closed", Priority: 2, Type: "task"},
	)

	if isConvoyClosed(source, "hq-cv-open") {
		t.Error("isConvoyClosed(open) = true, want false")
	}
	if !isConvoyClosed(source, "hq-cv-closed") {
		t.Error("isConvoyClosed(closed) = false, want true")
	}
}

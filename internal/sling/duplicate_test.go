package sling

import (
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// TestSlingDuplicateIgnoresDependencyChain is the gt-xydyc acceptance bar: a
// bead in the candidate's own dependency chain names the same test because it
// is the same work, so it must not refuse the sling; an unrelated bead naming
// that test still must.
func TestSlingDuplicateIgnoresDependencyChain(t *testing.T) {
	t.Parallel()

	const candidateID = "gt-plan.1"
	edges := []DepEdge{
		{From: "gt-plan.9", To: candidateID}, // depends on the candidate
		{From: candidateID, To: "gt-plan.3"}, // the candidate depends on it
		{From: "gt-plan.9.1", To: "gt-plan.9"},
	}
	related := RelatedBeads(candidateID, edges, 4)

	// The candidate carries the lower priority number, so every open bead here
	// is behind it in dispatch order and the only thing that can refuse the
	// sling is the overlap itself.
	candidate := Duplicate{ID: candidateID, Priority: 1}
	tests := []struct {
		name      string
		bead      string
		wantBlock bool
	}{
		{"dependent sharing a test is allowed", "gt-plan.9", false},
		{"prerequisite sharing a test is allowed", "gt-plan.3", false},
		{"a dependent's own dependent is allowed too", "gt-plan.9.1", false},
		{"unrelated sharing a test is refused", "gt-unrelated", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := DuplicateMatch{
				Bead:        Duplicate{ID: tt.bead, Status: "in_progress"},
				SharedTests: []string{"TestSharedStep"},
			}
			kept := DropRelatedMatches([]DuplicateMatch{match}, related)
			got := DecideDuplicates(candidate, kept)
			if got.Blocked != tt.wantBlock {
				t.Fatalf("Blocked = %v, want %v (kept %+v)", got.Blocked, tt.wantBlock, kept)
			}
		})
	}
}

// TestDropRelatedMatchesKeepsTheUnrelatedOne guards the report: the chain is
// dropped from what the operator sees, not just from what blocks.
func TestDropRelatedMatchesKeepsTheUnrelatedOne(t *testing.T) {
	t.Parallel()
	const candidateID = "gt-plan.1"
	matches := []DuplicateMatch{
		{Bead: Duplicate{ID: "gt-plan.3", Status: "open"}, SharedTests: []string{"TestSharedStep"}},
		{Bead: Duplicate{ID: "gt-unrelated", Status: "open"}, SharedTests: []string{"TestSharedStep"}},
		{Bead: Duplicate{ID: "gt-plan.9", Status: "hooked"}, SharedTests: []string{"TestSharedStep"}},
	}
	edges := []DepEdge{
		{From: "gt-plan.9", To: candidateID},
		{From: candidateID, To: "gt-plan.3"},
	}

	kept := DropRelatedMatches(matches, RelatedBeads(candidateID, edges, 4))
	if len(kept) != 1 || kept[0].Bead.ID != "gt-unrelated" {
		t.Fatalf("kept %+v, want only gt-unrelated", kept)
	}
	candidate := Duplicate{ID: candidateID, Priority: 1}
	if got := DecideDuplicates(candidate, kept); !got.Blocked {
		t.Fatalf("the unrelated bead must still refuse the sling:\n%s", got.Message)
	}
}

func TestRelatedBeadsIsBounded(t *testing.T) {
	t.Parallel()
	// gt-a depends on root, gt-b on gt-a, gt-c on gt-b.
	edges := []DepEdge{
		{From: "gt-a", To: "gt-root"},
		{From: "gt-b", To: "gt-a"},
		{From: "gt-c", To: "gt-b"},
	}
	if got := RelatedBeads("gt-root", edges, 2); !got["gt-a"] || !got["gt-b"] || got["gt-c"] {
		t.Errorf("RelatedBeads(maxHops=2) = %v, want gt-a and gt-b but not gt-c", got)
	}
	if got := RelatedBeads("gt-root", edges, 0); got != nil {
		t.Errorf("RelatedBeads(maxHops=0) = %v, want no walk", got)
	}
	if got := RelatedBeads("", edges, 4); got != nil {
		t.Errorf("RelatedBeads(\"\") = %v, want no walk", got)
	}
}

func TestIsPackageFixtureTest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"TestMain", true},
		{"TestMain_", true},
		{"TestMaintenanceWindowExpiry", false},
		{"TestMainFrame", false},
		{"TestHermeticHarnessEnforced", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsPackageFixtureTest(tt.name); got != tt.want {
			t.Errorf("IsPackageFixtureTest(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestDuplicateRefusalIsAnAutomaticDispatchersRefusal: the report DecideDuplicates
// renders for a blocking overlap must classify as a refusal by the text the
// daemon reads, not only by the typed error (gt-g1xiy). The two are pinned
// together so a reworded report cannot quietly turn a deferral into a failure.
func TestDuplicateRefusalIsAnAutomaticDispatchersRefusal(t *testing.T) {
	t.Parallel()
	candidate := Duplicate{ID: "gt-new", Title: "candidate", Status: "open", Priority: 2}
	match := DuplicateMatch{
		Bead:        Duplicate{ID: "gt-old", Title: "overlap", Status: "in_progress", Priority: 2},
		SharedTests: []string{"TestFoo"},
	}
	err := DecideDuplicates(candidate, []DuplicateMatch{match}).Err()
	if err == nil {
		t.Fatal("an in-flight overlap sharing a test must refuse the sling")
	}
	if _, ok := dispatch.SlingRefusalLine(err.Error()); !ok {
		t.Errorf("SlingRefusalLine does not classify the duplicate report as a refusal:\n%s", err)
	}
}

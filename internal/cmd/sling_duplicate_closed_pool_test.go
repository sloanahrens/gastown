package cmd

import (
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// The duplicate pool reads design and notes for the active beads and the beads
// closed within duplicateRecentlyClosed; a bead closed earlier is compared on
// its list row and is read only for a candidate whose refs overlap that row
// (gt-ebags).

// closedPoolFixture seeds one active bead, one bead closed two hours ago and
// three closed two days ago: one naming internal/shared/target.go in its
// description, one sharing nothing with any candidate, one naming a file only in
// its notes.
func closedPoolFixture(now time.Time) *beadsfake.Fake {
	stamp := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339) }
	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-act", Type: "task", Title: "Active bead", Status: "open", Priority: 2,
			Description: "Nothing shared.", CreatedAt: stamp(10 * time.Hour), UpdatedAt: stamp(10 * time.Hour)},
		beads.Issue{ID: "gt-recent", Type: "task", Title: "Closed two hours ago", Status: "closed", Priority: 2,
			Description: "Nothing shared.", Notes: "Touched internal/recent/only.go.",
			CreatedAt: stamp(20 * time.Hour), UpdatedAt: stamp(2 * time.Hour), ClosedAt: stamp(2 * time.Hour)},
		beads.Issue{ID: "gt-old-overlap", Type: "task", Title: "Closed two days ago, overlapping", Status: "closed", Priority: 2,
			Description: "Edits internal/shared/target.go.", Notes: "Also internal/shared/extra.go.",
			CreatedAt: stamp(60 * time.Hour), UpdatedAt: stamp(48 * time.Hour), ClosedAt: stamp(48 * time.Hour)},
		beads.Issue{ID: "gt-old-other", Type: "task", Title: "Closed two days ago, unrelated", Status: "closed", Priority: 2,
			Description: "Edits internal/unrelated/thing.go.",
			CreatedAt:   stamp(60 * time.Hour), UpdatedAt: stamp(48 * time.Hour), ClosedAt: stamp(48 * time.Hour)},
		beads.Issue{ID: "gt-old-notes", Type: "task", Title: "Closed two days ago, overlap in notes only", Status: "closed", Priority: 2,
			Description: "Nothing in the list row.", Notes: "Edits internal/hidden/notesonly.go.",
			CreatedAt: stamp(60 * time.Hour), UpdatedAt: stamp(48 * time.Hour), ClosedAt: stamp(48 * time.Hour)},
	)
	return db
}

func poolByID(pool []duplicateCandidate) map[string]duplicateCandidate {
	out := make(map[string]duplicateCandidate, len(pool))
	for _, c := range pool {
		out[c.ID] = c
	}
	return out
}

// The pool fetch shows the active bead and the recently closed one, and none of
// the beads closed two days ago. Fetching all closed beads again turns this red.
func TestClosedPoolDetailsOnlyForOverlapOrRecent(t *testing.T) {
	t.Parallel()
	store := newCountingStore(listLikeBD{Client: closedPoolFixture(time.Now())})

	pool, err := fetchDuplicatePoolFrom(store, func(string, error) {}, "beadsdir")
	if err != nil {
		t.Fatalf("fetchDuplicatePoolFrom: %v", err)
	}

	shown := shownIDs(store)
	slices.Sort(shown)
	if want := []string{"gt-act", "gt-recent"}; !slices.Equal(shown, want) {
		t.Errorf("the pool fetch showed %v, want %v: a bead closed two days ago is compared on its list row", shown, want)
	}
	byID := poolByID(pool)
	if len(byID) != 5 {
		t.Fatalf("pool has %d beads, want all 5 compared", len(byID))
	}
	for _, id := range []string{"gt-old-overlap", "gt-old-other", "gt-old-notes"} {
		if !byID[id].ListOnly {
			t.Errorf("%s is not marked ListOnly", id)
		}
	}
	for _, id := range []string{"gt-act", "gt-recent"} {
		if byID[id].ListOnly {
			t.Errorf("%s is marked ListOnly, but its design and notes were read", id)
		}
	}
}

// A candidate asks for the details of the older closed beads whose list row
// shares a ref with it, and of no other. Asking for every ListOnly bead turns
// this red.
func TestEnrichReadsOnlyTheOlderBeadsThatOverlap(t *testing.T) {
	t.Parallel()
	store := newCountingStore(listLikeBD{Client: closedPoolFixture(time.Now())})
	pool, err := fetchDuplicatePoolFrom(store, func(string, error) {}, "beadsdir")
	if err != nil {
		t.Fatalf("fetchDuplicatePoolFrom: %v", err)
	}

	var asked []string
	p := newDuplicatePools(nil, nil)
	p.details = func(_ string, ids []string) (map[string]duplicateDetail, error) {
		asked = append(asked, ids...)
		return fetchDuplicateDetails(listLikeBD{Client: closedPoolFixture(time.Now())}, ids)
	}
	candidate := duplicateCandidate{ID: "gt-new", Refs: extractOverlapRefs("Edit internal/shared/target.go.")}

	enriched := p.enrichOverlapping("beadsdir", candidate, pool)

	if want := []string{"gt-old-overlap"}; !slices.Equal(asked, want) {
		t.Errorf("details asked for %v, want %v", asked, want)
	}
	got := poolByID(enriched)["gt-old-overlap"]
	if got.ListOnly {
		t.Error("the overlapping bead is still ListOnly after its details were read")
	}
	if !slices.Contains(got.Refs.Files, "internal/shared/extra.go") {
		t.Errorf("enriched refs %v lack the file only its notes name", got.Refs.Files)
	}
	if poolByID(pool)["gt-old-overlap"].ListOnly != true {
		t.Error("the cached pool row was changed: enrichment must copy")
	}
}

// The accepted blind spot: a bead closed more than 24 hours ago whose only
// overlap with the candidate is in its design or notes is not seen, while the
// same overlap on a bead closed two hours ago is.
func TestClosedBeadWithOverlapInNotesOnlyIsMissedOutside24h(t *testing.T) {
	t.Parallel()
	store := newCountingStore(listLikeBD{Client: closedPoolFixture(time.Now())})
	pool, err := fetchDuplicatePoolFrom(store, func(string, error) {}, "beadsdir")
	if err != nil {
		t.Fatalf("fetchDuplicatePoolFrom: %v", err)
	}
	p := newDuplicatePools(nil, nil)
	p.details = func(_ string, ids []string) (map[string]duplicateDetail, error) {
		return fetchDuplicateDetails(listLikeBD{Client: closedPoolFixture(time.Now())}, ids)
	}

	older := duplicateCandidate{ID: "gt-new", Refs: extractOverlapRefs("Edit internal/hidden/notesonly.go.")}
	if matches := findDuplicateMatches(older, p.enrichOverlapping("beadsdir", older, pool)); len(matches) != 0 {
		t.Errorf("matched %d beads on a notes-only overlap with a bead closed two days ago, want the documented miss", len(matches))
	}

	recent := duplicateCandidate{ID: "gt-new", Refs: extractOverlapRefs("Edit internal/recent/only.go.")}
	matches := findDuplicateMatches(recent, p.enrichOverlapping("beadsdir", recent, pool))
	if len(matches) != 1 || matches[0].Bead.ID != "gt-recent" {
		t.Errorf("matches = %+v, want the bead closed two hours ago", matches)
	}
}

// A close time that does not parse counts as recent: the bead keeps the full
// read it had before the split.
func TestSplitClosedByAgeKeepsAnUnreadableCloseTimeRecent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rows := []*beads.Issue{
		{ID: "gt-bad", ClosedAt: "not a time"},
		{ID: "gt-none"},
		{ID: "gt-old", ClosedAt: "2026-10-29T12:00:00Z"},
		{ID: "gt-new", ClosedAt: "2026-10-31T10:00:00Z"},
	}

	recent, older := splitClosedByAge(rows, now)

	ids := func(rs []*beads.Issue) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	if want := []string{"gt-bad", "gt-none", "gt-new"}; !slices.Equal(ids(recent), want) {
		t.Errorf("recent = %v, want %v", ids(recent), want)
	}
	if want := []string{"gt-old"}; !slices.Equal(ids(older), want) {
		t.Errorf("older = %v, want %v", ids(older), want)
	}
}

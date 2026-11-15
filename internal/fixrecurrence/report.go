package fixrecurrence

import (
	"slices"
	"time"
)

// Edge is one bead's claim that a closed bead it cites did not hold.
type Edge struct {
	// New is the bead that makes the claim.
	New string
	// Old is the closed bead whose fix the claim is about.
	Old string
	// Rel is the relationship the citing bead's prose asserts.
	Rel Relationship
}

// Closed is a closed bead and when it closed.
type Closed struct {
	ID       string
	ClosedAt time.Time
}

// ReportResult is the fix-held rate over one window.
type ReportResult struct {
	// Held is how many of the window's bugs no failing edge points at.
	Held int
	// Total is how many distinct bugs closed inside the window.
	Total int
	// Rate is Held divided by Total, and 0 for an empty window.
	Rate float64
	// PerRelationship counts the edges pointing at the window's bugs, by the
	// relationship each asserts.
	PerRelationship map[Relationship]int
	// Chronic lists, sorted, the window's bugs that two or more citing beads
	// mark as failed fixes.
	Chronic []string
}

// chronicStrikes is how many distinct citing beads must fail one fix before the
// fix is chronic rather than a single bad landing.
const chronicStrikes = 2

// Report returns the fix-held rate over the half-open window [since, until):
// a bug closed in it held unless a recurrence, same-class or regression edge
// points at it, and a since at or after until is an empty window. Follow-up
// and reference edges are counted and never count as failure; a relationship
// the package does not define does, because the report has no channel to
// refuse it. The caller supplies only edges from a bead created after the
// cited bead closed; Report has no timestamps to enforce that order itself.
func Report(closed []Closed, edges []Edge, since, until time.Time) ReportResult {
	result := ReportResult{PerRelationship: make(map[Relationship]int)}
	inWindow := make(map[string]bool, len(closed))
	for _, c := range closed {
		// A record naming no bead, or one with no close time, cannot be shown to
		// have closed in this window, so it is not counted as held or failed.
		if c.ID == "" || c.ClosedAt.IsZero() {
			continue
		}
		if c.ClosedAt.Before(since) || !c.ClosedAt.Before(until) {
			continue
		}
		inWindow[c.ID] = true
	}

	type claimKey struct {
		new, old string
		rel      Relationship
	}
	seenClaims := make(map[claimKey]bool)
	// struck maps a closed bead to the distinct new beads whose edges say its
	// fix failed. One citing bead is one incident however many failing
	// relationships it asserts.
	struck := make(map[string]map[string]bool)
	for _, e := range edges {
		// An edge names no claim without a citing bead, when it cites the bead
		// itself, or when the bead it accuses did not close in this window.
		if e.New == "" || e.New == e.Old || !inWindow[e.Old] {
			continue
		}
		key := claimKey{e.New, e.Old, e.Rel}
		if seenClaims[key] {
			continue
		}
		seenClaims[key] = true
		result.PerRelationship[e.Rel]++
		if holdsFix(e.Rel) {
			continue
		}
		if struck[e.Old] == nil {
			struck[e.Old] = make(map[string]bool)
		}
		struck[e.Old][e.New] = true
	}

	for id := range inWindow {
		result.Total++
		switch strikes := len(struck[id]); {
		case strikes == 0:
			result.Held++
		case strikes >= chronicStrikes:
			result.Chronic = append(result.Chronic, id)
		}
	}
	slices.Sort(result.Chronic)
	if result.Total > 0 {
		result.Rate = float64(result.Held) / float64(result.Total)
	}
	return result
}

// holdsFix reports whether rel leaves the cited fix standing. Only follow-up,
// which says the work continues, and reference, which makes no claim, do; a
// value the package does not define cannot be read as holding, so it fails the
// fix (unknown denies).
func holdsFix(rel Relationship) bool {
	switch rel {
	case FollowUp, Reference:
		return true
	default:
		return false
	}
}

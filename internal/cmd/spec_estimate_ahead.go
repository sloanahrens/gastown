package cmd

import (
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// The estimate-ahead pass estimates a few open shaped beads the dispatcher is
// not about to sling, so a bead carries an a priori estimate before it is ever
// dispatched (gt-0f8446, split into this pick and the tick wiring gt-hzb051).
// This file is the pick alone: it reads no store, no settings and no clock, so
// the tick hands it the beads it read and the answer depends on those values
// only.

// specEstimateAheadLimit is how many beads one pass estimates: two, the budget
// the parent bead set, so a tick spends at most two estimates ahead of
// dispatch.
const specEstimateAheadLimit = 2

// specEstimateAheadBead is one bead as the pick reads it.
type specEstimateAheadBead struct {
	// Issue is the bead's full read. Its Status, Assignee and Dependencies,
	// and the description sections and acceptance field the estimate hashes,
	// are what the pick decides on.
	Issue *beads.Issue
	// Comments are the bead's comments, oldest first. They carry the note of
	// the bead's latest estimate, whose hash says whether that estimate is
	// current.
	Comments []beads.Comment
	// CommentsRead reports that the caller read Comments. The pick leaves a
	// bead alone when it did not: an estimate note it cannot see would make the
	// bead look unestimated, and the pass would write a second estimate over
	// the label it exists to keep.
	CommentsRead bool
	// Shaped reports that the dispatcher's shape lint cleared the bead for
	// dispatch (specdispatch.LintVerdict.Clean). The pick takes it from the
	// caller because the lint reads the shape template and the target branch's
	// files, which the caller holds; a candidate it was not given leaves this
	// false and is never chosen.
	Shaped bool
}

// specEstimateAheadPick returns the IDs of at most limit beads to estimate:
// the open, unassigned, shaped beads with no unresolved blocking dependency
// and no estimate note for their current sections, oldest first and ties by
// ID. It returns no IDs when none qualifies or limit is not positive.
//
// A bead whose latest estimate note carries its current hash is left alone.
// That hash is the note's own record that the estimate was made from these
// sections, which is the same predicate gt spec estimate reads to call a bead
// unchanged; estimating again would replace the label the note records.
func specEstimateAheadPick(cands []specEstimateAheadBead, limit int) []string {
	if limit <= 0 {
		return nil
	}
	chosen := make([]specEstimateAheadBead, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, b := range cands {
		if !specEstimateAheadEligible(b) {
			continue
		}
		if seen[b.Issue.ID] {
			continue
		}
		seen[b.Issue.ID] = true
		chosen = append(chosen, b)
	}
	specEstimateAheadSort(chosen)
	if len(chosen) > limit {
		chosen = chosen[:limit]
	}
	var ids []string
	for _, b := range chosen {
		ids = append(ids, b.Issue.ID)
	}
	return ids
}

// specEstimateAheadEligible reports whether one bead is one the pass may
// estimate: open, unassigned, shaped, its comments read, no dependency that
// keeps it off the board, and no estimate of these sections.
func specEstimateAheadEligible(b specEstimateAheadBead) bool {
	if b.Issue == nil || strings.TrimSpace(b.Issue.ID) == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(b.Issue.Status), string(beads.StatusOpen)) {
		return false
	}
	if strings.TrimSpace(b.Issue.Assignee) != "" {
		return false
	}
	if !b.Shaped {
		return false
	}
	if !b.CommentsRead {
		return false
	}
	if specEstimateAheadBlocked(b.Issue) {
		return false
	}
	return latestEstimateHash(b.Comments) != estimate.SectionHash(b.Issue)
}

// specEstimateAheadBlocked reports whether a dependency keeps the bead off an
// estimate: a blocking edge that is not resolved, or a dependency whose
// relation the read did not classify. The unread edge refuses here as it does
// in sling.Admit, since a pass that guessed "not blocking" would estimate a
// bead behind an edge nobody read.
func specEstimateAheadBlocked(issue *beads.Issue) bool {
	return beads.HasUnresolvedBlockers(issue) || len(sling.Admit(issue.Dependencies)) > 0
}

// specEstimateAheadSort orders candidates oldest first, then by ID. A created
// time the pick cannot parse sorts after every parsed one, so the order stays
// deterministic without dropping a bead the pass could still estimate.
func specEstimateAheadSort(cands []specEstimateAheadBead) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].Issue, cands[j].Issue
		ta, tb := specEstimateAheadCreated(a.CreatedAt), specEstimateAheadCreated(b.CreatedAt)
		if !ta.Equal(tb) {
			if ta.IsZero() {
				return false
			}
			if tb.IsZero() {
				return true
			}
			return ta.Before(tb)
		}
		return a.ID < b.ID
	})
}

// specEstimateAheadCreated parses a bead's created_at with two of the layouts
// specdispatch's candidate ordering accepts, so a value this pick cannot date
// is one the dispatcher cannot date either. It returns the zero time for such a
// value.
func specEstimateAheadCreated(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

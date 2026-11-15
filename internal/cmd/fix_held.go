package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/fixrecurrence"
)

// fix_held.go is the fix-recurrence signal's rate (gt-t9vnda): it reads the
// rig's beads once, the way `gt fix-recurrence scan` does, and hands
// fixrecurrence.Report the bugs the window closed and the claims the store
// already carries against them. The `gt fix-held` command that prints the
// result lands with gt-tiaczu.

// The labels Plan writes on a citing bead when it records a recurrence or a
// same-class claim (internal/fixrecurrence/plan.go). Each names a claim from
// that bead to the bead it names, which this file passes to Report as an edge.
const (
	fixHeldRecurrenceLabel = "recurrence-of:"
	fixHeldSameClassLabel  = "same-class-of:"
)

// fixHeldReport reads the rig's beads and returns the fix-held rate over
// [since, until): the closed bugs in the window, and the claims the store
// already carries against them. A failed read is returned before a window is
// computed, so a partial list is never taken as the rig's answer, and a closed
// bug the rows cannot place in time fails the read rather than counting as held.
func fixHeldReport(d fixRecurrenceDeps, since, until time.Time) (fixrecurrence.ReportResult, error) {
	dir, err := d.workDir()
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("finding the rig directory: %w", err)
	}
	raw, err := d.run(dir, fixRecurrenceListArgs)
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("reading the rig's beads: %w", err)
	}
	// The issue type is the one field Bead does not keep, and only a bug counts
	// toward the rate, so the rows are decoded for it first: a list that does
	// not parse fails here.
	issueTypes, err := fixHeldIssueTypes(raw)
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("reading the rig's beads: %w", err)
	}
	// fixRecurrenceBeads is the scan's parse and validation: every row's
	// timestamps are read, and a row the scan would refuse is refused here too.
	listed, state, err := fixRecurrenceBeads(raw)
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("reading the rig's beads: %w", err)
	}
	closed, err := fixHeldClosed(listed, issueTypes)
	if err != nil {
		return fixrecurrence.ReportResult{}, err
	}
	return fixrecurrence.Report(closed, fixHeldEdges(listed, state), since, until), nil
}

// fixHeldIssueTypes is each listed bead's issue type, keyed by its lowercased
// id as Report keys ids. It decodes the rows for the one field Bead does not
// carry, and a list that does not parse fails the read rather than yielding no
// bug.
func fixHeldIssueTypes(raw []byte) (map[string]string, error) {
	var rows []fixRecurrenceRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("bd list output does not parse: %w", err)
	}
	issueTypes := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			issueTypes[strings.ToLower(row.ID)] = row.IssueType
		}
	}
	return issueTypes, nil
}

// fixHeldClosed is the closed bugs the rate counts: a bead the store closed and
// typed bug. A closed bug whose closed_at is missing cannot be shown to be in
// the window, and dropping it would inflate the rate, so the read refuses it.
func fixHeldClosed(listed []fixrecurrence.Bead, issueTypes map[string]string) ([]fixrecurrence.Closed, error) {
	var closed []fixrecurrence.Closed
	for _, bead := range listed {
		if bead.Status != "closed" || issueTypes[strings.ToLower(bead.ID)] != "bug" {
			continue
		}
		if bead.ClosedAt.IsZero() {
			return nil, fmt.Errorf("bead %s: closed_at is missing, so it cannot be placed in the window", bead.ID)
		}
		closed = append(closed, fixrecurrence.Closed{ID: bead.ID, ClosedAt: bead.ClosedAt})
	}
	return closed, nil
}

// fixHeldEdges is every claim the store already carries: its typed dependency
// edges and the two labels Plan writes on a citing bead. Report has no
// timestamps and trusts the caller to supply only an edge from a bead created
// after the cited bead closed, so an edge whose order the rows do not show --
// either end unlisted, the cited bead not closed, or the citing bead created at
// or before the close -- is dropped. A pair a label names is one claim: bd
// keeps one "related" type for both a recurrence and a same-class claim, and
// the label on the citing bead says which, so the edge adds no second claim.
func fixHeldEdges(listed []fixrecurrence.Bead, state fixrecurrence.State) []fixrecurrence.Edge {
	type pair struct{ citing, cited string }
	created := make(map[string]time.Time, len(listed))
	closed := make(map[string]time.Time, len(listed))
	for _, bead := range listed {
		key := strings.ToLower(bead.ID)
		created[key] = bead.CreatedAt
		closed[key] = bead.ClosedAt
	}
	var edges []fixrecurrence.Edge
	labeled := make(map[pair]bool)
	for id, labels := range state.Labels {
		for label := range labels {
			rel, old, ok := fixHeldLabel(label)
			if !ok {
				continue
			}
			edges = append(edges, fixrecurrence.Edge{New: id, Old: old, Rel: rel})
			labeled[pair{strings.ToLower(id), strings.ToLower(old)}] = true
		}
	}
	for edge := range state.Deps {
		key := pair{strings.ToLower(edge.New), strings.ToLower(edge.Old)}
		if edge.Rel == fixrecurrence.Recurrence && labeled[key] {
			continue
		}
		edges = append(edges, edge)
	}
	claims := make([]fixrecurrence.Edge, 0, len(edges))
	for _, edge := range edges {
		citingCreated, ok := created[strings.ToLower(edge.New)]
		if !ok {
			continue
		}
		citedClosed, ok := closed[strings.ToLower(edge.Old)]
		if !ok || citedClosed.IsZero() || !citingCreated.After(citedClosed) {
			continue
		}
		claims = append(claims, edge)
	}
	return claims
}

// fixHeldLabel is the relationship and cited bead a claim label names:
// recurrence-of:<id> is a recurrence and same-class-of:<id> a same-class claim.
// Any other label, and one naming no bead, claims nothing here.
func fixHeldLabel(label string) (fixrecurrence.Relationship, string, bool) {
	if old, ok := strings.CutPrefix(label, fixHeldRecurrenceLabel); ok && old != "" {
		return fixrecurrence.Recurrence, old, true
	}
	if old, ok := strings.CutPrefix(label, fixHeldSameClassLabel); ok && old != "" {
		return fixrecurrence.SameClass, old, true
	}
	return "", "", false
}

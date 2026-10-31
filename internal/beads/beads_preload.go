package beads

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// PreloadBeads warms this *Beads' wisps and issues snapshots, and the
// dependency rows of both, from one bd sql round trip — the read behind `gt
// polecat list`, whose readers would otherwise each spawn a bd subprocess per
// rig (gt-92zx, gt-0hmt2, gt-59p7e).
//
// Wisps and issues travel together because one caller wants both at once and
// each read is a subprocess. The wisp side is unfiltered — the whole wisps
// table, split into per-label buckets in Go — because ListAgentBeadsFromWisps
// runs type/ID fallbacks over wisps whose label metadata is missing, and a
// label-filtered query cannot see them (gt-92zx). Labels and statuses scope the
// issues side.
//
// The caches never refresh — the same caveat as PreloadMergeRequests: don't
// hold this instance across writes that could create or change those rows
// mid-run.
//
// A rig the town's route table puts on the tracker backend has no bd to warm:
// every reader there takes its own per-call seam query, so the preload would
// be one failed read and one warning per rig and warm nothing (gt-p85pw.108).
//
// The route check runs before any read, so an unreadable route table is an
// error from PreloadBeads on every caller, not only on a tracker rig, and no
// store is read. The production caller, preloadRigSeats, treats that error as
// a warning and falls back to its per-call query.
func (b *Beads) PreloadBeads(labels []string, statuses []IssueStatus) error {
	onTracker, err := b.onTrackerRoute()
	if err != nil {
		return err
	}
	if onTracker {
		return nil
	}

	statuses = uniqueStatuses(statuses)
	statusNames := make([]string, len(statuses))
	for i, status := range statuses {
		statusNames[i] = string(status)
	}
	rows, err := b.queryIssueRows(beadsql.PreloadedBeads(labels, statusNames))
	if err != nil {
		return err
	}
	halves := splitPreloadRows(rows)

	// The wisps half: the snapshot, the per-label buckets ListMergeRequests
	// reads, and the dependency rows its hydration reads.
	wisps := wispRowsToIssues(halves.wisps)
	cache := make(map[string][]*Issue, len(labels))
	for _, label := range labels {
		cache[label] = nil
	}
	for _, wisp := range wisps {
		for _, label := range wisp.Labels {
			if _, ok := cache[label]; ok {
				cache[label] = append(cache[label], wisp)
			}
		}
	}
	b.wispCache = cache
	b.wispSnapshot = wisps
	b.wispDetails = newPreloadDetails(wisps, halves.wispDeps)

	// The issues half, recording what the read covers: a reader answers from
	// the snapshot only for a label or a status warmed here and falls through
	// to bd otherwise.
	snap := &issueSnapshot{
		labels:   make(map[string]bool, len(labels)),
		statuses: make(map[IssueStatus]bool, len(statuses)),
		issues:   make([]*Issue, 0, len(halves.issues)),
	}
	for _, label := range labels {
		snap.labels[label] = true
	}
	for _, status := range statuses {
		snap.statuses[status] = true
	}
	for _, row := range halves.issues {
		snap.issues = append(snap.issues, row.toIssue(false))
	}
	snap.details = newPreloadDetails(snap.issues, halves.issueDeps)
	b.issueSnapshot = snap
	return nil
}

// onTrackerRoute reports whether this wrapper's beads are a rig the town's
// route table puts on the tracker backend — the rigs internal/beads/backend
// opens without bd. It matches the way that package does: by the beads
// directory the route names, resolved through the rig's redirect.
//
// A wrapper outside a town, and one whose beads directory no route names, is
// not on the tracker and reads through bd as before. An unreadable route
// table is an error, never a false answer: reading it as "not the tracker"
// would send the caller's read into a store the table may have said is not
// there.
func (b *Beads) onTrackerRoute() (bool, error) {
	townRoot := b.getTownRoot()
	if townRoot == "" {
		return false, nil
	}
	routes, err := LoadRoutes(GetTownBeadsPath(townRoot))
	if err != nil {
		return false, fmt.Errorf("reading the route table: %w", err)
	}
	store := filepath.Clean(resolveSymlinksBestEffort(b.getResolvedBeadsDir()))
	for _, route := range routes {
		dir := filepath.Clean(resolveSymlinksBestEffort(filepath.Join(townRoot, route.Path, ".beads")))
		if dir == store {
			return route.BackendName() == BackendTracker, nil
		}
	}
	return false, nil
}

// preloadRows is one PreloadBeads read split into its four arms: the wisps and
// the issues it returned, and the dependency rows it carried for each.
type preloadRows struct {
	wisps     []bdSQLIssueRow
	wispDeps  []bdSQLIssueRow
	issues    []bdSQLIssueRow
	issueDeps []bdSQLIssueRow
}

// splitPreloadRows splits a PreloadBeads read by the tags its arms carry: src
// names the table a row came from, dep_row whether it is one of that table's
// rows or a dependency of one. A row missing either tag — the read would have
// to be a different query to produce one — is not routed anywhere, which
// leaves it out of both snapshots rather than silently filing it under the
// wrong table.
func splitPreloadRows(rows []bdSQLIssueRow) preloadRows {
	var halves preloadRows
	for _, row := range rows {
		switch {
		case row.Src == beadsql.WispSrc && row.DepRow != 0:
			halves.wispDeps = append(halves.wispDeps, row)
		case row.Src == beadsql.WispSrc:
			halves.wisps = append(halves.wisps, row)
		case row.Src == beadsql.IssueSrc && row.DepRow != 0:
			halves.issueDeps = append(halves.issueDeps, row)
		case row.Src == beadsql.IssueSrc:
			halves.issues = append(halves.issues, row)
		}
	}
	return halves
}

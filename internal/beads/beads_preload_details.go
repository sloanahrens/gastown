package beads

// preloadDetails is the dependency half of one preload read: every row that
// read returned, by id, plus the dependency rows the same round trip carried
// for those rows.
//
// Coverage is per id and complete by construction. The read asks for the
// dependencies of exactly the rows it returns, so an id in byID with nothing
// in deps has no dependencies — not "dependencies unknown" — and that is what
// lets hydrateMergeRequestDetails answer from a preload instead of from
// `bd show --json <ids>`. An id that is absent from byID was simply not read:
// detail reports ok=false for it and the caller pays the round trip.
type preloadDetails struct {
	byID map[string]*Issue
	deps map[string][]IssueDep
}

// newPreloadDetails indexes one preload read's two halves: rows is the read's
// own rows (already turned into Issues by the caller), depRows its dependency
// rows. The Issues are shared with the caller's snapshot, not copied; detail
// copies before it adds the dependencies, so a stored row stays as the read
// returned it.
func newPreloadDetails(rows []*Issue, depRows []bdSQLIssueRow) *preloadDetails {
	details := &preloadDetails{
		byID: make(map[string]*Issue, len(rows)),
		deps: make(map[string][]IssueDep, len(depRows)),
	}
	for _, row := range rows {
		if row != nil && row.ID != "" {
			details.byID[row.ID] = row
		}
	}
	for _, depRow := range depRows {
		if depRow.DepIssueID == "" {
			continue
		}
		details.deps[depRow.DepIssueID] = append(details.deps[depRow.DepIssueID], depRow.toIssueDep())
	}
	return details
}

// detail returns id's row with its dependencies and blocker counts filled in,
// the shape hydrateMergeRequestDetails builds out of ShowMultiple, so a
// hydration answered from a preload is indistinguishable from one answered by
// bd. ok is false for an id this read did not return a row for, and for a
// *preloadDetails that was never warmed.
func (p *preloadDetails) detail(id string) (*Issue, bool) {
	if p == nil {
		return nil, false
	}
	issue, ok := p.byID[id]
	if !ok {
		return nil, false
	}
	detail := *issue
	detail.Dependencies = p.deps[id]
	normalizeUnresolvedBlockers(&detail)
	return &detail, true
}

// preloadedDetail answers hydration from whichever preload read holds id: the
// issues read covers issues-table rows, the wisps read covers wisps. An unwarmed
// *Beads answers false from both.
func (b *Beads) preloadedDetail(id string) (*Issue, bool) {
	if detail, ok := b.issueSnapshot.detail(id); ok {
		return detail, true
	}
	return b.wispDetails.detail(id)
}

// hydrateFromPreload answers hydrateMergeRequestDetails from the preloaded
// snapshots when they cover every issue — the case whenever PreloadBeads ran,
// which is `gt polecat list` (gt-7dctf). That one read already carries the
// dependency rows of every row it returned, so the `bd show --json <ids>` this
// replaces would be re-reading data in memory.
//
// ok is false when even one id is not covered. Then the caller runs that one
// round trip for the whole set, as before: a partial answer would still need
// it, and a hydration that silently mixed two sources would be the kind of
// half-covered snapshot the rest of these caches are careful to refuse.
func (b *Beads) hydrateFromPreload(issues []*Issue) ([]*Issue, bool) {
	hydrated := make([]*Issue, 0, len(issues))
	for _, issue := range issues {
		if issue == nil || issue.ID == "" {
			hydrated = append(hydrated, issue)
			continue
		}
		detail, ok := b.preloadedDetail(issue.ID)
		if !ok {
			return nil, false
		}
		mergeListIssueFields(detail, issue)
		hydrated = append(hydrated, detail)
	}
	return hydrated, true
}

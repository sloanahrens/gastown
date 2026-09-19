package beads

import (
	"path/filepath"
	"sort"
	"strings"
)

// Merge-aware dependency satisfaction (gt-0r0z).
//
// Blocker resolution in this town is status-based: a dependency is treated as
// satisfied the moment the blocker bead's status reads "closed". But a polecat
// closes its bead at MR-CREATION time (in `gt done`), not at merge. So for the
// window between "MR submitted" and "MR merged" — which is where the beads gate
// spends ~13 minutes — "closed" means "submitted", while every dependency check
// (and every human reading `bd show`) takes it to mean "landed".
//
// The result is that a dependent bead is dispatched against a main that does
// not yet contain the artifact its blocker promised. The downstream worker
// designs against a world that does not exist and has no way to know.
//
// These helpers add the missing distinction. A blocking dependency is satisfied
// when the blocker is closed AND — if it produced a merge request — that merge
// request is no longer open. A blocker that never produced an MR (docs-only,
// decision beads, superseded) still satisfies on close, so nothing deadlocks.
// A blocker whose MR was rejected or superseded has a *closed* MR, so it too
// satisfies on close; only an OPEN MR holds dependents back.

// DependencyMergeState classifies whether a blocker's work has landed.
type DependencyMergeState string

const (
	// DependencyMergeLanded means the blocker is closed and its work has landed
	// (or it produced no merge request at all). Dependents may proceed.
	DependencyMergeLanded DependencyMergeState = "landed"

	// DependencyMergeUnmerged means the blocker is closed but still has an open
	// merge request. It has been submitted, not landed. Dependents must wait.
	DependencyMergeUnmerged DependencyMergeState = "unmerged"

	// DependencyMergeOpen means the blocker bead itself is still open.
	DependencyMergeOpen DependencyMergeState = "open"

	// DependencyMergeUnknown means the blocker's state could not be classified.
	DependencyMergeUnknown DependencyMergeState = "unknown"
)

// DependencyMergeStatus describes one blocking dependency's merge state, in a
// form compact enough to inject into a dispatched polecat's starting context.
type DependencyMergeStatus struct {
	ID     string               `json:"id"`
	Status string               `json:"status"`           // blocker bead status
	State  DependencyMergeState `json:"state"`            // landed | unmerged | open | unknown
	MR     string               `json:"mr,omitempty"`     // open MR bead ID, when unmerged
	Branch string               `json:"branch,omitempty"` // the MR's branch, when unmerged
}

// OpenMRsBySourceIssue returns a map from source issue ID to the open
// merge-request bead created for it. This is the index that turns the
// status-based question "is this blocker closed?" into the merge-aware
// question "has this blocker's work landed?".
//
// At most one MR per source issue is expected; if several are somehow open the
// lexicographically smallest MR ID wins, so callers get a deterministic answer.
// MRs without a parseable source_issue are skipped — they cannot be attributed
// to a blocker and so cannot hold one back.
func (b *Beads) OpenMRsBySourceIssue() (map[string]*Issue, error) {
	mrs, err := b.ListMergeRequests(ListOptions{
		Status: "open",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}

	bySource := make(map[string]*Issue, len(mrs))
	for _, mr := range mrs {
		if mr == nil || mr.Status == "closed" {
			continue
		}
		fields := ParseMRFields(mr)
		if fields == nil || strings.TrimSpace(fields.SourceIssue) == "" {
			continue
		}
		source := ExtractIssueID(fields.SourceIssue)
		prev, ok := bySource[source]
		if !ok || mr.ID < prev.ID {
			bySource[source] = mr
		}
	}
	return bySource, nil
}

// BlockingDependencies returns the blocking dependencies of issue, in the order
// bd reported them. A dependency counts as blocking when its relation type is
// one of blocks, conditional-blocks, waits-for, or merge-blocks.
func BlockingDependencies(issue *Issue) []IssueDep {
	if issue == nil {
		return nil
	}
	var deps []IssueDep
	for _, dep := range issue.Dependencies {
		if !isBlockingDependencyType(dep.DependencyType) {
			continue
		}
		deps = append(deps, dep)
	}
	return deps
}

// CandidateMergeAwareBlockerIDs returns the blocking dependency IDs of issue
// whose satisfaction could possibly depend on merge state: those that read as
// resolved by status alone. A blocker that is still open blocks its dependents
// for the ordinary reason and never needs a merge-queue lookup.
//
// This exists so callers can decide whether a merge-queue query is worth making
// before paying for it. A bead with no dependencies returns nil, and a dep
// graph of entirely open blockers returns nil too — in both cases the caller
// skips the query entirely and readiness is computed exactly as it was before
// this feature existed.
func CandidateMergeAwareBlockerIDs(issue *Issue) []string {
	deps := BlockingDependencies(issue)
	if len(deps) == 0 {
		return nil
	}

	var ids []string
	seen := make(map[string]bool, len(deps))
	for _, dep := range deps {
		if !isResolvedDependency(dep) {
			continue
		}
		id := ExtractIssueID(dep.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// UnmergedBlockerIDs returns the blocking dependency IDs of issue that are
// closed but whose merge request is still open — blockers that have been
// submitted but not landed. It returns nil when every blocker has landed, when
// the issue declares no blocking dependencies, or when merge state is unknown.
//
// openMRs is the index from OpenMRsBySourceIssue. A nil index is valid and
// means "no merge requests are open anywhere", which is the correct reading for
// a town with an empty merge queue.
func UnmergedBlockerIDs(issue *Issue, openMRs map[string]*Issue) []string {
	statuses := DependencyMergeStatuses(issue, openMRs)
	var ids []string
	for _, status := range statuses {
		if status.State == DependencyMergeUnmerged {
			ids = append(ids, status.ID)
		}
	}
	return ids
}

// HasUnmergedBlockers reports whether any blocking dependency of issue is
// closed but still has an open merge request.
func HasUnmergedBlockers(issue *Issue, openMRs map[string]*Issue) bool {
	return len(UnmergedBlockerIDs(issue, openMRs)) > 0
}

// DependencyMergeStatuses classifies every blocking dependency of issue into
// landed, unmerged, or open. Output is sorted by blocker ID for stable display
// and stable tests.
func DependencyMergeStatuses(issue *Issue, openMRs map[string]*Issue) []DependencyMergeStatus {
	deps := BlockingDependencies(issue)
	if len(deps) == 0 {
		return nil
	}

	statuses := make([]DependencyMergeStatus, 0, len(deps))
	seen := make(map[string]bool, len(deps))
	for _, dep := range deps {
		id := ExtractIssueID(dep.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		status := DependencyMergeStatus{
			ID:     id,
			Status: strings.ToLower(strings.TrimSpace(dep.Status)),
		}

		// A blocker that is not closed is unresolved for the ordinary reason;
		// merge state does not enter into it.
		if !isResolvedDependency(dep) {
			status.State = DependencyMergeOpen
			statuses = append(statuses, status)
			continue
		}

		// The blocker reads as resolved. Whether it has actually landed depends
		// on whether it left an open merge request behind.
		if mr, ok := openMRs[id]; ok && mr != nil {
			status.State = DependencyMergeUnmerged
			status.MR = mr.ID
			if fields := ParseMRFields(mr); fields != nil {
				status.Branch = fields.Branch
			}
		} else {
			status.State = DependencyMergeLanded
		}
		statuses = append(statuses, status)
	}

	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	return statuses
}

// ResolveDependencyMergeStatuses returns the merge status of every blocking
// dependency of issue, looking up each blocker's merge request in the beads
// database that blocker lives in.
//
// townBeadsDir is the town-level .beads directory; it is used to route a
// blocker ID to its rig's database so cross-rig dependencies resolve correctly.
//
// A blocker whose database could not be queried is reported as UNKNOWN, never
// as landed. The entire point of this signal is to distinguish "on main" from
// "still in the queue", so an unchecked blocker must not read as a safe one.
// This is deliberately asymmetric with the dispatch gate, which fails open: a
// gate that cannot act degrades to the old behavior, but a report that cannot
// check should still tell the worker the truth rather than reassure it.
func ResolveDependencyMergeStatuses(townBeadsDir string, issue *Issue) []DependencyMergeStatus {
	deps := BlockingDependencies(issue)
	if len(deps) == 0 {
		return nil
	}

	openMRs := make(map[string]*Issue)
	unchecked := make(map[string]bool)

	// One merge-queue query per database the blockers live in, not per blocker.
	byDir := make(map[string][]string)
	for _, dep := range deps {
		id := ExtractIssueID(dep.ID)
		if id == "" {
			continue
		}
		dir := ResolveBeadsDirForID(townBeadsDir, id)
		byDir[dir] = append(byDir[dir], id)
	}
	for dir, ids := range byDir {
		b := NewWithBeadsDir(filepath.Dir(dir), dir)
		index, err := b.OpenMRsBySourceIssue()
		if err != nil {
			for _, id := range ids {
				unchecked[id] = true
			}
			continue
		}
		for _, id := range ids {
			if mr, ok := index[id]; ok {
				openMRs[id] = mr
			}
		}
	}

	statuses := DependencyMergeStatuses(issue, openMRs)
	for i := range statuses {
		// Only a "landed" verdict depends on having reached the merge queue: it
		// is what an empty index produces. A blocker that is still open is
		// unresolved from its status alone, so a failed lookup must not downgrade
		// a definite answer to an indefinite one.
		if statuses[i].State == DependencyMergeLanded && unchecked[statuses[i].ID] {
			statuses[i].State = DependencyMergeUnknown
			statuses[i].MR = ""
			statuses[i].Branch = ""
		}
	}
	return statuses
}

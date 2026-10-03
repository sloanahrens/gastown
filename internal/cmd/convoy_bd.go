package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/workspace"
)

// bd JSON helpers the convoy commands share. They outlived gt mountain's
// staging code, which was deleted with it (gt-638go.2, gt-31vjc).

// bdShowResult is the bead shape the convoy commands read.
type bdShowResult struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	IssueType   string   `json:"issue_type"`
	Labels      []string `json:"labels"`
	Description string   `json:"description"`
}

// storeForBead is the read store for beadID: the bd wrapper pinned to the rig
// database its prefix routes to, via the same routes.jsonl lookup that names
// the directory bd would run in.
func storeForBead(beadID string) *beads.Beads {
	return beads.NewPinned(resolveBeadDir(beadID))
}

// bdShowResultFromIssue reduces an Issue to the fields the convoy commands read.
func bdShowResultFromIssue(issue *beads.Issue) *bdShowResult {
	return &bdShowResult{
		ID:          issue.ID,
		Title:       issue.Title,
		Status:      issue.Status,
		IssueType:   issue.Type,
		Labels:      issue.Labels,
		Description: issue.Description,
	}
}

// bdShow reads one bead, with the store resolved from its prefix. Returns an
// error if the bead does not exist.
func bdShow(beadID string) (*bdShowResult, error) {
	issue, err := storeForBead(beadID).Show(beadID)
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w", beadID, err)
	}
	return bdShowResultFromIssue(issue), nil
}

// bdListChildren returns the direct children of parentID. bd is CWD-sensitive,
// so the store is resolved from the bead's prefix via routes.jsonl and works
// regardless of the caller's working directory.
//
// When the read returns no children, we fall back to a direct query against
// the dependencies table (parent-child links) and resolve each child via
// bdShow. This handles the case (GH #3700) where the children read doesn't see
// links that were added via `bd dep add ... --type=parent-child`. The deps
// table is authoritative.
func bdListChildren(parentID string) ([]bdShowResult, error) {
	issues, err := storeForBead(parentID).Children(parentID)
	if err != nil {
		return nil, fmt.Errorf("bd children %s: %w", parentID, err)
	}
	children := make([]bdShowResult, 0, len(issues))
	for _, issue := range issues {
		children = append(children, *bdShowResultFromIssue(issue))
	}
	return childrenOrDepsFallback(children, parentID, bdListChildrenViaDeps)
}

// childrenOrDepsFallback returns children, consulting viaDeps only when the
// primary read lists no children.
func childrenOrDepsFallback(children []bdShowResult, parentID string, viaDeps func(parentID string) ([]bdShowResult, error)) ([]bdShowResult, error) {
	if len(children) > 0 {
		return children, nil
	}
	return viaDeps(parentID)
}

// bdListChildrenViaDeps resolves children by querying the dependencies table
// directly for parent-child links, then loading each child via bdShow.
//
// Used as a fallback when `bd list --parent=<id>` returns empty even though
// children exist (GH #3700). Returns nil (not an error) when the prefix can't
// be resolved or no parent-child deps exist.
func bdListChildrenViaDeps(parentID string) ([]bdShowResult, error) {
	beadsDir := beadsDirForID(parentID)
	if beadsDir == "" {
		// Can't resolve the rig; nothing more we can do.
		return nil, nil
	}

	// Production data stores parent-child as a typed dependency target where
	// issue_id=parent. "down" returns target rows for the epic's children.
	childIDs, err := convoy.DepListRawIDs(beadsDir, parentID, "down", "parent-child")
	if err != nil {
		return nil, nil // best-effort — caller still gets the empty primary result
	}
	if len(childIDs) == 0 {
		return nil, nil
	}

	results := make([]bdShowResult, 0, len(childIDs))
	for _, id := range childIDs {
		child, err := bdShow(id)
		if err != nil || child == nil {
			continue
		}
		results = append(results, *child)
	}
	return results, nil
}

// beadsDirForID resolves the .beads directory that owns a given bead ID by
// looking up its prefix in routes.jsonl. This is needed for CWD-sensitive bd
// commands like `bd list --parent=` which only search the local database.
// Returns empty string if the prefix or workspace cannot be resolved.
func beadsDirForID(beadID string) string {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return ""
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return ""
	}
	rigPath := beads.GetRigPathForPrefix(townRoot, prefix)
	if rigPath == "" {
		return ""
	}
	return beads.ResolveBeadsDir(rigPath)
}

// Package beads provides merge request and gate utilities.
package beads

import (
	"strings"
)

// FindMRForBranch returns the open merge-request bead whose description names
// branch, nil when there is none. On a *Beads an instance warmed by
// PreloadMergeRequests answers from memory instead of rescanning.
func FindMRForBranch(c Client, branch string) (*Issue, error) {
	return findMRForBranch(c, branch, true)
}

// FindMRForBranchAny searches for a merge-request bead for the given branch
// across all statuses (open and closed). Used by recovery checks to determine
// if work was ever submitted to the merge queue. See #1035.
func (b *Beads) FindMRForBranchAny(branch string) (*Issue, error) {
	return b.findMRForBranch(branch, false)
}

// findMRForBranch searches for a merge-request bead matching branch. When
// skipClosed is true, closed beads are excluded (for open-MR checks).
func (b *Beads) findMRForBranch(branch string, skipClosed bool) (*Issue, error) {
	issues, err := b.mergeRequestsForBranchSearch()
	if err != nil {
		return nil, err
	}
	return scanMRsForBranch(issues, branch, skipClosed), nil
}

// findMRForBranch is FindMRForBranch's Client path: the full-table
// merge-request scan ListMergeRequests runs, with no preload cache to consult.
func findMRForBranch(c Client, branch string, skipClosed bool) (*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return b.findMRForBranch(branch, skipClosed)
	}
	issues, err := ListMergeRequests(c, ListOptions{
		Status: "all",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}
	return scanMRsForBranch(issues, branch, skipClosed), nil
}

// scanMRsForBranch returns the first merge-request bead whose description
// opens with the branch header, skipping closed ones when skipClosed is set.
func scanMRsForBranch(issues []*Issue, branch string, skipClosed bool) *Issue {
	branchPrefix := "branch: " + branch + "\n"
	for _, issue := range issues {
		if skipClosed && issue.Status == "closed" {
			continue
		}
		if strings.HasPrefix(issue.Description, branchPrefix) {
			return issue
		}
	}
	return nil
}

// mergeRequestsForBranchSearch returns the merge-request beads findMRForBranch
// scans, preferring a PreloadMergeRequests() cache over the full-table
// `bd list --label=gt:merge-request` scan it otherwise repeats on every call.
//
// mrCache.loaded (not "mrCache.issues != nil") gates the cache hit: a rig
// with zero merge-request beads legitimately preloads a nil issues slice
// (ListMergeRequests' underlying bd list falls back to nil on bd's "No
// issues found." plain-text response — see listIssues), and that must still
// read as "warmed, nothing there" rather than "never warmed, fetch again".
func (b *Beads) mergeRequestsForBranchSearch() ([]*Issue, error) {
	if b.mrCache != nil && b.mrCache.loaded {
		return b.mrCache.issues, nil
	}
	return b.ListMergeRequests(ListOptions{
		Status: "all",
		Label:  "gt:merge-request",
	})
}

// mrCacheState is PreloadMergeRequests' warmed snapshot. loaded distinguishes
// "warmed with zero results" from "never warmed" — see
// mergeRequestsForBranchSearch.
type mrCacheState struct {
	loaded bool
	issues []*Issue
}

// PreloadMergeRequests warms this *Beads' merge-request cache with a single
// ListMergeRequests(status=all) call, so every subsequent
// FindMRForBranch/FindMRForBranchAny made through this same instance answers
// from memory instead of repeating the full-table label scan. Intended for
// fleet-wide callers (e.g. check-recovery-batch, gt-b839) that need every
// polecat's branch checked within one process lifetime; the cache never
// refreshes, so don't enable it on a *Beads held across writes that could
// create/close merge-request beads mid-run.
func (b *Beads) PreloadMergeRequests() error {
	mrs, err := b.ListMergeRequests(ListOptions{
		Status: "all",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return err
	}
	b.mrCache = &mrCacheState{loaded: true, issues: mrs}
	return nil
}

// FindOpenMRsForIssue returns all open merge-request beads whose source_issue
// matches the given issue ID. Used to find prior attempts when re-dispatching
// an issue and to supersede old MRs when a new one is created.
func FindOpenMRsForIssue(c Client, issueID string) ([]*Issue, error) {
	issues, err := ListMergeRequests(c, ListOptions{
		Status: "open",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}

	var matches []*Issue
	for _, issue := range issues {
		if MatchesMRSourceIssue(issue.Description, issueID) {
			matches = append(matches, issue)
		}
	}
	return matches, nil
}

// MatchesMRSourceIssue returns true if the MR description contains a
// source_issue field matching the given issue ID exactly. The trailing
// newline in the needle prevents partial ID matches (e.g., "gt-abc"
// must not match "gt-abcdef").
func MatchesMRSourceIssue(description, issueID string) bool {
	needle := "source_issue: " + issueID + "\n"
	return strings.Contains(description, needle)
}

// Package beads provides merge request and gate utilities.
package beads

import (
	"strings"
)

// FindMRForBranch searches for an open merge-request bead for the given branch.
// Returns the MR bead if found, nil if not found.
// This enables idempotent `gt done` - if an MR already exists, we skip creation.
func (b *Beads) FindMRForBranch(branch string) (*Issue, error) {
	return b.findMRForBranch(branch, true)
}

// FindMRForBranchAny searches for a merge-request bead for the given branch
// across all statuses (open and closed). Used by recovery checks to determine
// if work was ever submitted to the merge queue. See #1035.
func (b *Beads) FindMRForBranchAny(branch string) (*Issue, error) {
	return b.findMRForBranch(branch, false)
}

// FindMRForBranchAndSHA searches for an open merge-request bead matching both
// the branch name AND the commit SHA. This is the correct dedup key: two MRs
// from the same branch but with different commit SHAs are distinct submissions
// (e.g., polecat fixed a gate failure and re-pushed). See GH#3032.
//
// Returns nil if no MR matches both branch and SHA. Callers should create a
// new MR in that case and supersede old MRs for the same source issue.
func (b *Beads) FindMRForBranchAndSHA(branch, commitSHA string) (*Issue, error) {
	issues, err := b.ListMergeRequests(ListOptions{
		Status: "all",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}

	branchPrefix := "branch: " + branch + "\n"
	for _, issue := range issues {
		if issue.Status == "closed" {
			continue
		}
		if !strings.HasPrefix(issue.Description, branchPrefix) {
			continue
		}
		// Branch matches — check commit SHA.
		// If the MR has no commit_sha field (legacy), fall back to branch-only
		// match for backward compatibility.
		fields := ParseMRFields(issue)
		if fields != nil && fields.CommitSHA != "" && commitSHA != "" {
			if fields.CommitSHA != commitSHA {
				// Same branch but different SHA — this is a stale MR.
				// Don't return it; caller will create a new MR and supersede.
				continue
			}
		}
		return issue, nil
	}

	return nil, nil
}

// findMRForBranch searches the wisps table (Dolt) for a merge-request
// bead matching the given branch.
// Uses status=all which includes all issue statuses with full descriptions.
// Ephemeral=true routes to the wisps table where MR beads live (GH#2446).
// When skipClosed is true, closed beads are excluded (for open-MR checks).
func (b *Beads) findMRForBranch(branch string, skipClosed bool) (*Issue, error) {
	branchPrefix := "branch: " + branch + "\n"

	issues, err := b.mergeRequestsForBranchSearch()
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if skipClosed && issue.Status == "closed" {
			continue
		}
		if strings.HasPrefix(issue.Description, branchPrefix) {
			return issue, nil
		}
	}

	return nil, nil
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
func (b *Beads) FindOpenMRsForIssue(issueID string) ([]*Issue, error) {
	issues, err := b.ListMergeRequests(ListOptions{
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

// NonTerminalMRsForIssue returns every merge-request bead whose source_issue
// matches issueID and whose status is not terminal (closed/tombstone) —
// unlike FindOpenMRsForIssue, this also catches an MR the refinery has
// already claimed: status transitions open -> in_progress the moment the
// refinery picks one up (internal/refinery/types.go:183), and a
// status=="open" filter would miss it.
//
// This is the fallback the gt-6hmz close-time invariant's exit (b) uses when
// pendingMRID is empty — i.e. active_mr was never recorded on the agent
// bead — rather than treating "unknown" the same as "no MR exists" (gt-h8ld).
func (b *Beads) NonTerminalMRsForIssue(issueID string) ([]*Issue, error) {
	issues, err := b.ListMergeRequests(ListOptions{
		Status: "all",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}

	var matches []*Issue
	for _, issue := range issues {
		if !MatchesMRSourceIssue(issue.Description, issueID) {
			continue
		}
		if IssueStatus(issue.Status).IsTerminal() {
			continue
		}
		matches = append(matches, issue)
	}
	return matches, nil
}

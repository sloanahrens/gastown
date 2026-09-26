package cmd

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// listBeadsAcrossTables lists matching durable issues and ephemeral wisps.
// Hooked molecule roots can live in either table, so readers must merge both
// sources instead of relying on issue-only bd list output.
//
// This costs two subprocesses. For a fleet-wide lookup use
// beads.ListAssignedIssueStatuses, which unions both tables in one query
// (gt-t8tu); this is left for the per-parent child scan.
func listBeadsAcrossTables(b *beads.Beads, opts beads.ListOptions) ([]*beads.Issue, error) {
	limit := opts.Limit

	issueOpts := opts
	issueOpts.Ephemeral = false
	issueOpts.Limit = 0
	issues, err := b.List(issueOpts)
	if err != nil {
		return nil, err
	}

	wispOpts := opts
	wispOpts.Ephemeral = true
	wispOpts.Limit = 0
	wisps, err := b.List(wispOpts)
	if err != nil {
		return nil, err
	}

	merged := mergeBeadLists(issues, wisps)
	if limit > 0 && len(merged) > limit {
		return merged[:limit], nil
	}
	return merged, nil
}

func listAssignedActiveWork(b *beads.Beads, assignee string) ([]*beads.Issue, error) {
	assignments, err := listAssignedActiveWorkAcrossStatuses(b, assignee)
	if err != nil {
		return nil, err
	}
	return preferHooked(assignments), nil
}

// listAssignedActiveWorkAcrossStatuses returns every active assignment for
// assignee, newest first, from one query over both statuses and both tables.
func listAssignedActiveWorkAcrossStatuses(b *beads.Beads, assignee string) ([]*beads.Issue, error) {
	assignments, err := b.ListAssignedIssueStatuses(assignee, activeWorkStatuses()...)
	if err != nil {
		return nil, err
	}
	return mergeBeadLists(assignments, nil), nil
}

// preferHooked puts a hooked bead ahead of an in_progress one. Callers read
// element 0 as "the" hook, and the merged query returns both statuses
// interleaved, so the split happens here.
func preferHooked(assignments []*beads.Issue) []*beads.Issue {
	var hooked, inProgress []*beads.Issue
	for _, issue := range assignments {
		if issue.Status == beads.StatusHooked {
			hooked = append(hooked, issue)
		} else {
			inProgress = append(inProgress, issue)
		}
	}
	if len(hooked) > 0 {
		return mergeBeadLists(hooked, nil)
	}
	return mergeBeadLists(inProgress, nil)
}

func listChildrenAcrossTables(b *beads.Beads, parentID string) ([]*beads.Issue, error) {
	return listBeadsAcrossTables(b, beads.ListOptions{
		Parent:   parentID,
		Status:   "all",
		Priority: -1,
	})
}

func resolveHookLookupWorkDir(workDir, target, townRoot string) string {
	target = strings.TrimSpace(target)
	if townRoot == "" || isTownLevelRole(target) {
		return workDir
	}
	if !safeAgentTargetPath(target) {
		return workDir
	}

	rigName := strings.Split(target, "/")[0]
	if rigName == "" || rigName == "mayor" || rigName == "deacon" {
		return workDir
	}
	if rigDir := beads.GetRigDirForName(townRoot, rigName); rigDir != "" {
		return rigDir
	}
	return filepath.Join(townRoot, rigName)
}

func activeWorkStatuses() []beads.IssueStatus {
	return []beads.IssueStatus{beads.IssueStatusHooked, beads.StatusInProgress}
}

func safeAgentTargetPath(target string) bool {
	parts := strings.Split(target, "/")
	for _, part := range parts {
		if !safeAgentPathSegment(part) {
			return false
		}
	}
	return len(parts) > 0
}

func safeAgentPathSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func mergeBeadLists(primary, secondary []*beads.Issue) []*beads.Issue {
	merged := make([]*beads.Issue, 0, len(primary)+len(secondary))
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	for _, issue := range append(primary, secondary...) {
		if issue == nil || issue.ID == "" {
			continue
		}
		if _, ok := seen[issue.ID]; ok {
			continue
		}
		seen[issue.ID] = struct{}{}
		merged = append(merged, issue)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		left := beadRecencyTime(merged[i])
		right := beadRecencyTime(merged[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return beadSortID(merged[i]) > beadSortID(merged[j])
	})
	return merged
}

func beadRecencyTime(issue *beads.Issue) time.Time {
	if issue == nil {
		return time.Time{}
	}
	if ts := parseBeadTime(issue.UpdatedAt); !ts.IsZero() {
		return ts
	}
	return parseBeadTime(issue.CreatedAt)
}

func parseBeadTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func beadSortID(issue *beads.Issue) string {
	if issue == nil {
		return ""
	}
	return issue.ID
}

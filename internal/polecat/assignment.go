package polecat

import (
	"sort"
	"strings"
)

// IsStaleBranchIssue reports whether a branch-derived issue id is overridden by
// the seat's hooked bead (hq-l0fj): both exist, they differ, and the branch id
// is not a subtask of the hooked bead.
func IsStaleBranchIssue(branchIssue, hookedIssue string) bool {
	if branchIssue == "" || hookedIssue == "" {
		return false
	}
	return branchIssue != hookedIssue && !strings.HasPrefix(branchIssue, hookedIssue+".")
}

// SelectAssignedIssue picks the one assignment a submission is attributed to:
// "" when the branch's issue is held by the seat or no assignment exists, the
// single remaining assignment, or "" with ambiguous set when several conflict.
func SelectAssignedIssue(branchIssue string, assigned []string) (issue string, ambiguous bool) {
	unique := make(map[string]bool, len(assigned))
	for _, id := range assigned {
		if id != "" {
			unique[id] = true
		}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	if len(ids) == 0 {
		return "", false
	}
	if branchIssue != "" {
		for _, id := range ids {
			if branchIssue == id || strings.HasPrefix(branchIssue, id+".") {
				return "", false
			}
		}
	}
	if len(ids) > 1 {
		return "", true
	}
	return ids[0], false
}

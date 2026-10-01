package done

import (
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat"
)

// branchInfo holds parsed branch information.
type branchInfo struct {
	Branch string // Full branch name
	Issue  string // Issue ID extracted from branch
	Worker string // Worker name (polecat name)
}

// issuePattern matches issue IDs in branch names (e.g., "gt-xyz" or "gt-abc.1")
var issuePattern = regexp.MustCompile(`([a-z]+-[a-z0-9]+(?:\.[0-9]+)?)`)

// parseBranchName extracts issue ID and worker from a branch name.
//
// Supports formats:
//   - polecat/<worker>/<issue>[+|@]<suffix>  → issue=<issue>, worker=<worker>
//   - polecat/<worker>/<issue>  → issue=<issue>, worker=<worker>
//   - polecat/<worker>-<suffix>  → issue="", worker=<worker>
//   - <issue>                   → issue=<issue>, worker=""
func parseBranchName(branch string) branchInfo {
	info := branchInfo{Branch: branch}

	if meta, ok := polecat.ParseBranchName(branch); ok {
		info.Worker = meta.Polecat
		info.Issue = meta.Issue
		return info
	}
	if strings.HasPrefix(branch, "polecat/") {
		return info
	}

	// Try to find an issue ID pattern in the branch name
	// Common patterns: prefix-xxx, prefix-xxx.n (subtask)
	if matches := issuePattern.FindStringSubmatch(branch); len(matches) > 1 {
		info.Issue = matches[1]
	}

	return info
}

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

// issuePattern matches a branch name that is exactly an issue ID (e.g.,
// "gt-xyz" or "gt-abc.1"). It is anchored: a branch such as fix/add-retry
// carries no issue, and an unanchored match would read "add-retry" as one.
var issuePattern = regexp.MustCompile(`^([a-z]+-[a-z0-9]+(?:\.[0-9]+)?)$`)

// embeddedIssuePattern finds an issue ID anywhere in a branch name. Only crew
// branches (crew/<user>/<id>-<slug>) use it, and crewBeadFromBranch checks the
// prefix it finds against the town's routes.
var embeddedIssuePattern = regexp.MustCompile(`([a-z]+-[a-z0-9]+(?:\.[0-9]+)?)`)

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

	// A non-polecat branch carries an issue only when it is the bare id.
	// Common patterns: prefix-xxx, prefix-xxx.n (subtask)
	if matches := issuePattern.FindStringSubmatch(branch); len(matches) > 1 {
		info.Issue = matches[1]
	}

	return info
}

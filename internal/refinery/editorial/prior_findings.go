package editorial

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// priorFindingLineRE matches the interim MERGE REJECTION finding-line format
// ("- id:<hex> sev:<severity> <path>:<line> — <title>"); om-gate T10 owns
// writing these lines and may refine the format.
var priorFindingLineRE = regexp.MustCompile(`^-\s*id:(\S+)\s+sev:(\S+)\s+([^:]+):(\d+)\s+—\s+(.*)$`)

// BuildPriorFindings collects prior MERGE REJECTION findings for sourceIssue
// so the reviewer classifies them resolved/unresolved/regressed instead of
// rediscovering them from scratch. Best-effort: an unparsable or missing
// source issue yields no prior findings rather than an error.
//
// Exported so every ReviewRequest builder shares one implementation instead
// of drifting — `gt mq review` (cmd/mq_review.go) and the batch path
// (om-gate T7, refinery/batch_editorial.go) both call this rather than each
// keeping its own copy.
func BuildPriorFindings(bd *beads.Beads, sourceIssue string, attempt int) []PriorFinding {
	if sourceIssue == "" {
		return nil
	}
	issue, err := bd.Show(sourceIssue)
	if err != nil || issue == nil {
		return nil
	}
	var findings []PriorFinding
	for _, line := range strings.Split(issue.Notes, "\n") {
		m := priorFindingLineRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		lineNo, _ := strconv.Atoi(m[4])
		findings = append(findings, PriorFinding{
			ID:       m[1],
			Severity: m[2],
			Path:     m[3],
			Line:     lineNo,
			Title:    m[5],
			Attempt:  attempt,
		})
	}
	return findings
}

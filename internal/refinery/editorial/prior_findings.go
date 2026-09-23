package editorial

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// priorFindingLineRE matches the MERGE REJECTION finding-line format
// ("- id:<hex> sev:<severity> <path>:<line> — <title>"). The id identifies
// the FINDING, not the diff it was found on — see RejectionFinding.ID for
// what a writer of these lines must not use (gt-2ok0).
//
// Either bullet matches. The writer is a formula an agent executes, and it
// has emitted these lines under '•' — a line that then parsed as nothing, so
// the rejection carried no findings at all (gt-3mp1). The rest of the line is
// format enough that a bullet choice cannot make a non-finding line match.
//
// The prose finding lines the same notes carry ("FINDING [major] <path>
// (~line N): <title>") are deliberately not parsed: they carry no om finding
// id, and an id invented here would be one om has never issued, so its
// resolved/unresolved classification cannot key on it.
//
// The path group is greedy ((.+), not [^:]+) so a path containing its own
// colon (e.g. a Windows-style "C:\..." path) still parses: greedy backtracks
// to the LAST ":<digits>" in the line, which is always the line number, not
// the first colon encountered. The trailing title is \s* (not \s+) after the
// em dash so an empty title still matches — formatMergeRejectionNote's " — "
// separator loses its trailing space to strings.TrimSpace before this regex
// ever sees the line, so an empty title leaves nothing after the em dash at
// all (gt-j6ez: both cases previously failed to match, silently dropping the
// finding from BuildPriorFindings).
var priorFindingLineRE = regexp.MustCompile(`^[-•]\s*id:(\S+)\s+sev:(\S+)\s+(.+):(\d+)\s+—\s*(.*)$`)

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
	return dedupePriorFindings(findings)
}

// dedupePriorFindings keeps one entry per id — the last line naming that id
// wins, in first-seen order.
//
// om keys its resolved/unresolved/regressed classification on the id and
// rejects a --prior-findings payload that repeats one, and the gate fails
// closed, so a repeat here does not spoil one review: it blocks every later
// review of that source issue. The notes it was parsed from are append-only,
// so the offending line cannot even be edited out (gt-2ok0). A repeated id is
// a bookkeeping artifact of the writer, and the cost of collapsing it is one
// description the reviewer would have rediscovered anyway.
func dedupePriorFindings(findings []PriorFinding) []PriorFinding {
	if len(findings) == 0 {
		return findings
	}
	seen := make(map[string]int, len(findings))
	unique := make([]PriorFinding, 0, len(findings))
	for _, f := range findings {
		if i, dup := seen[f.ID]; dup {
			unique[i] = f
			continue
		}
		seen[f.ID] = len(unique)
		unique = append(unique, f)
	}
	return unique
}

package specdispatch

import (
	"fmt"
	"regexp"

	"github.com/steveyegge/gastown/internal/beads"
)

// evidenceBoxReason is the fix a refused box gets (gt-7qtrif).
const evidenceBoxReason = "item %q asks for pasted or recorded evidence of a run, which a seat that runs nothing cannot show; delete the evidence clause and keep the behavior and its test command"

// evidenceBoxRecord is a record location and evidenceBoxRun a run object; a box citing both records a run.
const (
	evidenceBoxRecord = `\b(?:commit (?:body|message)|bead notes?|bead comments?)\b`
	evidenceBoxRun    = `\b(?:output|result|log|failure|error|tail)s?\b`
)

// evidenceBoxAsksRun matches a request to paste, a red or failing line, or a
// record location beside a run object (gt-13dre6 Q44).
var evidenceBoxAsksRun = regexp.MustCompile(`(?i)\bpast(?:e|es|ed|ing)\b|\b(?:red|failing)[ -]lines?\b|` +
	evidenceBoxRecord + `[^.;]{0,60}` + evidenceBoxRun + `|` + evidenceBoxRun + `[^.;]{0,60}` + evidenceBoxRecord)

// evidenceBoxRefusal names the first box asking for pasted or recorded
// evidence of a run; boxSubject keeps a finding title from reading as one.
func evidenceBoxRefusal(text string) (Refusal, bool) {
	for _, item := range beads.AcceptanceItems(text) {
		criterion := acceptanceItemText(item)
		if evidenceBoxAsksRun.MatchString(boxSubject(criterion)) {
			return Refusal{Field: acceptanceBoxField, Reason: fmt.Sprintf(evidenceBoxReason, criterion)}, true
		}
	}
	return Refusal{}, false
}

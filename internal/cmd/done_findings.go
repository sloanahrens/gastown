package cmd

import (
	"io"
	"strings"
	"unicode"

	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/style"
)

// gt done --finding records a defect a polecat met outside its task on the
// source bead, so the polecat reports it without filing a bead of its own. A
// later step files one bead per FINDING: comment; nothing here parses the text.

const (
	// maxDoneFindings is how many --finding values one run records. A polecat
	// with more has a task-sized problem; the cap keeps one run from writing
	// an unbounded comment chain.
	maxDoneFindings = 5
	// maxDoneFindingLen is the longest finding text recorded, in runes.
	maxDoneFindingLen = 1200
	// doneFindingMarker is the comment's first line, the marker a later reader
	// finds the finding by.
	doneFindingMarker = "FINDING:"
)

// sanitizeDoneFinding returns the text gt done records for one --finding: the
// C category removed (control, format, private-use and surrogate code points,
// so a bidi override or zero-width joiner cannot reorder the comment), except
// that tab and newline stay, so a multi-line finding remains readable. The
// result is at most maxDoneFindingLen runes, surrounding whitespace trimmed. A
// first line naming the owning rig ("rig: <name>") is kept as written; this
// function does not parse it.
func sanitizeDoneFinding(finding string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.Is(unicode.C, r) {
			return r
		}
		return -1
	}, finding)
	if runes := []rune(cleaned); len(runes) > maxDoneFindingLen {
		cleaned = string(runes[:maxDoneFindingLen])
	}
	return strings.TrimSpace(cleaned)
}

// recordDoneFindings records each --finding as one FINDING: comment on the
// source bead, in the order the flags were given. The issue id routes to its
// owning store, opened through open. The text is recorded as given: gt done's
// other notes screen nothing for secrets, and neither does this.
//
// It reports through w and never refuses: a finding that cannot be written,
// or is dropped at the cap, is a warning, because the polecat's submitted
// work must still land.
func recordDoneFindings(w io.Writer, cwd, issueID string, findings []string, open done.SourceStoreOpener) {
	if len(findings) == 0 {
		return
	}
	if issueID == "" {
		style.FprintWarning(w, "cannot record %d --finding(s): this run resolved no source bead", len(findings))
		return
	}
	bd, _, _ := done.RoutedIssueBeadsIn(cwd, issueID, open)
	if bd == nil {
		style.FprintWarning(w, "cannot record %d --finding(s) on %s: no bead store", len(findings), issueID)
		return
	}
	if len(findings) > maxDoneFindings {
		style.FprintWarning(w, "%d --finding(s) given; recording the first %d and dropping the rest", len(findings), maxDoneFindings)
		findings = findings[:maxDoneFindings]
	}
	for i, raw := range findings {
		text := sanitizeDoneFinding(raw)
		if text == "" {
			style.FprintWarning(w, "--finding %d is empty once control characters are removed; not recorded", i+1)
			continue
		}
		if err := bd.AddComment(issueID, doneFindingMarker+"\n"+text); err != nil {
			style.FprintWarning(w, "could not record --finding %d on %s: %v", i+1, issueID, err)
		}
	}
}

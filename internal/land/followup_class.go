package land

import (
	"fmt"
	"strings"
)

// followupNoteLineLimit is the most findings one note names; the rest are
// counted, so a landing's minors cannot crowd out the bead's other comments (gt-i8ywix).
const followupNoteLineLimit = 20

// followupNoted reports whether a finding is a note on the landing's own bead
// rather than a follow-up bead: om's comment and test-style minors are the note
// (operator decision, 2026-11-19; the bound stays 40), and every other class,
// including none, is filed (gt-i8ywix).
func followupNoted(class string) bool {
	return class == FindingClassComment || class == FindingClassTestStyle
}

// splitNoted splits a landing's findings into the ones filed as beads and the
// ones noted on the bead, keeping om's order on both sides (gt-i8ywix).
func splitNoted(findings []Finding) (filed, noted []Finding) {
	for _, f := range findings {
		if followupNoted(f.Class) {
			noted = append(noted, f)
			continue
		}
		filed = append(filed, f)
	}
	return filed, noted
}

// followupNoteComment is the one comment a landing leaves on its own bead for
// the findings it filed no bead for: each finding's title, location and class,
// the first followupNoteLineLimit of them and a count of the rest (gt-i8ywix).
func followupNoteComment(w Work, res Result, noted []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "minor notes om raised on %s (approved at %.2f, landed %s), filed as no bead:",
		w.BeadID, res.Verdict.Score, shortSHA(res.LandedCommit))
	shown := noted[:min(len(noted), followupNoteLineLimit)]
	for _, f := range shown {
		loc := location(NoteField(f.Path), f.Line)
		if loc != "" {
			loc += ", "
		}
		fmt.Fprintf(&b, "\n- %s (%s%s)", NoteField(f.Title), loc, NoteField(f.Class))
	}
	if rest := len(noted) - len(shown); rest > 0 {
		fmt.Fprintf(&b, "\nand %d more", rest)
	}
	return b.String()
}

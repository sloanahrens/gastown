package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/steveyegge/gastown/internal/hmhints"
)

// hmSlowHintHeading introduces the town's prior records when a bead's text
// mentions a slow or timed-out test. Spec lint and polecat prime share it, so
// one hint reads the same at both points (gt-p07bw, gt-jamix).
const hmSlowHintHeading = "hint: this bead mentions slow or timed-out tests; prior records:"

// hmHitLine renders one hm hit for a hint line, shared so the spec lint and
// polecat prime hints cannot drift apart (gt-jamix). The kind, id and title are
// another agent's text from the hm index, so each is escaped: a newline in a
// title must not forge a hint line, and an escape sequence must not move the
// terminal (gt-xoqk0).
func hmHitLine(h hmhints.Hit) string {
	return fmt.Sprintf("hm #%d %s %s: %s", h.Rank, hintText(h.Kind), hintText(h.ID), hintText(h.Title))
}

// hintText makes one hm field safe for a hint line: every rune that is not
// printable becomes its Go escape, so the line stays one printable line
// whatever the record's author wrote.
func hintText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		// QuoteRune wraps the escape in single quotes; the escape is what is
		// between.
		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

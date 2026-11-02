package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/hmhints"
)

// hmSlowHintHeading introduces the town's prior records when a bead's text
// mentions a slow or timed-out test. Spec lint and polecat prime share it, so
// one hint reads the same at both points (gt-p07bw, gt-jamix).
const hmSlowHintHeading = "hint: this bead mentions slow or timed-out tests; prior records:"

// hmHitLine renders one hm hit for a hint line, shared so the spec lint and
// polecat prime hints cannot drift apart (gt-jamix).
func hmHitLine(h hmhints.Hit) string {
	return fmt.Sprintf("hm #%d %s %s: %s", h.Rank, h.Kind, h.ID, h.Title)
}

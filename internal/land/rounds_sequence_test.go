package land

import (
	"fmt"
	"strings"
	"testing"
)

// omRoundNumberProblem restates om's rule for the round numbers of a
// --rounds-file (internal/verdict/rounds.go ParseRounds in the om rig): every
// number is at least 1 and appears once. It returns the refusal om would print,
// or "" when om would accept the numbers.
func omRoundNumberProblem(rounds rendered) string {
	seen := map[int]bool{}
	for i, r := range rounds {
		if r.Round < 1 {
			return fmt.Sprintf("entry %d: round must be 1 or more, got %d", i, r.Round)
		}
		if seen[r.Round] {
			return fmt.Sprintf("entry %d: duplicate round %d", i, r.Round)
		}
		seen[r.Round] = true
	}
	return ""
}

// TestRoundsSequenceTwoLanes: a bead whose rounds span two lanes, a polecat's
// rounds 1 to 3 and then a takeover's round 1 on a crew branch, renders as one
// sequence numbered 1 to 4, so om does not refuse the file for a repeated round.
func TestRoundsSequenceTwoLanes(t *testing.T) {
	t.Parallel()
	l := ledgerIn(t, "gt-abc")
	finding := []Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 1, Title: "not atomic"}}
	for i, head := range []string{"p1", "p2", "p3"} {
		must(t, l.Append(i+1, head, finding))
	}
	must(t, l.AppendLane("crew/overseer", 1, "crewhead", finding))

	rounds := renderedOf(t, l, 0)
	if msg := omRoundNumberProblem(rounds); msg != "" {
		t.Fatalf("om would refuse the rendered rounds: %s", msg)
	}
	var heads []string
	for i, r := range rounds {
		if r.Round != i+1 {
			t.Errorf("rendered round %d is numbered %d, want %d", i, r.Round, i+1)
		}
		heads = append(heads, r.Head)
	}
	if got := strings.Join(heads, " "); got != "p1 p2 p3 crewhead" {
		t.Errorf("rendered heads = %q, want p1 p2 p3 crewhead", got)
	}
}

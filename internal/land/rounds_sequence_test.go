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

// TestRoundsSequenceLanes: each case records rounds in several lanes and
// asserts the rendered sequence runs 1 to N with no repeat and the heads
// come out in lane order. A later lane's rounds are numbered after the
// earlier lanes', so a crew lane's rounds 1 and 2 after a polecat's round 1
// read as 2 and 3.
func TestRoundsSequenceLanes(t *testing.T) {
	t.Parallel()
	type step struct {
		lane  string
		round int
		head  string
	}
	cases := []struct {
		name  string
		steps []step
		heads string
	}{
		{
			name:  "crew lane with two rounds beside a polecat round",
			steps: []step{{"", 1, "p1"}, {"crew/overseer", 1, "c1"}, {"crew/overseer", 2, "c2"}},
			heads: "p1 c1 c2",
		},
	}
	finding := []Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 1, Title: "not atomic"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := ledgerIn(t, "gt-abc")
			for _, s := range tc.steps {
				must(t, l.AppendLane(s.lane, s.round, s.head, finding))
			}

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
			if got := strings.Join(heads, " "); got != tc.heads {
				t.Errorf("rendered heads = %q, want %q", got, tc.heads)
			}
		})
	}
}

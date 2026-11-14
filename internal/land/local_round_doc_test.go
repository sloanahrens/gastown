package land

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalRoundDocStatesTheRules holds the local round design page to the rules
// the code enforced. The bound is built from the constant that enforced it, so
// a changed rule fails here until the page states it; the unavailable rule's
// sentence is pinned as the page writes it. The page's switch section is not
// pinned: the setting left the schema (gt-i9rw9d), so a row for its key would
// check the page against a literal no code declares.
func TestLocalRoundDocStatesTheRules(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "local-round.md"))
	if err != nil {
		t.Fatalf("read the local round design page: %v", err)
	}
	doc := string(data)
	for _, want := range []struct {
		name   string
		phrase string
	}{
		{"the bound", wordForBound(t, localRoundsPerDay) + " per bead per day"},
		// The retired daemon patrol fetched the head before Allow counted the
		// round, so a fetch failure spent no round; the page states the rule.
		{"a fetch failure is not a round", "A request whose head cannot be fetched is not a round"},
		// gt done's gate enforces this rule (internal/cmd/done_local_round.go),
		// and no land constant carries its sentence to build the row from.
		{"the unavailable rule", "An unavailable answer never blocks a submit"},
	} {
		t.Run(want.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(doc, want.phrase) {
				t.Errorf("docs/design/local-round.md does not state %q (%q)", want.name, want.phrase)
			}
		})
	}
}

// TestLocalRoundDocMarksTheRoundRetired pins gt-dpl81b: the operator retired the
// local round on 2026-11-14, so the page opens with the Status line that says
// so. A page whose first line loses the retirement, or the date, fails here.
func TestLocalRoundDocMarksTheRoundRetired(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "local-round.md"))
	if err != nil {
		t.Fatalf("read the local round design page: %v", err)
	}
	first := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			first = strings.TrimSpace(line)
			break
		}
	}
	for _, want := range []string{"Status:", "retired", "2026-11-14"} {
		if !strings.Contains(first, want) {
			t.Errorf("the local round design page's first line does not state %q: %q", want, first)
		}
	}
}

// wordForBound is n in words for the page's bound sentence, and fails the test
// for a bound it has no word for so a raised bound cannot leave the page's
// number unchecked.
func wordForBound(t *testing.T, n int) string {
	t.Helper()
	words := [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n < 0 || n >= len(words) {
		t.Fatalf("localRoundsPerDay = %d has no word here; extend wordForBound and state the new bound on the design page", n)
	}
	return words[n]
}

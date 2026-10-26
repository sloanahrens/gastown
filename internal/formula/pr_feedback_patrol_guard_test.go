package formula

import (
	"regexp"
	"strings"
	"testing"
)

// patrolGuardCount matches the two bead-count assignments in the dispatch
// step: the safety valve's OPEN_COUNT and the dedup guard's EXISTING.
var patrolGuardCount = regexp.MustCompile(`(?m)^\s*(?:OPEN_COUNT|EXISTING)=.*$`)

// TestPRFeedbackPatrolCountsBeadsNotOutputLines guards gt-497s7: both guards
// counted `bd list`'s human output with `wc -l`, whose header and footer added
// 6 to every count. That made the dedup guard skip every finding and let the
// safety valve trip early. A guard counts beads from bd's JSON array instead,
// and each keeps its rule: the valve compares against max_open_beads, the dedup
// guard asks whether any bead exists.
func TestPRFeedbackPatrolCountsBeadsNotOutputLines(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("mol-pr-feedback-patrol")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)

	guards := patrolGuardCount.FindAllString(text, -1)
	if len(guards) != 2 {
		t.Fatalf("dispatch step carries %d bead-count assignments, want 2 (valve and dedup guard): %q", len(guards), guards)
	}
	for _, guard := range guards {
		if strings.Contains(guard, "wc -l") {
			t.Errorf("guard counts output lines, not beads: %s", guard)
		}
		// jq -e, not jq: an empty or failed read prints nothing and exits 4, so
		// the fallback hands the comparison a 0 instead of an empty string.
		for _, want := range []string{"bd list", "--json", "jq -e", "length", `echo "0"`} {
			if !strings.Contains(guard, want) {
				t.Errorf("guard does not count beads from bd's JSON array with an empty fallback (missing %q): %s", want, guard)
			}
		}
	}

	for _, rule := range []string{
		`if [ "$OPEN_COUNT" -ge {{max_open_beads}} ]; then`,
		`if [ "$EXISTING" -gt 0 ]; then`,
	} {
		if !strings.Contains(text, rule) {
			t.Errorf("dispatch step no longer carries %q", rule)
		}
	}
}

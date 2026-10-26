package formula

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The patrol's two guards once counted `bd list`'s human output with `wc -l`,
// whose header and footer added 6 to every count (gt-497s7). That made the
// dedup guard skip every finding and let the safety valve trip early. A count
// alone does not fix that: a failed `bd list` must not read as zero open beads,
// or the patrol dispatches duplicates and never trips its valve.
//
// The behaviour under a failing bd is held by the integration tier
// (TestIntegrationPRFeedbackPatrolGuardsFailClosed, which runs the blocks); the
// gate tier holds the fence below, so the fail-open fallback cannot come back
// unnoticed by a gate that never starts a shell.

// patrolTestMaxOpen is the {{max_open_beads}} the dispatch blocks are rendered
// with; the valve cases bracket it.
const patrolTestMaxOpen = 3

var patrolBashBlock = regexp.MustCompile("(?s)```bash\n(.*?)```")

// patrolDispatchBlocks returns the dispatch step's safety valve and dedup-loop
// blocks, rendered with test variables and picked by the count they assign so a
// reordered step cannot hand the tests the wrong one.
func patrolDispatchBlocks(t *testing.T) (valve, dedup string) {
	t.Helper()
	raw, err := GetEmbeddedFormulaContent("mol-pr-feedback-patrol")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	start := strings.Index(text, `id = "dispatch-work"`)
	if start < 0 {
		t.Fatal("the patrol has no dispatch-work step")
	}
	step := text[start:]
	if end := strings.Index(step, "[[steps]]"); end >= 0 {
		step = step[:end]
	}
	render := strings.NewReplacer(
		"{{patrol_label}}", "patrol-test",
		"{{max_open_beads}}", fmt.Sprint(patrolTestMaxOpen),
		"{{rig}}", "testrig",
	)
	for _, m := range patrolBashBlock.FindAllStringSubmatch(step, -1) {
		block := render.Replace(m[1])
		switch {
		case strings.Contains(block, "OPEN_COUNT="):
			valve = block
		case strings.Contains(block, "EXISTING="):
			dedup = block
		}
	}
	if valve == "" || dedup == "" {
		t.Fatal("dispatch step must carry a safety valve assigning OPEN_COUNT and a dedup loop assigning EXISTING")
	}
	return valve, dedup
}

// TestPRFeedbackPatrolGuardsReadBeadsFromJSON holds the shape both guards need:
// a count of beads from bd's JSON array with no row cap (bd list defaults to
// 50, so a capped read undercounts a backlog and the valve never trips), and a
// failure branch that stops the guard instead of supplying a zero.
func TestPRFeedbackPatrolGuardsReadBeadsFromJSON(t *testing.T) {
	t.Parallel()
	valve, dedup := patrolDispatchBlocks(t)

	for name, block := range map[string]string{"safety valve": valve, "dedup guard": dedup} {
		for _, want := range []string{"bd list", "--json", "--limit 0", "jq -e 'length'"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s does not count beads from bd's JSON array (missing %q):\n%s", name, want, block)
			}
		}
		if strings.Contains(block, "wc -l") {
			t.Errorf("%s counts output lines, not beads:\n%s", name, block)
		}
		if strings.Contains(block, `echo "0"`) {
			t.Errorf("%s supplies a zero when bd list fails, so the patrol cannot tell a failed read from an empty one:\n%s", name, block)
		}
	}

	// Each guard keeps its rule once the count is readable.
	both := valve + dedup
	for _, rule := range []string{
		fmt.Sprintf(`if [ "$OPEN_COUNT" -ge %d ]; then`, patrolTestMaxOpen),
		`if [ "$EXISTING" -gt 0 ]; then`,
	} {
		if !strings.Contains(both, rule) {
			t.Errorf("dispatch step no longer carries %q", rule)
		}
	}
}

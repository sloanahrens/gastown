package tiertrigger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSweepScriptHoldsNoTriggerLogic holds scripts/tier-sweep.sh to its share
// of the trigger: two calls into this command and nothing else. The decision
// stays in Go, where the unit tier can reach it (gt-e69b2.3).
func TestSweepScriptHoldsNoTriggerLogic(t *testing.T) {
	t.Parallel()
	src := readSweepScript(t)

	// These words appear only when the trigger's decision, its state or its
	// git reads have moved back into the script.
	for _, word := range []string{"state.json", "DiffNameOnly", "DailyRunDue", "DailyZone", ".runtime", "GT_TOWN_ROOT", "git diff", "git log", "go list"} {
		if i := strings.Index(src, word); i >= 0 {
			t.Errorf("the sweep script mentions %q on line %d: the trigger's decision belongs to internal/tiertrigger (gt-e69b2.3)",
				word, 1+strings.Count(src[:i], "\n"))
		}
	}

	var calls, skips, records int
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "tiertrigger/cmd/tiertrigger") {
			continue
		}
		calls++
		if strings.Contains(line, "skips integration-docker") {
			skips++
		}
		if strings.Contains(line, "record integration-docker") {
			records++
		}
	}
	if calls != 2 || skips != 1 || records != 1 {
		t.Errorf("the sweep script has %d trigger calls (%d skips, %d record); want exactly one skips and one record call",
			calls, skips, records)
	}
}

func readSweepScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "tier-sweep.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

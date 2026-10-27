package tiertrigger

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sweepLogicWords appear in the sweep script only when the trigger's decision,
// its state or its git reads have moved back into it.
var sweepLogicWords = []string{"state.json", "DiffNameOnly", "DailyRunDue", "DailyZone", ".runtime", "GT_TOWN_ROOT", "git diff", "git log", "go list"}

// sweepScriptProblems holds a sweep script to its share of the trigger: one
// skips call and one record call into this command, and none of the trigger's
// logic. The call lines are found by the command's package path, which is what
// the script writes (go run ./internal/tiertrigger/cmd/tiertrigger ...); the
// decision stays in Go, where the unit tier can reach it (gt-e69b2.3).
func sweepScriptProblems(src string) []string {
	var problems []string
	for _, word := range sweepLogicWords {
		if i := strings.Index(src, word); i >= 0 {
			problems = append(problems, "the sweep script mentions "+word+" on line "+strconv.Itoa(1+strings.Count(src[:i], "\n"))+": the trigger's decision belongs to internal/tiertrigger")
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
	// Exactly one of each: a second skips call would decide twice, and a
	// record call in the wrong place would stamp a run that did not pass.
	if calls != 2 || skips != 1 || records != 1 {
		problems = append(problems, "the sweep script has "+strconv.Itoa(calls)+" trigger calls ("+strconv.Itoa(skips)+" skips, "+strconv.Itoa(records)+" record); want exactly one skips and one record call")
	}
	return problems
}

// TestSweepScriptHoldsNoTriggerLogic runs the guard on the real script.
func TestSweepScriptHoldsNoTriggerLogic(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "scripts", "tier-sweep.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if problems := sweepScriptProblems(string(data)); len(problems) > 0 {
		t.Errorf("scripts/tier-sweep.sh breaks its share of the trigger (gt-e69b2.3):\n%s", strings.Join(problems, "\n"))
	}
}

// TestSweepScriptGuardFailsOnAScriptThatBreaksIt drives the guard's failing
// branches: a script that is right passes, and each way of breaking it is named.
func TestSweepScriptGuardFailsOnAScriptThatBreaksIt(t *testing.T) {
	t.Parallel()
	const skips = `export TIER_SWEEP_SKIP="$(go run ./internal/tiertrigger/cmd/tiertrigger skips integration-docker)"` + "\n"
	const record = `go run ./internal/tiertrigger/cmd/tiertrigger record integration-docker` + "\n"
	cases := []struct {
		name   string
		script string
		want   string // a substring of a problem; "" means the script is right
	}{
		{"one skips and one record call", skips + record, ""},
		{"logic moved into the script", skips + record + "git diff --name-only HEAD\n", "git diff"},
		{"a second skips call", skips + skips + record, "3 trigger calls"},
		{"no record call", skips, "1 trigger calls"},
		{"no calls at all", "echo sweep\n", "0 trigger calls"},
		{"calls through another path", "go run ./cmd/other skips integration-docker\ngo run ./cmd/other record integration-docker\n", "0 trigger calls"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			problems := sweepScriptProblems(c.script)
			if c.want == "" {
				if len(problems) > 0 {
					t.Fatalf("a right script was refused: %v", problems)
				}
				return
			}
			if !strings.Contains(strings.Join(problems, "\n"), c.want) {
				t.Fatalf("problems = %v, want one naming %q", problems, c.want)
			}
		})
	}
}

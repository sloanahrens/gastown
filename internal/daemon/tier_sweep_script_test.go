package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tierSweepScriptContracts are the lines of scripts/tier-sweep.sh the bead's
// log reading stands on, quoted from the script itself:
//
//   - the per-unit log name, which tierSweepUnitLogPath rebuilds: the sweep's
//     log directory, the marker tier, a dash, and the package with "./" dropped
//     and "/" turned into "_";
//   - the grep that decides what the stage log carries, which is why a
//     slowcheck finding is reachable only in the unit's own log.
//
// The sweep script is a file this repo can read, so the daemon's naming is read
// from it rather than from a bead. A test pins the two lines, so a script change
// that moves either shows up here instead of silently turning every slowcheck
// read into the fallback (gt-j6vlz).
var tierSweepScriptContracts = []string{
	`log="$TIER_SWEEP_LOGDIR/$tier-$(echo "${name#./}" | tr / _).log"`,
	`grep -a -E '^(--- FAIL|panic:|FAIL)' "$log"`,
}

// tierSweepScriptProblems holds src, a sweep script, to the contracts above.
func tierSweepScriptProblems(src string) []string {
	var problems []string
	for _, want := range tierSweepScriptContracts {
		if !strings.Contains(src, want) {
			problems = append(problems, "the sweep script no longer carries `"+want+"`")
		}
	}
	return problems
}

// TestTierSweepScriptNamesTheUnitLogs checks the real script.
func TestTierSweepScriptNamesTheUnitLogs(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "scripts", "tier-sweep.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if problems := tierSweepScriptProblems(string(data)); len(problems) > 0 {
		t.Errorf("scripts/tier-sweep.sh breaks what the bead's log read stands on (gt-j6vlz):\n%s", strings.Join(problems, "\n"))
	}
}

// TestTierSweepScriptGuardNamesEachDeparture drives the guard's failing branches:
// a script that moved the log name, and one that stopped filtering the stage
// output, are each named, while a script that carries both is clean.
func TestTierSweepScriptGuardNamesEachDeparture(t *testing.T) {
	t.Parallel()
	whole := strings.Join(tierSweepScriptContracts, "\n")
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{name: "both contracts", script: whole + "\n", want: ""},
		{name: "log name moved", script: tierSweepScriptContracts[1] + "\n", want: "the sweep script no longer carries"},
		{name: "stage grep moved", script: tierSweepScriptContracts[0] + "\n", want: "the sweep script no longer carries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := strings.Join(tierSweepScriptProblems(tc.script), "\n")
			if tc.want == "" {
				if problems != "" {
					t.Fatalf("problems = %q, want none for a script that carries both", problems)
				}
				return
			}
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("problems = %q, want one naming the departure (%q)", problems, tc.want)
			}
		})
	}
}

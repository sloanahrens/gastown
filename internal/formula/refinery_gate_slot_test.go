package formula

import (
	"regexp"
	"strings"
	"testing"
)

// TestRefineryPatrolRunTestsTakesSlotWithoutLoadWait (gt-22hdp.34): the
// run-tests step used to hold the gate until 1-minute load fell to 60, and crew
// suites kept load above that, so the merge queue starved. gt slot run already
// makes new crew suites wait while the gate runs, so the refinery takes its
// reserved slot straight away. A gate killed or timed out under load stays
// INCONCLUSIVE and is retried, never counted against the branch.
func TestRefineryPatrolRunTestsTakesSlotWithoutLoadWait(t *testing.T) {
	t.Parallel()
	runTests := requireFormulaStep(t, loadRefineryPatrolFormula(t), "run-tests")
	d := runTests.Description

	for _, forbidden := range []string{"loadavg", "load average", "uptime"} {
		if strings.Contains(d, forbidden) {
			t.Errorf("run-tests still gates on host load: found %q", forbidden)
		}
	}
	for _, required := range []string{"gt slot run --role {{rig}}/refinery", "INCONCLUSIVE", "retry"} {
		if !strings.Contains(d, required) {
			t.Errorf("run-tests missing %q", required)
		}
	}
}

// TestRefineryPatrolScratchFilesUseAPrivateDir (gt-22hdp.34): the gate log and
// the batch json/err/result files are read back to decide merges, so a
// predictable name in world-writable /tmp is one another local user can
// pre-create or symlink. They live in a per-user mode-0700 directory, like the
// review JSON already does.
func TestRefineryPatrolScratchFilesUseAPrivateDir(t *testing.T) {
	t.Parallel()
	f := loadRefineryPatrolFormula(t)
	bareTmp := regexp.MustCompile(`/tmp/<(rig|mr-bead-id)>-(gate\.log|batch)`)
	for _, id := range []string{"run-tests", "batch-scan"} {
		d := requireFormulaStep(t, f, id).Description
		if m := bareTmp.FindString(d); m != "" {
			t.Errorf("step %s writes a predictable /tmp path: %q", id, m)
		}
		if !strings.Contains(d, `GATE_DIR="${TMPDIR:-/tmp}/gt-refinery-$(id -u)"; mkdir -p -m 700 "$GATE_DIR"`) {
			t.Errorf("step %s does not create its per-user mode-0700 scratch dir", id)
		}
	}
}

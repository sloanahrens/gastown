package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// captureReviewOutput runs fn and returns everything it wrote to stdout and
// stderr. printMQReviewResult and runMQReview's failure branch write through
// os.Stdout/os.Stderr rather than the cobra command's streams, so
// rootCmd.SetOut does not see either.
func captureReviewOutput(t *testing.T, fn func()) string {
	t.Helper()
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, fn)
	})
	return stdout + stderr
}

// TestMQReviewPreVerdictFailuresExitTwo names every way gt mq review can fail
// without producing a verdict: the flag and argument errors cobra raises
// before RunE, and a failure raised inside RunE itself. Each one reports its
// cause once and exits 2.
//
// mqReviewCmd silences cobra's error line, so a verdict is not followed by
// "Error: exit 1", and cobra's own error path exits 1 — the request_changes
// code the refinery patrol acts on by closing the MR, reopening the source
// bead with MERGE REJECTION and redispatched the polecat. Left unrouted,
// `gt mq review --timeout=abc` printed nothing and exited 1, so a mistyped
// invocation rejected code the gate never reviewed (gt-twt2).
func TestMQReviewPreVerdictFailuresExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // a phrase the failure must name, exactly once
	}{
		{"unknown flag", []string{"mq", "review", "gt-mr-abc", "--nope"}, "--nope"},
		{"unparsable flag value", []string{"mq", "review", "gt-mr-abc", "--timeout=abc"}, "--timeout"},
		{"too many positional args", []string{"mq", "review", "a", "b", "c"}, "accepts between 0 and 2 arg(s), received 3"},
		{"no MR id and no --landed", []string{"mq", "review"}, "usage: gt mq review"},
		{"runE failure", []string{"mq", "review", "--landed", "deadbeef", "--rig", "no-such-rig"}, "no-such-rig"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rigDir := testRigRoot(t, "main")

			var code int
			var err error
			var captured *editorial.ReviewRequest
			out := captureReviewOutput(t, func() {
				code, err, captured = runReviewArgsErr(t, rigDir, noGate(t), tc.args...)
			})

			if code != 2 {
				t.Errorf("exit = %d, want 2: exit 1 is the request_changes code, and the gate wrote no verdict", code)
			}
			if silentCode, ok := IsSilentExit(err); !ok || silentCode != 2 {
				t.Errorf("error = %v (silent exit code %d, ok %v), want a SilentExitError carrying 2", err, silentCode, ok)
			}
			if captured != nil {
				t.Error("the gate ran")
			}
			if got := strings.Count(out, tc.want); got != 1 {
				t.Errorf("output names %q %d times, want exactly 1 (cobra's own error line is silenced, so the cause is reported once):\n%s", tc.want, got, out)
			}
		})
	}
}

// TestMQReviewUsageErrorCarriesItsClassInJSON pins the machine-readable half.
// The refinery patrol reads failure_class out of the review JSON to route a
// failure, and a usage error must land there as config_error — the class the
// rig's gate script gives its own usage errors.
func TestMQReviewUsageErrorCarriesItsClassInJSON(t *testing.T) {
	_, _, rigDir := testRigRoot(t, "main")

	var code int
	out := captureReviewOutput(t, func() {
		code, _, _ = runReviewArgsErr(t, rigDir, noGate(t), "mq", "review", "--json", "--nope")
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(out, `"Class": "config_error"`) {
		t.Errorf("output does not carry the config_error class:\n%s", out)
	}
}

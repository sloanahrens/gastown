package main

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// run only returns without starting a build for arguments it refuses, which is
// all a unit test can reach: the build itself is a subprocess.
func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"no arguments":           {},
		"no alarm":               {"--", "bash", "run.sh"},
		"zero alarm":             {"--alarm", "0", "--", "bash", "run.sh"},
		"negative alarm":         {"--alarm", "-3", "--", "bash", "run.sh"},
		"no command":             {"--alarm", "5"},
		"fingerprint without gt": {"--alarm", "5", "--fingerprint", "fp", "--", "bash", "run.sh"},
		"gt without fingerprint": {"--alarm", "5", "--gt", "/bin/gt", "--", "bash", "run.sh"},
		"unknown flag":           {"--alarm", "5", "--nope", "--", "bash", "run.sh"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr strings.Builder
			if got := run(args, &stderr); got != 2 {
				t.Errorf("run(%v) = %d, want 2 (stderr: %s)", args, got, stderr.String())
			}
			if stderr.Len() == 0 {
				t.Errorf("run(%v) said nothing about why", args)
			}
		})
	}
}

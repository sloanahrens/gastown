//go:build integration

package tiersweep

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestIntegrationProcessBuildExecReportsTheBuildsOutcome: ProcessBuildExec
// returns the child's own exit code, 127 for a command that does not start,
// and buildKilledCode for a command still running at its deadline, which it
// kills.
func TestIntegrationProcessBuildExecReportsTheBuildsOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		argv    []string
		timeout time.Duration
		want    int
		wantOut string
	}{
		{name: "exit 0 passes", argv: []string{"sh", "-c", "exit 0"}, timeout: time.Minute, want: 0},
		{name: "exit 3 fails with 3", argv: []string{"sh", "-c", "exit 3"}, timeout: time.Minute, want: 3},
		{
			name: "a command that does not start is 127", timeout: time.Minute, want: 127,
			argv:    []string{"tier-sweep-no-such-command"},
			wantOut: "tier-sweep-no-such-command:",
		},
		{
			name: "a command still running at its deadline is killed", timeout: 50 * time.Millisecond,
			argv: []string{"sleep", "60"}, want: buildKilledCode,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if got := ProcessBuildExec(".", nil, tc.argv, &out, tc.timeout); got != tc.want {
				t.Errorf("ProcessBuildExec() = %d, want %d (output %q)", got, tc.want, out.String())
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tc.wantOut)
			}
		})
	}
}

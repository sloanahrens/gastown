//go:build integration

package tiersweep

import (
	"io"
	"testing"
)

// TestIntegrationProcessExecReturnsTheChildExitCode: ProcessExec returns the
// child's own exit code, so a failing package fails and a passing one passes.
func TestIntegrationProcessExecReturnsTheChildExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		script string
		want   int
	}{
		{name: "exit 3 fails with 3", script: "exit 3", want: 3},
		{name: "exit 0 passes", script: "exit 0", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ProcessExec(".", nil, []string{"sh", "-c", tc.script}, io.Discard); got != tc.want {
				t.Errorf("ProcessExec() = %d, want %d", got, tc.want)
			}
		})
	}
}

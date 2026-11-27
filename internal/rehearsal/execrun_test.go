package rehearsal

import (
	"context"
	"strings"
	"testing"
)

// TestRehearsalExecRunRefusesWithoutProcess drives the refusal branches that
// start no process: an empty argv, and a program that is not on PATH, which
// fails to resolve before anything runs. The real exit-status path is in the
// integration tier.
func TestRehearsalExecRunRefusesWithoutProcess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		argv   []string
		wantIn string
		notIn  string
	}{
		{name: "empty argv", argv: nil, wantIn: "empty argv"},
		{
			name:   "program not on PATH names its first two words",
			argv:   []string{"no-such-rehearsal-program", "subcommand", "--token=sekret"},
			wantIn: "run no-such-rehearsal-program subcommand",
			notIn:  "sekret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := ExecRun(context.Background(), tc.argv)
			if err == nil {
				t.Fatalf("ExecRun(%q) = %q, want an error", tc.argv, out)
			}
			if out != nil {
				t.Errorf("ExecRun output on error = %q, want nil", out)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("ExecRun error = %q, want it to name %q", err, tc.wantIn)
			}
			if tc.notIn != "" && strings.Contains(err.Error(), tc.notIn) {
				t.Errorf("ExecRun error = %q, want no argv past the second word (%q)", err, tc.notIn)
			}
		})
	}
}

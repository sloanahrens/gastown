package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// sessionRestartRun runs the restart with the production Args and RunE, but
// without the root's persistent pre-run and its town-wide checks: the test
// drives the restart itself.
func sessionRestartRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := &cobra.Command{Use: "restart", Args: sessionRestartCmd.Args, RunE: sessionRestartCmd.RunE}
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// A restart that parsed its arguments and then failed prints its one error
// line, while a mistyped invocation still prints the usage block. The block
// after a runtime failure buried the cause, leaving a caller that keeps only
// the last line with a flag description instead of the reason (gt-6hhp8).
func TestSessionRestartFailurePrintsNoUsage(t *testing.T) {
	t.Parallel()

	// An address the restart refuses once it is running: the argument count
	// is valid, so cobra's Args check passes and the failure is the command's
	// own.
	t.Run("failure after the arguments parsed", func(t *testing.T) {
		t.Parallel()
		out, err := sessionRestartRun(t, "")
		if err == nil {
			t.Fatal("restart with no address = success, want its address error")
		}
		if strings.Contains(out, "Usage:") {
			t.Errorf("a failed restart printed the usage block:\n%s", out)
		}
		if !strings.Contains(out, err.Error()) {
			t.Errorf("output = %q, want the failure's own line", out)
		}
	})

	t.Run("bad argument", func(t *testing.T) {
		t.Parallel()
		out, err := sessionRestartRun(t, "gastown/amber", "extra")
		if err == nil {
			t.Fatal("a restart with two arguments = success, want cobra's argument error")
		}
		if !strings.Contains(out, "Usage:") {
			t.Errorf("a mistyped restart printed no usage block:\n%s", out)
		}
	})
}

// A verb run with no command (the integration test's verb.run(nil, args)) must
// not crash the restart, and a real command is silenced (gt-5apwi).
func TestSilenceUsageToleratesANilCommandAndSilencesARealOne(t *testing.T) {
	t.Parallel()
	silenceUsage(nil)
	c := &cobra.Command{Use: "restart"}
	silenceUsage(c)
	if !c.SilenceUsage {
		t.Error("silenceUsage left a real command printing its usage block")
	}
}

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
func sessionRestartRun(args []string) (string, error) {
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
// after a runtime failure buried the cause and left the daemon's log with a
// flag description where the reason should be (gt-6hhp8).
func TestSessionRestartFailurePrintsNoUsage(t *testing.T) {
	// Executes the shared command's RunE, so the test cannot run in parallel.
	townRoot := setupSessionSeatTown(t, "amber")
	t.Chdir(townRoot)
	t.Setenv("GT_TOWN_ROOT", townRoot)

	t.Run("runtime failure", func(t *testing.T) {
		out, err := sessionRestartRun([]string{"gastown/ghost"})
		if err == nil {
			t.Fatal("restarting a polecat that is not there = success, want its not-found error")
		}
		if strings.Contains(out, "Usage:") {
			t.Errorf("a failed restart printed the usage block:\n%s", out)
		}
		if !strings.Contains(out, err.Error()) {
			t.Errorf("output = %q, want the failure's own line", out)
		}
	})

	t.Run("bad argument", func(t *testing.T) {
		out, err := sessionRestartRun([]string{"gastown/amber", "extra"})
		if err == nil {
			t.Fatal("a restart with two arguments = success, want cobra's argument error")
		}
		if !strings.Contains(out, "Usage:") {
			t.Errorf("a mistyped restart printed no usage block:\n%s", out)
		}
	})
}

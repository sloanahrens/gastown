package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestLifecycleVerbsRejectPositionalArgs is gt-fcxe9.14: runShutdown ignored
// its positional args, so a typo after a lifecycle verb ran a real shutdown
// behind a confirmation prompt. The town lifecycle verbs take no positional
// args; a stray word must fail cobra's Args check, which runs before RunE and
// so before any prompt or stop.
func TestLifecycleVerbsRejectPositionalArgs(t *testing.T) {
	t.Parallel()
	for _, c := range []*cobra.Command{shutdownCmd, downCmd, upCmd, thawCmd} {
		if c.Args == nil {
			t.Errorf("gt %s has no Args validator, so it accepts and ignores any positional", c.Name())
			continue
		}
		if err := c.Args(c, []string{"preflight"}); err == nil {
			t.Errorf("gt %s preflight: Args accepted a positional it would ignore", c.Name())
		}
		if err := c.Args(c, nil); err != nil {
			t.Errorf("gt %s with no args: %v", c.Name(), err)
		}
		if gtTakesArgs(c) {
			t.Errorf("command-tree lint still reads gt %s as taking args", c.Name())
		}
	}
}

// TestShutdownPreflightFailsBeforeRunE drives the real tree: "gt shutdown
// preflight" must error out of argument validation, and the RunE (which
// prompts and then stops the town) must never run.
func TestShutdownPreflightFailsBeforeRunE(t *testing.T) {
	t.Parallel()
	ran := false
	c := &cobra.Command{Use: "shutdown", Args: shutdownCmd.Args, RunE: func(*cobra.Command, []string) error {
		ran = true
		return nil
	}}
	c.SetArgs([]string{"preflight"})
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.Execute()
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Errorf("shutdown preflight error = %v, want one naming the stray arg", err)
	}
	if ran {
		t.Error("shutdown RunE ran for a stray positional")
	}
}

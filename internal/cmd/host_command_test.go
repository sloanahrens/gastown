package cmd

import "testing"

// TestNoHostCommand pins the removal of the Mac host agent: the root command
// tree has no `host` command, so `gt host agent` is no longer a command gt
// runs. The Mac's probes are written by gt-mac-watch, not by gt.
func TestNoHostCommand(t *testing.T) {
	t.Parallel()
	for _, c := range rootCmd.Commands() {
		if c.Name() == "host" {
			t.Errorf("root command tree has a %q command; the host agent is removed", c.Name())
		}
	}
}

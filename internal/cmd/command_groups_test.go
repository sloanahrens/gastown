package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestEveryCommandGroupIDIsDeclaredByItsParent: cobra checks the command tree
// when the binary runs and panics, for every command, if a command names a
// GroupID its parent never declared with AddGroup. A subcommand of `gt town`
// that copied GroupDiag from a root-level command made every gt call panic
// ("group id 'diag' is not defined for subcommand 'gt town runner'") while the
// unit tier stayed green, because no unit test walks the tree.
func TestEveryCommandGroupIDIsDeclaredByItsParent(t *testing.T) {
	t.Parallel()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if p := c.Parent(); p != nil && c.GroupID != "" && !p.ContainsGroup(c.GroupID) {
			t.Errorf("%s names group %q, which %s never declares with AddGroup",
				c.CommandPath(), c.GroupID, p.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

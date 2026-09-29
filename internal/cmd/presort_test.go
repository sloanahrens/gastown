package cmd

import "github.com/spf13/cobra"

// presortCommandTree calls Commands() on every command once, before any test
// runs. cobra sorts a command's children in place on the first call; doing it
// here, single-threaded, makes every later walk from parallel tests a pure
// read (see TestCommandTreeWalkIsReadOnlyUnderParallelTests).
//
// cobra's Execute adds the default help and completion commands to the root
// on its first call, and adding a command marks the root unsorted again, so a
// test that calls Execute re-opened the race for every parallel walker. Both
// are added here first; the Init calls are no-ops once they exist.
func presortCommandTree(c *cobra.Command) {
	if !c.HasParent() {
		c.InitDefaultHelpCmd()
		c.InitDefaultCompletionCmd()
	}
	for _, sub := range c.Commands() {
		presortCommandTree(sub)
	}
}

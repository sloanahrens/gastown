package cmd

import "github.com/spf13/cobra"

// presortCommandTree calls Commands() on every command once, before any test
// runs. cobra sorts a command's children in place on the first call; doing it
// here, single-threaded, makes every later walk from parallel tests a pure
// read (see TestCommandTreeWalkIsReadOnlyUnderParallelTests).
func presortCommandTree(c *cobra.Command) {
	for _, sub := range c.Commands() {
		presortCommandTree(sub)
	}
}

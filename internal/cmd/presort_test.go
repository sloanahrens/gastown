package cmd

import (
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// presortCommandTree calls Commands() on every command once, before any test
// runs. cobra sorts a command's children in place on the first call; doing it
// here, single-threaded, makes every later walk from parallel tests a pure
// read (see TestCommandTreeWalkIsReadOnlyUnderParallelTests).
//
// cobra's Execute adds the default help and completion commands to the root
// on its first call, and adding a command marks the root unsorted again, so a
// test that calls Execute re-opened the race for every parallel walker. Both
// are added here first; the Init calls are no-ops once they exist.
//
// Find, Flags and the Args validators merge each command's inherited
// persistent flags lazily and sort its flag sets on first use, so the merge
// runs here too, and the suggestion distance cobra sets on first use is
// set up front.
func presortCommandTree(c *cobra.Command) {
	if !c.HasParent() {
		c.InitDefaultHelpCmd()
		c.InitDefaultCompletionCmd()
	}
	c.InheritedFlags().VisitAll(func(*pflag.Flag) {})
	c.LocalFlags().VisitAll(func(*pflag.Flag) {})
	c.Flags().VisitAll(func(*pflag.Flag) {})
	c.PersistentFlags().VisitAll(func(*pflag.Flag) {})
	if c.SuggestionsMinimumDistance <= 0 {
		c.SuggestionsMinimumDistance = 2 // cobra's default, set on first suggestion
	}
	for _, sub := range c.Commands() {
		presortCommandTree(sub)
	}
}

// prepareTestCommandTree is what both TestMains call (the unit build in
// hermetic_main_test.go, the integration build in
// integration_testmain_test.go): it installs the strict completion command,
// as Execute does, and then presorts. The integration TestMain once skipped
// it, and the parallel tree walkers there raced cobra's first sort
// (gt-jz03n.6: "gt slot run", "gt rig add" reported not in the tree).
func prepareTestCommandTree() {
	strictCompletionCmd(rootCmd)
	presortCommandTree(rootCmd)
}

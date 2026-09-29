package cmd

import (
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// Parallel tests walk the global cobra tree (wlCmd.Commands(), rootCmd.Find,
// help output). cobra sorts a command's children in place the first time
// Commands() is called, so two first callers race: one iterates while the
// other sorts, and a subcommand briefly looks missing ("subcommand \"claim\"
// not found on wl command", gt-wisp-e4s gate, 2026-09-29). TestMain sorts the
// whole tree once before any test runs; after that every walk is a read.
// Under -race this test reports the write if the presort is missing.
func TestCommandTreeWalkIsReadOnlyUnderParallelTests(t *testing.T) {
	t.Parallel()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			walk(rootCmd)
		}()
	}
	wg.Wait()
}

// presortCommandTree must leave nothing for Execute to add: cobra's Execute
// adds the default help and completion commands to a root that lacks them,
// which marks the root unsorted and re-opens the race above.
func TestPresortCommandTreeAddsCobraDefaults(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{Use: "zeta"}, &cobra.Command{Use: "alpha"})
	presortCommandTree(root)
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"help", "completion"} {
		if !have[want] {
			t.Errorf("after presort the root lacks %q, so Execute would add it and unsort the tree", want)
		}
	}
}

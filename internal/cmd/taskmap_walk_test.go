package cmd

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/taskmap"
)

// TestEveryCommandHasATask keeps the task map complete. Every runnable,
// visible gt command must have an entry in internal/taskmap; there is no
// allowlist, so a command added without an entry fails here.
//
// Hidden commands are out of the map on purpose: they are plumbing that runs
// from a formula, hook or wrapper (daemon workers, the credential helper, the
// status line), and no agent should be told to reach for one.
func TestEveryCommandHasATask(t *testing.T) {
	t.Parallel()

	paths := taskmapCommandPaths(rootCmd)

	if problems := taskmap.Default().Validate(paths); len(problems) > 0 {
		t.Fatalf("%d problem(s) in the task map:\n%s\n"+
			"Every runnable, visible gt command must be placed, deliberately, so an agent finds tools by the job to be done instead of rediscovering the command tree: a new command gets an entry in internal/taskmap in the change that adds it.",
			len(problems), strings.Join(problems, "\n"))
	}
}

// taskmapCommandPaths returns every runnable, visible command path under root,
// without the binary name, sorted. Cobra's own help and completion commands
// are outside the map, and so are hidden commands (see the test above).
func taskmapCommandPaths(root *cobra.Command) []string {
	var paths []string
	var walk func(c *cobra.Command, prefix []string)
	walk = func(c *cobra.Command, prefix []string) {
		for _, child := range c.Commands() {
			name := child.Name()
			if child.Parent() == root && (name == "help" || name == "completion") {
				continue
			}
			p := append(append([]string(nil), prefix...), name)
			if child.Runnable() && !child.Hidden {
				paths = append(paths, strings.Join(p, " "))
			}
			walk(child, p)
		}
	}
	walk(root, nil)
	sort.Strings(paths)
	return paths
}

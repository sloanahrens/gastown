package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// deletedCommands lists command paths removed as dead or duplicate surface
// (gt-638go.6, wayfinder D7). Each must stay gone: re-adding one should be a
// deliberate decision that also deletes its line here.
var deletedCommands = [][]string{
	{"proxy-subcmds"},
	{"wl"},
	{"mayor", "acp"},
	{"convoy", "land"},
	{"convoy", "stage"},
	{"convoy", "launch"},
	{"convoy", "watch"},
	{"convoy", "unwatch"},
	{"activity"},
	{"assign"},
	{"audit"},
	{"broadcast"},
	{"changelog"},
	{"checkpoint"},
	{"dnd"},
	{"notify"},
	{"prune-branches"},
	{"release"},
	{"repair"},
	{"thanks"},
	{"whoami"},
	{"upgrade"},
	{"uninstall"},
	{"git-init"},
	{"bead"},
	{"cat"},
	{"close"},
	{"info"},
	{"issue"},
	{"enable"},
	{"disable"},
	{"shell"},
	{"resume"},
	{"synthesis"},
	{"worktree"},
	{"town"},
	{"namepool"},
	{"role"},
	{"remember"},
	{"memories"},
	{"forget"},
	{"trail"},
	{"orphans"},
	{"cleanup"},
	{"vitals"},
	{"start"},
	{"crew", "next"},
	{"crew", "prev"},
	{"dolt", "flatten"},
	{"dolt", "rebase"},
	{"dolt", "rollback"},
	{"dolt", "recover"},
	{"dashboard"},
}

// TestDeletedCommandsGone fails if any deleted command path resolves in the
// cobra tree again. Find may land on a parent or apply prefix matching, so the
// check compares the resolved command's full path with the deleted one.
//
// Not parallel: it walks the shared rootCmd, which other tests execute.
func TestDeletedCommandsGone(t *testing.T) {
	// Positive control: the same lookup must find commands that stayed, or a
	// broken lookup would pass every deleted path vacuously. These are the
	// survivors of the clusters the deletions thinned.
	for _, path := range [][]string{{"convoy", "check"}, {"convoy", "close"}, {"dolt", "status"}, {"cycle", "next"}, {"crew", "start"}, {"show"}, {"up"}, {"feed"}} {
		if !resolvesExactly(path) {
			t.Errorf("live command %q does not resolve; the lookup is broken", "gt "+strings.Join(path, " "))
		}
	}
	for _, path := range deletedCommands {
		if resolvesExactly(path) {
			t.Errorf("%q resolves in the command tree; it was deleted in gt-638go.6", "gt "+strings.Join(path, " "))
		}
	}
}

// resolvesExactly reports whether path names a command in rootCmd, as opposed
// to Find stopping on a parent or the root.
func resolvesExactly(path []string) bool {
	c, _, err := rootCmd.Find(path)
	return err == nil && c != nil && c.CommandPath() == "gt "+strings.Join(path, " ")
}

// TestNoPolecatSafeAnnotation pins the proxy removal: the polecatSafe
// annotation's only reader was gt proxy-subcmds, so no command may carry it.
func TestNoPolecatSafeAnnotation(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if _, ok := c.Annotations["polecatSafe"]; ok {
			t.Errorf("%q still carries the polecatSafe annotation, which nothing reads", c.CommandPath())
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(rootCmd)
}

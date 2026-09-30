package cmd

import (
	"strings"
	"testing"
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
}

// TestDeletedCommandsGone fails if any deleted command path resolves in the
// cobra tree again. Find may land on a parent or apply prefix matching, so the
// check compares the resolved command's full path with the deleted one.
//
// Not parallel: it walks the shared rootCmd, which other tests execute.
func TestDeletedCommandsGone(t *testing.T) {
	for _, path := range deletedCommands {
		want := "gt " + strings.Join(path, " ")
		c, _, err := rootCmd.Find(path)
		if err == nil && c != nil && c.CommandPath() == want {
			t.Errorf("%q resolves in the command tree; it was deleted in gt-638go.6", want)
		}
	}
}

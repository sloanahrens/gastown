package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// These tests resolve commands with lookupCommand and call RunE directly, so
// persistentPreRun (usage logging, town discovery) never runs. TestMain
// installs the strict completion command and sorts the tree, so every walk
// here only reads it. They never call rootCmd.Find on a path that names a
// command: Find records the name each command was called as, a write that
// races every other walker.

// lookupCommand resolves path through the tree by exact name or alias, as
// Find does with prefix matching off, and returns nil when a word names no
// child. It only reads the tree.
func lookupCommand(root *cobra.Command, path []string) *cobra.Command {
	c := root
	for _, word := range path {
		var next *cobra.Command
		for _, child := range c.Commands() {
			if child.Name() == word || child.HasAlias(word) {
				next = child
				break
			}
		}
		if next == nil {
			return nil
		}
		c = next
	}
	return c
}

// TestParentCommandsAreNotHelpOnly walks the whole command tree (gt-fcxe9.6,
// deep review G4-04). A parent with subcommands and no Run/RunE is answered by
// cobra itself: it prints help and exits 0 on a missing or unknown
// subcommand, so `gt tap guard <typo>` in a PreToolUse hook allows every tool
// call and a misspelled formula step "succeeds".
func TestParentCommandsAreNotHelpOnly(t *testing.T) {
	t.Parallel()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			if child.HasSubCommands() && !child.Runnable() {
				t.Errorf("%s has subcommands but no Run/RunE; set RunE: requireSubcommand", child.CommandPath())
			}
			walk(child)
		}
	}
	walk(rootCmd)
}

// TestUnknownSubcommandExitsTwo checks every parent that uses
// requireSubcommand: a word that names none of its children names no command
// (TestPrefixMatchingDisabled pins that Find does not guess one), so cobra
// hands it to the parent, which fails with exit status 2.
func TestUnknownSubcommandExitsTwo(t *testing.T) {
	t.Parallel()
	const bogus = "zz-no-such-subcommand"
	var walk func(c *cobra.Command)
	checked := 0
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			if child.HasSubCommands() && isRequireSubcommand(child) {
				if lookupCommand(child, []string{bogus}) != nil {
					t.Errorf("%s has a subcommand named %s", child.CommandPath(), bogus)
				} else if code := exitCodeForError(child.RunE(child, []string{bogus})); code != 2 {
					t.Errorf("%s %s: exit %d, want 2", child.CommandPath(), bogus, code)
				}
				checked++
			}
			walk(child)
		}
	}
	walk(rootCmd)
	// A floor that proves the walk reached the command tree, not a count to
	// keep in step with it: command deletions shrink the tree.
	if checked < 30 {
		t.Fatalf("checked %d requireSubcommand parents; expected at least 30", checked)
	}
}

func TestRequireSubcommandExitsTwo(t *testing.T) {
	t.Parallel()
	parent := &cobra.Command{Use: "p", RunE: requireSubcommand}
	parent.AddCommand(&cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}})
	for _, args := range [][]string{nil, {"chidl"}} {
		err := requireSubcommand(parent, args)
		if err == nil {
			t.Fatalf("requireSubcommand(%v) = nil, want an error", args)
		}
		if code := exitCodeForError(err); code != 2 {
			t.Errorf("requireSubcommand(%v) exit = %d, want 2", args, code)
		}
	}
	if err := requireSubcommand(parent, []string{"chidl"}); !strings.Contains(err.Error(), "p child") {
		t.Errorf("unknown-subcommand error should suggest the near miss, got: %v", err)
	}
}

// TestTapGuardUnknownGuardBlocks is the security case behind G4-04: a
// renamed or misspelled guard must block (exit 2), not allow.
func TestTapGuardUnknownGuardBlocks(t *testing.T) {
	t.Parallel()
	if lookupCommand(rootCmd, []string{"tap", "guard", "dangerous-commands-typo"}) != nil {
		t.Fatal("dangerous-commands-typo names a guard")
	}
	c := lookupCommand(rootCmd, []string{"tap", "guard"})
	if c != tapGuardCmd {
		t.Fatal("gt tap guard does not resolve to tapGuardCmd")
	}
	rest := []string{"dangerous-commands-typo"}
	if c.RunE == nil {
		t.Fatal("gt tap guard has no RunE; cobra prints help and exits 0")
	}
	if code := exitCodeForError(c.RunE(c, rest)); code != 2 {
		t.Fatalf("gt tap guard <unknown> exit = %d, want 2 (PreToolUse block)", code)
	}
}

// TestPrefixMatchingDisabled: with prefix matching on, `gt statu` ran
// `gt status` and a truncated or typo'd word in a formula or hook could run a
// different command than the author named (deep review G4-11).
func TestPrefixMatchingDisabled(t *testing.T) {
	t.Parallel()
	if cobra.EnablePrefixMatching {
		t.Fatal("cobra.EnablePrefixMatching is on")
	}
	// "stat" is a declared alias of status, so it still resolves; these are
	// bare abbreviations. Find writes nothing on a word that matches no
	// command, so this is safe beside the parallel walkers.
	for _, args := range [][]string{{"statu"}, {"witn"}} {
		c, _, err := rootCmd.Find(args)
		if err == nil {
			t.Errorf("gt %s resolved to %q; an abbreviation must not resolve", strings.Join(args, " "), c.CommandPath())
		}
	}
}

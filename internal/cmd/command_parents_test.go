package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// These tests resolve commands with rootCmd.Find and call RunE directly, so
// persistentPreRun (usage logging, town discovery) never runs. They are not
// parallel: they read the shared rootCmd that other tests in this package
// execute.

// TestParentCommandsAreNotHelpOnly walks the whole command tree (gt-fcxe9.6,
// deep review G4-04). A parent with subcommands and no Run/RunE is answered by
// cobra itself: it prints help and exits 0 on a missing or unknown
// subcommand, so `gt tap guard <typo>` in a PreToolUse hook allows every tool
// call and a misspelled formula step "succeeds".
func TestParentCommandsAreNotHelpOnly(t *testing.T) {
	strictCompletionCmd(rootCmd) // what Execute does; cobra would add it lazily
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
// requireSubcommand: a word that names none of its children resolves to the
// parent itself (no prefix match) and fails with exit status 2.
func TestUnknownSubcommandExitsTwo(t *testing.T) {
	const bogus = "zz-no-such-subcommand"
	var walk func(c *cobra.Command)
	checked := 0
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			if child.HasSubCommands() && isRequireSubcommand(child) {
				path := append(strings.Fields(child.CommandPath())[1:], bogus)
				found, rest, err := rootCmd.Find(path)
				if err != nil {
					t.Errorf("Find(%v): %v", path, err)
				} else if found != child {
					t.Errorf("Find(%v) resolved to %q, want %q", path, found.CommandPath(), child.CommandPath())
				} else if code := exitCodeForError(found.RunE(found, rest)); code != 2 {
					t.Errorf("%s %s: exit %d, want 2", child.CommandPath(), bogus, code)
				}
				checked++
			}
			walk(child)
		}
	}
	walk(rootCmd)
	if checked < 40 {
		t.Fatalf("checked %d requireSubcommand parents; expected at least 40", checked)
	}
}

func TestRequireSubcommandExitsTwo(t *testing.T) {
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
	c, rest, err := rootCmd.Find([]string{"tap", "guard", "dangerous-commands-typo"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if c != tapGuardCmd {
		t.Fatalf("resolved to %q, want gt tap guard", c.CommandPath())
	}
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
	if cobra.EnablePrefixMatching {
		t.Fatal("cobra.EnablePrefixMatching is on")
	}
	// "stat" is a declared alias of status, so it still resolves; these are
	// bare abbreviations.
	for _, args := range [][]string{{"statu"}, {"refin"}, {"mq", "lis"}} {
		c, _, err := rootCmd.Find(args)
		if err == nil && (len(args) == 1 || c != mqCmd) {
			t.Errorf("gt %s resolved to %q; an abbreviation must not resolve", strings.Join(args, " "), c.CommandPath())
		}
	}
}

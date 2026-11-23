package cmd

import (
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

// commandGroupViolations reports each command under c that names a GroupID its
// parent never declares with AddGroup. It takes the tree so a fixture with a
// known violation drives the report, not only production (gt-e88y9c).
func commandGroupViolations(c *cobra.Command) []string {
	var out []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if p := c.Parent(); p != nil && c.GroupID != "" && !p.ContainsGroup(c.GroupID) {
			out = append(out, fmt.Sprintf("%s names group %q, which %s never declares with AddGroup",
				c.CommandPath(), c.GroupID, p.CommandPath()))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(c)
	return out
}

// TestEveryCommandGroupIDIsDeclaredByItsParent: cobra panics for every command
// when one names a GroupID its parent never declared with AddGroup, and the
// unit tier missed that because nothing walked the tree (gt-e88y9c).
func TestEveryCommandGroupIDIsDeclaredByItsParent(t *testing.T) {
	t.Parallel()
	for _, v := range commandGroupViolations(rootCmd) {
		t.Error(v)
	}
}

func TestEveryCommandGroupIDFixtureReportsUndeclaredGroup(t *testing.T) {
	t.Parallel()
	parent := &cobra.Command{Use: "parent"}
	parent.AddGroup(&cobra.Group{ID: "declared", Title: "Declared"})
	parent.AddCommand(&cobra.Command{Use: "declared-child", GroupID: "declared"})
	parent.AddCommand(&cobra.Command{Use: "child", GroupID: "undeclared"})

	got := commandGroupViolations(parent)
	want := `parent child names group "undeclared", which parent never declares with AddGroup`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("violations = %q, want exactly [%q]", got, want)
	}
}

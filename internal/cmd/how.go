package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/taskmap"
)

// how.go is `gt how`: the task groups an agent looks a job up by, one group's
// commands with their use lines (gt-j669b4.6), or the commands a word matches
// (gt-j669b4.24). The map is read through howMap, so the tests drive the
// command from a fixed map and a placement file landing later cannot change a
// golden.

// howMap is what a listing reads. howDefault supplies the real one.
type howMap interface {
	// Groups returns every group, in display order.
	Groups() []taskmap.Group
	// Describe returns g's one-line description, or "" for a group it does not
	// know.
	Describe(g taskmap.Group) string
	// ByGroup returns g's entries.
	ByGroup(g taskmap.Group) []taskmap.Entry
	// Search returns the entries whose path or use contains word, ignoring
	// case and sorted by path.
	Search(word string) []taskmap.Entry
}

// howDefault is the real map: the entries the placement files register, plus
// the package's fixed group list and descriptions.
type howDefault struct{ reg *taskmap.Registry }

func (howDefault) Groups() []taskmap.Group { return taskmap.Groups() }

func (howDefault) Describe(g taskmap.Group) string { return taskmap.Describe(g) }

func (h howDefault) ByGroup(g taskmap.Group) []taskmap.Entry { return h.reg.ByGroup(g) }

func (h howDefault) Search(word string) []taskmap.Entry { return h.reg.Search(word) }

var howCmd = &cobra.Command{
	Use:     "how [group|word]",
	GroupID: GroupDiag,
	Short:   "List the task groups, one group's commands, or the commands a word matches",
	Long: `List the task groups an agent can look a job up by, one line each saying
what the group is for. With a group, list that group's commands instead: one
line each, "gt <path>  <use>", followed by "check: <sign>" when the map names a
way to see the step worked.

A word that names no group searches the commands' paths and use lines for it,
ignoring case, and prints each match in the same "gt <path>  <use>" form. A
word that matches nothing prints "nothing matches <word>; run gt how for the
groups". Both exit 0.

Examples:
  gt how
  gt how roll
  gt how install`,
	Args:         cobra.MaximumNArgs(1),
	RunE:         runHowCmd,
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(howCmd)
}

func runHowCmd(cmd *cobra.Command, args []string) error {
	return runHow(cmd.OutOrStdout(), howDefault{taskmap.Default()}, args)
}

// runHow writes the listing for args to out: every group for no argument, one
// group's entries for a group, and the matching commands for any other word. It
// writes once, at the end, so a listing that cannot be built never reaches out.
func runHow(out io.Writer, m howMap, args []string) error {
	var b strings.Builder
	switch len(args) {
	case 0:
		if err := writeHowGroups(&b, m); err != nil {
			return err
		}
	case 1:
		g := taskmap.Group(args[0])
		if hasGroup(m.Groups(), g) {
			writeHowGroup(&b, m, g)
		} else {
			writeHowSearch(&b, m, args[0])
		}
	default:
		return fmt.Errorf("want at most one group or word, got %d arguments", len(args))
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// writeHowGroups writes every group with its description, aligned in the
// order Groups returns them. A group with no description is a broken table, so
// the listing refuses rather than print a blank line.
func writeHowGroups(b *strings.Builder, m howMap) error {
	groups := m.Groups()
	width := 0
	for _, g := range groups {
		if n := len(g); n > width {
			width = n
		}
	}
	for _, g := range groups {
		desc := m.Describe(g)
		if desc == "" {
			return fmt.Errorf("task map group %q has no description", g)
		}
		fmt.Fprintf(b, "%-*s  %s\n", width, g, desc)
	}
	return nil
}

// writeHowGroup writes g's entries, "gt <path>  <use>", with a "check:" line
// after the use line when the entry names one. Entries are sorted here so the
// listing's order does not depend on the map's.
func writeHowGroup(b *strings.Builder, m howMap, g taskmap.Group) {
	entries := append([]taskmap.Entry(nil), m.ByGroup(g)...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if len(entries) == 0 {
		fmt.Fprintf(b, "no commands in group %s yet\n", g)
		return
	}
	for _, e := range entries {
		fmt.Fprintf(b, "gt %s  %s\n", e.Path, e.Use)
		if e.Check != "" {
			fmt.Fprintf(b, "check: %s\n", e.Check)
		}
	}
}

// writeHowSearch writes word's matches, "gt <path>  <use>", in the order
// Search returns them. With no match it writes the one line that sends the
// agent back to the group list, so a guess at a word ends in a next step rather
// than an empty screen.
func writeHowSearch(b *strings.Builder, m howMap, word string) {
	entries := m.Search(word)
	if len(entries) == 0 {
		fmt.Fprintf(b, "nothing matches %s; run gt how for the groups\n", word)
		return
	}
	for _, e := range entries {
		fmt.Fprintf(b, "gt %s  %s\n", e.Path, e.Use)
	}
}

// hasGroup reports whether groups holds g.
func hasGroup(groups []taskmap.Group, g taskmap.Group) bool {
	for _, known := range groups {
		if known == g {
			return true
		}
	}
	return false
}

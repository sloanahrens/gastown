// Package cmdtree checks the gt and bd commands that formulas, templates,
// plugins, hook scripts and Go exec literals invoke against the real command
// trees (gt-fcxe9.5, deep review G4-03/B1-08/B5-11). Those callers are prose
// or untyped argv: nothing else notices when a command they name is renamed
// or removed, and an agent handed "unknown command" improvises.
//
// gt's tree comes from its cobra root in-process; bd's from a checked-in
// snapshot of `bd capabilities --json` (see bdtree.go). The gate that runs
// the check over the repository lives in internal/cmd, the only package that
// can see gt's root command.
package cmdtree

import (
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// Node is one command in a Tree. Children is keyed by name and by alias.
type Node struct {
	Name     string
	Children map[string]*Node
	// TakesArgs reports whether the command accepts positional arguments.
	// When false and the command has children, a plain word after it can
	// only be a subcommand, so an unrecognised one is a violation.
	TakesArgs bool
}

// Tree is a command tree rooted at the binary.
type Tree struct {
	root *Node
}

// NewTree returns an empty tree.
func NewTree() *Tree {
	return &Tree{root: &Node{Children: map[string]*Node{}}}
}

// Add registers the command at path. aliases name the last path element.
// Missing ancestors are created argument-free; a later Add for the ancestor
// itself sets its TakesArgs.
func (t *Tree) Add(path, aliases []string, takesArgs bool) {
	n := t.root
	for i, name := range path {
		child, ok := n.Children[name]
		if !ok {
			child = &Node{Name: name, Children: map[string]*Node{}}
			n.Children[name] = child
		}
		if i == len(path)-1 {
			child.TakesArgs = takesArgs
			for _, a := range aliases {
				n.Children[a] = child
			}
		}
		n = child
	}
}

// FromCobra builds a Tree from a cobra root, hidden commands included (they
// run just the same). takesArgs decides each command's TakesArgs.
func FromCobra(root *cobra.Command, takesArgs func(*cobra.Command) bool) *Tree {
	t := NewTree()
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, child := range c.Commands() {
			p := append(append([]string(nil), path...), child.Name())
			t.Add(p, child.Aliases, takesArgs(child))
			walk(child, p)
		}
	}
	walk(root, nil)
	return t
}

// Resolution is the outcome of resolving the words after a binary name.
type Resolution struct {
	Matched []string // the words that named commands, as written
	Unknown string   // the word that failed to resolve, when !OK
	OK      bool
}

// MatchedPath is Matched joined with spaces.
func (r Resolution) MatchedPath() string { return strings.Join(r.Matched, " ") }

// plainWord is a token that can only be a command name: no digits, slashes,
// dots or placeholders, so bead ids, rig/agent addresses and paths never
// read as an unknown subcommand.
var plainWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// Resolve walks words down the tree. The first word must name a top-level
// command. Later words descend while they name children; at the first one
// that does not, the invocation fails only if the command reached has
// children, takes no positional arguments, and the word is plain.
func (t *Tree) Resolve(words []string) Resolution {
	var r Resolution
	n := t.root
	for i, w := range words {
		child, ok := n.Children[w]
		if ok {
			r.Matched = append(r.Matched, w)
			n = child
			continue
		}
		if i == 0 || (len(n.Children) > 0 && !n.TakesArgs && plainWord.MatchString(w)) {
			r.Unknown = w
			return r
		}
		break
	}
	r.OK = true
	return r
}

// Package taskmap gives every gt command a task group and a one-line use, so
// an agent looking for a tool finds it by the job to be done rather than by
// rediscovering the command tree. Entries are registered from the per-batch
// placement files (internal/taskmap/place_pN.go); TestEveryCommandHasATask in
// internal/cmd fails when a command has neither an entry nor a line in
// testdata/unplaced.
package taskmap

import (
	"fmt"
	"sort"
	"strings"
)

// Group is one task group. The set is fixed; see Groups.
type Group string

// Entry is one command's place in the map.
type Entry struct {
	Path  string // command path without "gt", e.g. "update" or "pool set"
	Group Group
	Use   string // one line, starts "Use this when" or "Use this to"
	Check string // optional: the command or sign that shows the step worked
}

// groups is every task group, in display order.
var groups = []Group{
	"orient",
	"dispatch",
	"land",
	"review",
	"roll",
	"seats",
	"sandboxes",
	"birds",
	"host",
	"dolt",
	"cost",
	"shutdown",
	"internal",
}

var descriptions = map[Group]string{
	"orient":    "Use this to find where you are and what to do next.",
	"dispatch":  "Use this to give work to an agent, a crew or a polecat.",
	"land":      "Use this to get finished work onto main.",
	"review":    "Use this to check work before or after it lands.",
	"roll":      "Use this to undo, replay or promote a landing.",
	"seats":     "Use this to inspect or move agent seats and sessions.",
	"sandboxes": "Use this to run work in an isolated environment.",
	"birds":     "Use this to run the town's autonomous bird agents.",
	"host":      "Use this to inspect or change the machine the town runs on.",
	"dolt":      "Use this to inspect or repair the beads database.",
	"cost":      "Use this to see or limit what the town spends.",
	"shutdown":  "Use this to stop agents, sessions or the town.",
	"internal":  "Use this for gt's own plumbing when no task group fits.",
}

// defaultRegistry holds the entries the package-level functions expose. Each
// placement file registers into it from its init.
var defaultRegistry = newRegistry()

// Groups returns every task group, in display order.
func Groups() []Group {
	return append([]Group(nil), groups...)
}

// Describe returns the one-line description of g, or "" for a group not in
// Groups.
func Describe(g Group) string {
	return descriptions[g]
}

// registry holds the entries one map knows about.
type registry struct {
	entries map[string]Entry
}

func newRegistry() *registry {
	return &registry{entries: map[string]Entry{}}
}

// Register adds entries to the default map. Each placement file calls it from
// its init.
func Register(es ...Entry) {
	defaultRegistry.register(es...)
}

// Lookup returns the entry for path.
func Lookup(path string) (Entry, bool) {
	return defaultRegistry.lookup(path)
}

// ByGroup returns g's entries, sorted by Path.
func ByGroup(g Group) []Entry {
	return defaultRegistry.byGroup(g)
}

// Search returns entries whose Path or Use contains word, ignoring case and
// sorted by Path.
func Search(word string) []Entry {
	return defaultRegistry.search(word)
}

// Validate returns the problems that stop the map covering every command:
// a path with no entry and no unplaced line, an unplaced line that has an
// entry or names no command, and a registered entry that has an unknown
// group, an empty or multi-line Use, or names no command. The result is empty
// when the map is clean.
func Validate(paths []string, unplaced []string) []string {
	return defaultRegistry.validate(paths, unplaced)
}

func (r *registry) register(es ...Entry) {
	for _, e := range es {
		r.entries[e.Path] = e
	}
}

func (r *registry) lookup(path string) (Entry, bool) {
	e, ok := r.entries[path]
	return e, ok
}

func (r *registry) byGroup(g Group) []Entry {
	var out []Entry
	for _, e := range r.entries {
		if e.Group == g {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (r *registry) search(word string) []Entry {
	needle := strings.ToLower(word)
	var out []Entry
	for _, e := range r.entries {
		if strings.Contains(strings.ToLower(e.Path), needle) || strings.Contains(strings.ToLower(e.Use), needle) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (r *registry) validate(paths []string, unplaced []string) []string {
	commands := make(map[string]bool, len(paths))
	for _, p := range paths {
		commands[p] = true
	}
	excused := make(map[string]bool, len(unplaced))
	var problems []string
	for _, p := range unplaced {
		if excused[p] {
			continue
		}
		excused[p] = true
		if _, ok := r.entries[p]; ok {
			problems = append(problems, fmt.Sprintf("%q: line in testdata/unplaced but the command has an entry (remove the line)", p))
			continue
		}
		if !commands[p] {
			problems = append(problems, fmt.Sprintf("%q: line in testdata/unplaced but no such command (remove the line)", p))
		}
	}
	for _, p := range paths {
		if _, ok := r.entries[p]; ok {
			continue
		}
		if excused[p] {
			continue
		}
		problems = append(problems, fmt.Sprintf("%q: no task map entry and no line in testdata/unplaced", p))
	}
	for path, e := range r.entries {
		if !commands[path] {
			problems = append(problems, fmt.Sprintf("%q: task map entry names no command", path))
		}
		if !isGroup(e.Group) {
			problems = append(problems, fmt.Sprintf("%q: task map entry has unknown group %q", path, e.Group))
		}
		if strings.TrimSpace(e.Use) == "" {
			problems = append(problems, fmt.Sprintf("%q: task map entry has an empty Use", path))
		} else if strings.Contains(e.Use, "\n") {
			problems = append(problems, fmt.Sprintf("%q: task map entry Use spans more than one line", path))
		}
	}
	sort.Strings(problems)
	return problems
}

func isGroup(g Group) bool {
	for _, known := range groups {
		if g == known {
			return true
		}
	}
	return false
}

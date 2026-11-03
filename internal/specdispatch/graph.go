package specdispatch

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// interfacesSection is the "## " heading, lower-cased as Sections keys it,
// whose lines say which names a bead defines and uses (design decision 3,
// docs/plans/2026-11-03-bead-lint-v2-design.md).
const interfacesSection = "interfaces"

// InterfacesBlock is a bead's parsed "## Interfaces" section: the identifiers
// it defines and the identifiers it uses, each in the order first written and
// without repeats. The zero value is both lists empty.
type InterfacesBlock struct {
	Defines []string
	Uses    []string
}

// interfacesIdentifierRe matches one identifier: letters, digits, underscores
// and dots, starting with a letter or underscore. A dot joins segments, so
// Type.Method is one identifier while a leading, trailing or doubled dot is
// not.
var interfacesIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// ParseInterfaces reads a bead description's optional "## Interfaces"
// section: "Defines:" and "Uses:" lines, each a comma-separated list of
// identifiers. A line may carry a leading "- " bullet, and each item may sit
// in backticks; both are stripped. Blank lines are ignored. present is false
// when there is no Interfaces section, which is not an error. A line that is
// not blank and is neither a Defines nor a Uses line, and an item that is not
// an identifier, are errors naming the 1-based line within the section — and
// the item, when it is the item that is refused. Repeated lines of one kind
// merge in order without duplicates, so a bead may add to a list on a later
// line.
func ParseInterfaces(description string) (InterfacesBlock, bool, error) {
	body, present := Sections(description)[interfacesSection]
	if !present {
		return InterfacesBlock{}, false, nil
	}
	var block InterfacesBlock
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		text := strings.TrimPrefix(line, "- ")
		key, list, ok := strings.Cut(text, ":")
		var target *[]string
		switch strings.TrimSpace(key) {
		case "Defines":
			target = &block.Defines
		case "Uses":
			target = &block.Uses
		}
		if !ok || target == nil {
			return InterfacesBlock{}, true, fmt.Errorf("interfaces: line %d: %q is not a Defines: or Uses: line", i+1, line)
		}
		for _, item := range strings.Split(list, ",") {
			name := unquotePath(strings.TrimSpace(item))
			if !interfacesIdentifierRe.MatchString(name) {
				return InterfacesBlock{}, true, fmt.Errorf("interfaces: line %d: %q is not an identifier (letters, digits, underscore and dot, starting with a letter or underscore)", i+1, name)
			}
			if !slices.Contains(*target, name) {
				*target = append(*target, name)
			}
		}
	}
	return block, true, nil
}

// GraphBead is what CheckGraph needs to know about one bead of an epic: its
// id, the ids it depends on directly, the names it declares under
// "## Interfaces", the paths it creates and modifies under "## Files", and the
// leading identifier of each acceptance test it lists. The "gt spec lint
// --graph" command builds these from the epic's beads (bead gt-hgt9j.16).
type GraphBead struct {
	ID             string
	DependsOn      []string // ids of the beads it depends on, direct edges only
	Defines, Uses  []string // from ## Interfaces
	Modify, Create []string // from ## Files
	TestNames      []string // the leading TestX identifier of each acceptance box
}

// The kinds of finding CheckGraph reports. The first three refuse the epic;
// KindDuplicateTest is only an advisory (design decision 3,
// docs/plans/2026-11-03-bead-lint-v2-design.md), so the Kind itself is what
// tells a caller not to fail the epic over it.
const (
	KindDuplicateDefine = "duplicate-define"
	KindUnmetUse        = "unmet-use"
	KindSharedModify    = "shared-modify"
	KindDuplicateTest   = "duplicate-test"
)

// graphKindRank is the order findings are sorted in: the design lists the
// kinds with the three refusals first and the duplicate-test advisory last.
var graphKindRank = map[string]int{
	KindDuplicateDefine: 0,
	KindUnmetUse:        1,
	KindSharedModify:    2,
	KindDuplicateTest:   3,
}

// GraphFinding is one mistake that only shows across the beads of an epic.
// Kind is one of the Kind constants. Bead is the bead the finding is filed
// against, and for a finding about a pair of beads it is the one written
// first. Detail names the beads and the name or path at fault.
type GraphFinding struct {
	Kind, Bead, Detail string
}

// CheckGraph reports the mistakes that only show across the beads of an epic:
// a name two beads define, a name a bead uses that no bead it depends on,
// directly or transitively, defines, two beads that both modify a file
// although neither depends on the other, directly or transitively, and a test
// name two beads list. A bead's own Defines do not satisfy its Uses; only the
// beads it depends on do. A dependency id that is not among beads is ignored,
// and a dependency cycle terminates because each bead is visited once.
// Findings are sorted by kind (the order of graphKindRank), then bead, then
// detail, so equal input gives equal output.
func CheckGraph(beads []GraphBead) []GraphFinding {
	byID := make(map[string]GraphBead, len(beads))
	for _, b := range beads {
		byID[b.ID] = b
	}
	// reach is the set of ids each bead depends on, transitively.
	reach := make(map[string]map[string]bool, len(beads))
	for _, b := range beads {
		deps := map[string]bool{}
		graphVisitDependencies(b.DependsOn, byID, func(dep GraphBead) { deps[dep.ID] = true })
		reach[b.ID] = deps
	}

	var findings []GraphFinding
	for i := range beads {
		for j := i + 1; j < len(beads); j++ {
			a, b := beads[i], beads[j]
			for _, name := range uniqueNames(a.Defines) {
				if slices.Contains(b.Defines, name) {
					findings = append(findings, GraphFinding{
						Kind:   KindDuplicateDefine,
						Bead:   a.ID,
						Detail: fmt.Sprintf("%s is defined by both %s and %s", name, a.ID, b.ID),
					})
				}
			}
			for _, name := range uniqueNames(a.TestNames) {
				if slices.Contains(b.TestNames, name) {
					findings = append(findings, GraphFinding{
						Kind:   KindDuplicateTest,
						Bead:   a.ID,
						Detail: fmt.Sprintf("%s is listed by both %s and %s", name, a.ID, b.ID),
					})
				}
			}
			if reach[a.ID][b.ID] || reach[b.ID][a.ID] {
				continue
			}
			for _, path := range uniqueNames(a.Modify) {
				if slices.Contains(b.Modify, path) {
					findings = append(findings, GraphFinding{
						Kind:   KindSharedModify,
						Bead:   a.ID,
						Detail: fmt.Sprintf("%s and %s both modify %s", a.ID, b.ID, path),
					})
				}
			}
		}
	}
	for _, b := range beads {
		defined := map[string]bool{}
		for id := range reach[b.ID] {
			for _, name := range byID[id].Defines {
				defined[name] = true
			}
		}
		for _, name := range uniqueNames(b.Uses) {
			if !defined[name] {
				findings = append(findings, GraphFinding{
					Kind:   KindUnmetUse,
					Bead:   b.ID,
					Detail: fmt.Sprintf("%s uses %s, which no bead it depends on defines", b.ID, name),
				})
			}
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if a, b := graphKindRank[findings[i].Kind], graphKindRank[findings[j].Kind]; a != b {
			return a < b
		}
		if findings[i].Bead != findings[j].Bead {
			return findings[i].Bead < findings[j].Bead
		}
		return findings[i].Detail < findings[j].Detail
	})
	return findings
}

// graphVisitDependencies calls visit for each bead reachable from ids by
// following DependsOn, once per bead, so a cycle terminates. An id that is not
// in byID is ignored.
func graphVisitDependencies(ids []string, byID map[string]GraphBead, visit func(GraphBead)) {
	visited := make(map[string]bool, len(byID))
	stack := append([]string(nil), ids...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[id] {
			continue
		}
		visited[id] = true
		dep, ok := byID[id]
		if !ok {
			continue
		}
		visit(dep)
		stack = append(stack, dep.DependsOn...)
	}
}

// uniqueNames returns names without repeats, in first-written order.
func uniqueNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

package specdispatch

import (
	"fmt"
	"regexp"
	"slices"
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

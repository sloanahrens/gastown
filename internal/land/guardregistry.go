package land

import (
	"fmt"
	"sort"
)

// guardRegistryFailures returns one sorted line for each test in found that
// neither registry nor exempt covers, and one for each exempt key the registry
// or the scan has outgrown. found names the tests ScanningTests found, registry
// is treeWideGuards, and exempt maps "<pkg> <TestName>" to the reason it walks
// only a fixture.
func guardRegistryFailures(found []ScanningTest, registry []treeWideGuard, exempt map[string]string) []string {
	registered := make(map[string]bool)
	for _, g := range registry {
		for _, name := range g.tests {
			registered[g.pkg+" "+name] = true
		}
	}
	scanning := make(map[string]bool, len(found))
	var lines []string
	for _, st := range found {
		key := st.Pkg + " " + st.Name
		scanning[key] = true
		if _, ok := exempt[key]; ok || registered[key] {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s scans the source tree but is not in treeWideGuards (internal/land/changed.go): add it to the entry for %s with the paths it reads as judges, or, if it only walks a fixture, list it in the exemption file with a reason", key, st.Pkg))
	}
	for key := range exempt {
		switch {
		case registered[key]:
			lines = append(lines, "exemption "+key+" is registered now: delete its line")
		case !scanning[key]:
			lines = append(lines, "exemption "+key+" names a test that no longer scans the tree: delete its line")
		}
	}
	sort.Strings(lines)
	return lines
}

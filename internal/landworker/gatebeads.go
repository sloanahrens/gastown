package landworker

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// Labels on the beads Land's flake policy files (gt-v4ssj.5).
const (
	LabelFlake      = "flake"
	LabelTestBudget = "test-budget"
)

// GateBeads files the beads Land's flake policy finds on a rig: one per
// flaky test -- or one per package when MinPackageFlakeTests or more of its
// tests failed together -- and one per package over the test budget. A
// second sighting of the same test (or package) comments on the bead already
// open for it.
type GateBeads struct {
	Rig   string
	Beads RedMainBeads
}

var _ land.GateBeads = GateBeads{}

// FlakeTitle is the title of the flake bead for test in pkg on rig; it is the
// key an open bead is found by.
func FlakeTitle(rig, pkg, test string) string {
	return flakeTitlePrefix(rig) + pkg + " " + test
}

// PackageFlakeTitle is the title of the package-wide flake bead for pkg on
// rig: the same prefix, extended with the package rather than a test name, so
// it is a different key from any per-test bead for the same package.
func PackageFlakeTitle(rig, pkg string) string {
	return flakeTitlePrefix(rig) + pkg
}

func flakeTitlePrefix(rig string) string { return fmt.Sprintf("flaky test (%s): ", rig) }

// BudgetTitle is the title of the test-budget bead for pkg on rig.
func BudgetTitle(rig, pkg string) string {
	return fmt.Sprintf("test budget overrun (%s): %s", rig, pkg)
}

// FileGateBead files b, or comments on the open bead for the same key.
func (g GateBeads) FileGateBead(b land.GateBead) (string, error) {
	var label, prefix, key, title, desc string
	priority := 2
	switch b.Kind {
	case land.GateBeadFlake:
		label, prefix, key = LabelFlake, flakeTitlePrefix(g.Rig), b.Package+" "+b.Test
		title = FlakeTitle(g.Rig, b.Package, b.Test)
		desc = "Land's flake policy (gt-v4ssj.5) found this test failing on a merged tree and passing on a rerun of its package. " + b.Detail +
			"\n\nMake the test deterministic. Each further sighting is a comment here."
	case land.GateBeadFlakePackage:
		label, prefix, key = LabelFlake, flakeTitlePrefix(g.Rig), b.Package
		title = PackageFlakeTitle(g.Rig, b.Package)
		desc = fmt.Sprintf("Land's flake policy (gt-v4ssj.5) found %d tests of this package failing together on a merged tree and passing together on a rerun of the package: one sighting, not %d, because a setup stall (a container or fixture that took long to start) fails everything behind it.\n\nFailed: %s.\n\n%s"+
			"\n\nMake the package's setup deterministic. Each further sighting is a comment here.",
			len(b.Tests), len(b.Tests), strings.Join(b.Tests, ", "), b.Detail)
	case land.GateBeadBudget:
		label, prefix, key = LabelTestBudget, BudgetTitle(g.Rig, ""), b.Package
		title = BudgetTitle(g.Rig, b.Package)
		priority = 1
		desc = "Land's flake policy (gt-v4ssj.5): a package over the test budget blocks every landing on the rig until it passes. " + b.Detail +
			"\n\nMake the package's tests cheaper, or track the overrun in internal/testpolicy/overbudget.txt with this bead."
	default:
		return "", fmt.Errorf("unknown gate bead kind %q", b.Kind)
	}
	// An unreadable list files anyway: a duplicate beats a silent flake.
	open, _ := openBeadsByTitle(g.Beads, label, prefix)
	return fileOrComment(g.Beads, open[key], "seen again: "+b.Detail, beads.CreateOptions{
		Title:       title,
		Labels:      []string{label},
		Priority:    priority,
		Description: desc,
	})
}

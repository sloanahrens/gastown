package landworker

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// Labels on the beads Land's flake policy files (gt-v4ssj.5).
const (
	LabelFlake      = "flake"
	LabelTestBudget = "test-budget"
)

// GateBeads files the beads Land's flake policy finds on a rig: one per
// flaky test, one per package over the test budget. A second sighting of
// the same test (or package) comments on the bead already open for it.
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

func flakeTitlePrefix(rig string) string { return fmt.Sprintf("flaky test (%s): ", rig) }

// BudgetTitle is the title of the test-budget bead for pkg on rig.
func BudgetTitle(rig, pkg string) string {
	return fmt.Sprintf("test budget overrun (%s): %s", rig, pkg)
}

// FileGateBead files b, or comments on the open bead for the same key.
func (g GateBeads) FileGateBead(b land.GateBead) (string, error) {
	var label, prefix, key, desc string
	priority := 2
	switch b.Kind {
	case land.GateBeadFlake:
		label, prefix, key = LabelFlake, flakeTitlePrefix(g.Rig), b.Package+" "+b.Test
		desc = "Land's flake policy (gt-v4ssj.5) found this test failing on a merged tree and passing on a rerun of its package. " + b.Detail +
			"\n\nMake the test deterministic. Each further sighting is a comment here."
	case land.GateBeadBudget:
		label, prefix, key = LabelTestBudget, BudgetTitle(g.Rig, ""), b.Package
		priority = 1
		desc = "Land's flake policy (gt-v4ssj.5): a package over the test budget blocks every landing on the rig until it passes. " + b.Detail +
			"\n\nMake the package's tests cheaper, or track the overrun in internal/testpolicy/overbudget.txt with this bead."
	default:
		return "", fmt.Errorf("unknown gate bead kind %q", b.Kind)
	}
	// An unreadable list files anyway: a duplicate beats a silent flake.
	open, _ := openBeadsByTitle(g.Beads, label, prefix)
	return fileOrComment(g.Beads, open[key], "seen again: "+b.Detail, beads.CreateOptions{
		Title:       prefix + key,
		Labels:      []string{label},
		Priority:    priority,
		Description: desc,
	})
}

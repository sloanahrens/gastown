package landworker

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

func TestGateBeadsFilesOncePerTestThenComments(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	g := GateBeads{Rig: "gastown", Beads: bd}
	flake := land.GateBead{Kind: land.GateBeadFlake, Package: pkgA, Test: "TestX", Detail: "first"}
	id1, err := g.FileGateBead(flake)
	if err != nil {
		t.Fatal(err)
	}
	flake.Detail = "second"
	id2, err := g.FileGateBead(flake)
	if err != nil || id2 != id1 {
		t.Fatalf("second sighting = %s, %v; want a comment on %s", id2, err, id1)
	}
	other, err := g.FileGateBead(land.GateBead{Kind: land.GateBeadFlake, Package: pkgA, Test: "TestY"})
	if err != nil || other == id1 {
		t.Fatalf("another test = %s, %v; want its own bead", other, err)
	}
	budget, err := g.FileGateBead(land.GateBead{Kind: land.GateBeadBudget, Package: "internal/a"})
	if err != nil {
		t.Fatal(err)
	}

	is, err := bd.Show(id1)
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != FlakeTitle("gastown", pkgA, "TestX") || !beads.HasLabel(is, LabelFlake) {
		t.Errorf("flake bead = %q labels %v", is.Title, is.Labels)
	}
	comments, err := bd.Comments(id1)
	if err != nil || len(comments) != 1 || comments[0].Text != "seen again: second" {
		t.Errorf("comments = %+v, %v", comments, err)
	}
	b, err := bd.Show(budget)
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != BudgetTitle("gastown", "internal/a") || !beads.HasLabel(b, LabelTestBudget) || b.Priority != 1 {
		t.Errorf("budget bead = %q labels %v priority %d", b.Title, b.Labels, b.Priority)
	}
}

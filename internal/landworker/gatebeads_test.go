package landworker

import (
	"strings"
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

func TestGateBeadsFilesOnePackageBeadThenComments(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	g := GateBeads{Rig: "gastown", Beads: bd}
	tests := []string{"TestA", "TestB", "TestC", "TestD", "TestE"}
	pkg := land.GateBead{Kind: land.GateBeadFlakePackage, Package: pkgA, Test: "TestA", Tests: tests, Detail: "first"}
	id1, err := g.FileGateBead(pkg)
	if err != nil {
		t.Fatal(err)
	}
	is, err := bd.Show(id1)
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != PackageFlakeTitle("gastown", pkgA) || !beads.HasLabel(is, LabelFlake) {
		t.Errorf("package bead = %q labels %v, want %q", is.Title, is.Labels, PackageFlakeTitle("gastown", pkgA))
	}
	for _, name := range tests {
		if !strings.Contains(is.Description, name) {
			t.Errorf("description does not list %s:\n%s", name, is.Description)
		}
	}
	pkg.Detail = "second"
	id2, err := g.FileGateBead(pkg)
	if err != nil || id2 != id1 {
		t.Fatalf("second sighting = %s, %v; want a comment on %s", id2, err, id1)
	}
	if comments, err := bd.Comments(id1); err != nil || len(comments) != 1 || comments[0].Text != "seen again: second" {
		t.Errorf("comments = %+v, %v", comments, err)
	}
	// One test of the same package is a different key: its own bead, not a
	// comment on the package bead.
	single, err := g.FileGateBead(land.GateBead{Kind: land.GateBeadFlake, Package: pkgA, Test: "TestA"})
	if err != nil || single == id1 {
		t.Fatalf("single test = %s, %v; want its own bead", single, err)
	}
}

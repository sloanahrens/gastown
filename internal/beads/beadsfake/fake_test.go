package beadsfake

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestFakeClientContract(t *testing.T) {
	t.Parallel()
	RunClientContract(t, func(t *testing.T) beads.Client { return New() })
	RunShowTextContract(t, func(t *testing.T) beads.Client { return New() })
	RunDepListUpContract(t, func(t *testing.T) beads.Client { return New() })
	RunCreatorOptionContract(t, func(t *testing.T) beads.Client { return New() })
	RunNotesFilterContract(t, func(t *testing.T) beads.Client { return New() })
}

// TestFakeClientContractSharedDatabase runs the contract the way the
// integration tier does, every case on one database, so a case that
// leaks into another's assertions fails here in milliseconds.
func TestFakeClientContractSharedDatabase(t *testing.T) {
	t.Parallel()
	shared := New()
	RunClientContract(t, func(t *testing.T) beads.Client { return shared })
	RunShowTextContract(t, func(t *testing.T) beads.Client { return shared })
	RunDepListUpContract(t, func(t *testing.T) beads.Client { return shared })
	RunCreatorOptionContract(t, func(t *testing.T) beads.Client { return shared })
	RunNotesFilterContract(t, func(t *testing.T) beads.Client { return shared })
}

// TestReadyFiltered runs the ReadyFiltered contract case on its own, so the
// case is runnable alone: go test ./internal/beads/beadsfake -run ReadyFiltered.
func TestReadyFiltered(t *testing.T) {
	t.Parallel()
	contractReadyFiltered(t, newScope(t, New()))
}

// TestShowText runs the ShowText contract case on its own, so the case is
// runnable alone: go test ./internal/beads/beadsfake -run ShowText.
func TestShowText(t *testing.T) {
	t.Parallel()
	contractShowText(t, newScope(t, New()))
}

// TestFormulaContract runs the formula seam contract on the fake, so the case
// is runnable alone: go test ./internal/beads/beadsfake -run Formula.
func TestFormulaContract(t *testing.T) {
	t.Parallel()
	RunFormulaContract(t, func(t *testing.T) beads.FormulaVerbs {
		f := New()
		SeedSampleFormula(f)
		return f
	})
}

// TestFormulaListSeeded pins what the fake adds beyond the contract: a
// registered formula appears in both list forms and in both show forms.
func TestFormulaListSeeded(t *testing.T) {
	t.Parallel()
	f := New()
	f.SeedFormula("mol-widget", "workflow", "widgets, made")

	if out, err := f.FormulaList(); err != nil || !strings.Contains(string(out), `"mol-widget"`) {
		t.Errorf("FormulaList = %q, %v; want the seeded name", out, err)
	}
	if out, err := f.FormulaListText(); err != nil || !strings.Contains(string(out), "mol-widget") {
		t.Errorf("FormulaListText = %q, %v; want the seeded name", out, err)
	}
	if out, err := f.FormulaShow("mol-widget"); err != nil || !strings.Contains(string(out), "mol-widget") {
		t.Errorf("FormulaShow = %q, %v; want the seeded name", out, err)
	}
	if out, err := f.FormulaShowText("mol-widget"); err != nil || !strings.Contains(string(out), "mol-widget") {
		t.Errorf("FormulaShowText = %q, %v; want the seeded name", out, err)
	}
}

// TestGateCheck runs the GateCheck contract case on its own, so the case is
// runnable alone: go test ./internal/beads/beadsfake -run GateCheck.
func TestGateCheck(t *testing.T) {
	t.Parallel()
	contractGateCheck(t, newScope(t, New()))
}

func TestFakeActorContract(t *testing.T) {
	t.Parallel()
	RunActorContract(t, func(t *testing.T, actor string) beads.Client { return New(WithActor(actor)) })
}

func TestFakeAdminContract(t *testing.T) {
	t.Parallel()
	RunAdminContract(t, func(t *testing.T) AdminClient { return New() })
}

// TestFakeAdminVersionContract runs the version suite against the fake, so it
// is runnable alone: go test ./internal/beads/beadsfake -run Admin.
func TestFakeAdminVersionContract(t *testing.T) {
	t.Parallel()
	RunVersionContract(t, func(t *testing.T) AdminClient { return New() })
}

// TestVersionReadsReportScriptedFailures: both version reads honor FailWith, so
// a handshake test can script a store that did not answer.
func TestVersionReadsReportScriptedFailures(t *testing.T) {
	t.Parallel()
	f := New()
	boom := errors.New("boom")
	f.FailWith("version", boom)
	if _, err := f.Version(); !errors.Is(err, boom) {
		t.Errorf("Version with FailWith(version) = %v, want boom", err)
	}
	f.FailWith("schema level", boom)
	if _, err := f.SchemaLevel(); !errors.Is(err, boom) {
		t.Errorf("SchemaLevel with FailWith(schema level) = %v, want boom", err)
	}
}

func TestSeedStoresTheGivenID(t *testing.T) {
	t.Parallel()
	f := New()
	f.Seed(beads.Issue{ID: "tr-rig-testrig", Title: "testrig", Labels: []string{"gt:rig"}})
	got, err := f.Show("tr-rig-testrig")
	if err != nil || got.Title != "testrig" || got.Status != "open" || len(got.Labels) != 1 {
		t.Fatalf("Show(seeded) = %+v, %v", got, err)
	}
}

// TestWispGCCandidatesCascadeAndHooks covers what the contract cannot build
// through Client: a hook_bead reference, and a child wisp younger than the
// threshold that bd's cascade still collects.
func TestWispGCCandidatesCascadeAndHooks(t *testing.T) {
	t.Parallel()
	f := New()
	old, _ := f.Create(beads.CreateOptions{Title: "old", Priority: -1, Ephemeral: true})
	hooked, _ := f.Create(beads.CreateOptions{Title: "hooked", Priority: -1, Ephemeral: true})
	f.Seed(beads.Issue{ID: "gt-agent", Title: "agent", HookBead: hooked.ID})
	young, _ := f.Create(beads.CreateOptions{Title: "young step", Parent: old.ID, Priority: -1, Ephemeral: true})
	// The clock moves one second a write: old is 2s idle, young 0s.
	all, err := f.WispGCCandidates(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, id := range all {
		has[id] = true
	}
	if !has[old.ID] || has[hooked.ID] {
		t.Errorf("candidates = %v, want %s and never the hooked %s", all, old.ID, hooked.ID)
	}
	if !has[young.ID] {
		t.Errorf("candidates = %v, want the dependent %s by cascade", all, young.ID)
	}
}

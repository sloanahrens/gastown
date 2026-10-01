package beadsfake

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestFakeClientContract(t *testing.T) {
	t.Parallel()
	RunClientContract(t, func(t *testing.T) beads.Client { return New() })
}

// TestFakeClientContractSharedDatabase runs the contract the way the
// integration tier does, every case on one database, so a case that
// leaks into another's assertions fails here in milliseconds.
func TestFakeClientContractSharedDatabase(t *testing.T) {
	t.Parallel()
	shared := New()
	RunClientContract(t, func(t *testing.T) beads.Client { return shared })
}

func TestFakeActorContract(t *testing.T) {
	t.Parallel()
	RunActorContract(t, func(t *testing.T, actor string) beads.Client { return New(WithActor(actor)) })
}

func TestFakeAdminContract(t *testing.T) {
	t.Parallel()
	RunAdminContract(t, func(t *testing.T) AdminClient { return New() })
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

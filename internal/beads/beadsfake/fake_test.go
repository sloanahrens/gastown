package beadsfake

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestFakeClientContract(t *testing.T) {
	t.Parallel()
	RunClientContract(t, func(t *testing.T) beads.Client { return New() })
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

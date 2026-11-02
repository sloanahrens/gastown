package done

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// failingUpdateClient is a bead store whose label write fails: reads still
// reach the bead, the update never does.
type failingUpdateClient struct {
	beads.Client
	err error
}

func (c failingUpdateClient) Update(string, beads.UpdateOptions) error { return c.err }

// TestReadyLabelWriteTouchesStamp: a submission touches the rig's ready stamp
// once the label write is verified, so an idle worker lists the board on its
// next pass rather than waiting for its ceiling (gt-flo7a). A label write that
// failed leaves no stamp: there is nothing on the board to list.
func TestReadyLabelWriteTouchesStamp(t *testing.T) {
	t.Parallel()

	t.Run("a verified label write stamps the rig", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		fake := beadsfake.New(beadsfake.WithPrefix("gt"))
		fake.Seed(beads.Issue{ID: "gt-work", Title: "the work", Type: "task", Status: string(beads.StatusHooked)})
		work := land.Work{BeadID: "gt-work", Rig: "gastown", Branch: "polecat/opal/gt-work", Head: strings.Repeat("a", 40), Target: "main"}

		if err := markReadyToLand(fake, townRoot, work); err != nil {
			t.Fatalf("markReadyToLand: %v", err)
		}
		stamp, err := land.ReadyStamp(townRoot, "gastown")
		if err != nil {
			t.Fatalf("ReadyStamp: %v", err)
		}
		if stamp.IsZero() {
			t.Fatal("the drawn label left no ready stamp")
		}
	})

	t.Run("a failed label write stamps nothing", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		fake := beadsfake.New(beadsfake.WithPrefix("gt"))
		fake.Seed(beads.Issue{ID: "gt-work", Title: "the work", Type: "task", Status: string(beads.StatusHooked)})
		bd := failingUpdateClient{Client: fake, err: errors.New("dolt: connection reset")}
		work := land.Work{BeadID: "gt-work", Rig: "gastown", Branch: "polecat/opal/gt-work", Head: strings.Repeat("a", 40), Target: "main"}

		if err := markReadyToLand(bd, townRoot, work); err == nil {
			t.Fatal("markReadyToLand with a failing update = nil, want an error")
		}
		stamp, err := land.ReadyStamp(townRoot, "gastown")
		if err != nil {
			t.Fatalf("ReadyStamp: %v", err)
		}
		if !stamp.IsZero() {
			t.Errorf("a failed label write left ready stamp %v, want none", stamp)
		}
	})
}

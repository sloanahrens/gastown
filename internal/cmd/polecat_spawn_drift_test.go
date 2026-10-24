package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// TestImageDriftStartGivesBackTheRespawn guards gt-cjmt5: a session start
// refused because the sandbox image lags the installed gt is the seat's
// precondition, and the dispatcher defers on it and slings the bead again next
// tick. Each deferral leaves the respawn count where it found the tick, or a
// few ticks of one image rebuild reach the limit and the next sling is refused
// as a failed dispatch. A start that fails for anything else still counts, and
// it counts once however often the outcome is delivered.
//
// The spawn under test comes from freshSpawnInfo, the constructor the fresh
// allocation itself returns, so the count given back is the one the spawn path
// recorded rather than one the test wrote down.
func TestImageDriftStartGivesBackTheRespawn(t *testing.T) {
	t.Parallel()
	const bead = "gt-drift"
	drift := fmt.Errorf("starting polecat session: %w",
		fmt.Errorf("starting session: %w", session.ErrImageDrift))
	for _, tt := range []struct {
		name     string
		startErr error
		// record is whether this spawn recorded a count of its own: a forced
		// sling and one whose record failed do not.
		record bool
		want   int
	}{
		{name: "an image-drift deferral", startErr: drift, record: true, want: 1},
		{name: "a startup death", startErr: errors.New("starting session: agent exited"), record: true, want: 2},
		{name: "a deferral that recorded no count", startErr: drift, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			// One death already on the bead, so a give-back that removes the
			// wrong increment reads as a count of 0.
			if _, err := polecat.RecordBeadRespawn(townRoot, bead); err != nil {
				t.Fatalf("RecordBeadRespawn: %v", err)
			}
			// The seam prepareSlingPolecat hands its spawn: one count on the
			// bead, or none where the sling must not count.
			recorded := 0
			record := func() int {
				if !tt.record {
					return 0
				}
				n, err := polecat.RecordBeadRespawn(townRoot, bead)
				if err != nil {
					t.Fatalf("RecordBeadRespawn: %v", err)
				}
				recorded = n
				return n
			}

			s := freshSpawnInfo("rig",
				&polecat.Polecat{Name: "alpha", Branch: "polecat/alpha/" + bead},
				"alpha-session", "main", "main",
				SlingSpawnOptions{HookBead: bead}, record)
			if s.respawnCount != recorded {
				t.Fatalf("spawn carries respawn count %d, want the %d its recordRespawn returned",
					s.respawnCount, recorded)
			}
			// What slingSeatSpawn.spawn sets on the info it hands on.
			s.townRoot = townRoot

			// Delivered twice, as two readers of one start outcome would.
			s.finishStart("", tt.startErr)
			s.finishStart("", tt.startErr)

			if got := respawnCountOf(t, townRoot, bead); got != tt.want {
				t.Errorf("respawn count after the failed start = %d, want %d", got, tt.want)
			}
			// finishStart records the failed start against the bead as well as
			// handing the count back; that record is what makes an automatic
			// dispatcher back off instead of re-slinging on its next tick
			// (gt-wacl).
			if dispatch.StartupBackoff(townRoot, bead) == "" {
				t.Error("the failed start left no startup-failure record for the bead")
			}
		})
	}
}

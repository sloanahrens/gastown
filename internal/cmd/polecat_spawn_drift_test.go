package cmd

import (
	"errors"
	"fmt"
	"testing"

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
func TestImageDriftStartGivesBackTheRespawn(t *testing.T) {
	t.Parallel()
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
			if _, err := polecat.RecordBeadRespawn(townRoot, "gt-drift"); err != nil {
				t.Fatalf("RecordBeadRespawn: %v", err)
			}
			respawnCount := 0
			if tt.record {
				if _, err := polecat.RecordBeadRespawn(townRoot, "gt-drift"); err != nil {
					t.Fatalf("RecordBeadRespawn: %v", err)
				}
				respawnCount = 2
			}

			s := &SpawnedPolecatInfo{HookBead: "gt-drift", townRoot: townRoot, respawnCount: respawnCount}
			// Delivered twice, as two readers of one start outcome would.
			s.finishStart("", tt.startErr)
			s.finishStart("", tt.startErr)

			if got := respawnCountOf(t, townRoot, "gt-drift"); got != tt.want {
				t.Errorf("respawn count after the failed start = %d, want %d", got, tt.want)
			}
		})
	}
}

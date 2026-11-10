package daemon

import (
	"testing"
	"time"
)

// TestSlungFloorOverrunGuard: a run is never measured from before the session
// that is running it (gt-i6recg). The sling event of a dispatch whose session
// never started is inherited by the next sling of the bead; the session's own
// creation time is the floor for it, unless a restart accounts for the later
// session or the session was created within the sling's own startup.
func TestSlungFloorOverrunGuard(t *testing.T) {
	t.Parallel()
	slung := overrunSlung
	tests := []struct {
		name                      string
		sessionStart, restartedAt time.Time
		want                      time.Time
	}{
		{"an unknown session start leaves the sling", time.Time{}, time.Time{}, slung},
		{"a session created during the sling's own startup leaves it", slung.Add(2 * time.Minute), time.Time{}, slung},
		{"a session created exactly at the grace leaves it", slung.Add(overrunSessionGrace), time.Time{}, slung},
		{"a session created before the sling leaves it", slung.Add(-time.Minute), time.Time{}, slung},
		{"a session created long after the sling dates the run", slung.Add(32 * time.Minute), time.Time{}, slung.Add(32 * time.Minute)},
		{"a session created after a restart is the restart's to date", slung.Add(32 * time.Minute), slung.Add(31 * time.Minute), slung},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := slungFloor(slung, tt.sessionStart, tt.restartedAt); !got.Equal(tt.want) {
				t.Errorf("slungFloor = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunOverrunGuard_StaleSlingIsNotAnOverrun is the live case: a bead's sling
// at 14:12 failed in Prepare, the same bead was slung again 32 minutes later to
// the same seat name, and the guard stopped the new polecat a minute after it
// started, timing it from the failed sling. With the seat's session start as the
// floor it is not an overrun; a seat whose session started with its sling and ran
// past stop_after still stops.
func TestRunOverrunGuard_StaleSlingIsNotAnOverrun(t *testing.T) {
	t.Parallel()
	t.Run("a run start older than the seat's session is not an overrun", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		f := newOverrunFake(34*time.Minute, overrunSeat{
			Rig: "gastown", Polecat: "furiosa", Bead: "gt-b3ar38",
			SessionStart: overrunSlung.Add(33 * time.Minute),
		})
		d.runOverrunGuard(f.seams())
		if len(f.stops) != 0 || len(f.nudges) != 0 {
			t.Errorf("a polecat one minute into its session was nudged %v / stopped %v", f.nudges, f.stops)
		}
	})
	t.Run("an honest overrun still stops at stop_after", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		f := newOverrunFake(34*time.Minute, overrunSeat{
			Rig: "gastown", Polecat: "furiosa", Bead: "gt-b3ar38",
			SessionStart: overrunSlung.Add(time.Minute),
		})
		d.runOverrunGuard(f.seams())
		if len(f.stops) != 1 {
			t.Errorf("stops = %v, want the overrunning seat stopped", f.stops)
		}
	})
	t.Run("a session that outlives the floor by the stop threshold is stopped from the session", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		f := newOverrunFake(60*time.Minute, overrunSeat{
			Rig: "gastown", Polecat: "furiosa", Bead: "gt-b3ar38",
			SessionStart: overrunSlung.Add(33 * time.Minute),
		})
		d.runOverrunGuard(f.seams())
		if len(f.stops) != 1 {
			t.Errorf("stops = %v, want the seat stopped 27 minutes into its own session", f.stops)
		}
	})
}

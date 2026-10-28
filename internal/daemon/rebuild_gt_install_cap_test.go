package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// TestRebuildGTStarvedEscalation_FiresAtThePoolInstallCap (gt-pa4tg): a
// post-landing acquisition yields to an install in flight for at most
// slot.DefaultMaxInstallYield and then competes for a slot, and this job
// escalates a binary that stays due at rebuildGTStarveAfter. The pool's
// constant says the install job escalates at that same cap, so the alarm
// follows the cap: a block a minute short of it stays quiet, and one that
// reaches it escalates.
func TestRebuildGTStarvedEscalation_FiresAtThePoolInstallCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		age  time.Duration
		want int
	}{
		{"a block short of the pool's cap stays quiet", slot.DefaultMaxInstallYield - time.Minute, 0},
		{"a block at the pool's cap escalates", slot.DefaultMaxInstallYield, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec := daemonWithRecorder(t)
			d.rebuildGTBlock.open(d.clk().Now().Add(-tc.age))
			cycle := d.startDogCycle("rebuild_gt")
			defer cycle.close()

			d.rebuildGTNoteBlocked(cycle, true, "not quiet: a gate suite holds a slot")

			if got := len(rec.Escalations()); got != tc.want {
				t.Fatalf("escalations for a block of %s (the pool's cap is %s) = %d, want %d",
					tc.age, slot.DefaultMaxInstallYield, got, tc.want)
			}
		})
	}
}

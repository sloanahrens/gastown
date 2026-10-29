package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// TestRebuildGTStarvedEscalation_FiresAtThePoolInstallYield (gt-pa4tg) drives
// the starvation alarm through the call site at the pool's install cap: the
// duration a post-landing suite yields to an install in flight is also the
// point this job escalates a binary that stays due. The rows run it at 45
// minutes, so a threshold copied from the 30-minute default instead of read
// from the pool stays quiet at 45m and fails the at-cap row. The last row pins
// the default cap, which is what the town's own pool carries.
func TestRebuildGTStarvedEscalation_FiresAtThePoolInstallYield(t *testing.T) {
	t.Parallel()
	const longerThanDefault = 45 * time.Minute
	cases := []struct {
		name string
		cap  time.Duration
		age  time.Duration
		want int
	}{
		{"a block short of the pool's cap stays quiet", longerThanDefault, longerThanDefault - time.Minute, 0},
		{"a block at the pool's cap escalates", longerThanDefault, longerThanDefault, 1},
		{"a block past the default cap, under the pool's, stays quiet", longerThanDefault, slot.DefaultMaxInstallYield + time.Minute, 0},
		{"the default cap escalates at its own length", slot.DefaultMaxInstallYield, slot.DefaultMaxInstallYield, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec := daemonWithRecorder(t)
			d.rebuildGTInstallYieldFn = func() time.Duration { return tc.cap }
			d.rebuildGTBlock.open(d.clk().Now().Add(-tc.age))
			cycle := d.startDogCycle("rebuild_gt")
			defer cycle.close()

			d.rebuildGTNoteBlocked(cycle, true, "not quiet: a gate suite holds a slot")

			if got := len(rec.Escalations()); got != tc.want {
				t.Fatalf("escalations for a block of %s against a pool cap of %s = %d, want %d",
					tc.age, tc.cap, got, tc.want)
			}
		})
	}
}

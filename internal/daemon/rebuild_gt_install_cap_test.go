package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// TestRebuildGTStarvedEscalation_FiresAtTheDefaultInstallCap (gt-pa4tg) pins
// the install job's alarm at slot.DefaultMaxInstallYield: a block a minute
// short of it stays quiet, and one that reaches it escalates. It fails if the
// daemon's threshold moves off the default. It cannot fail on a revert to a
// literal equal to the default, because that changes no behaviour, and it does
// not cover a Pool.MaxInstallYield override, which the daemon never reads.
func TestRebuildGTStarvedEscalation_FiresAtTheDefaultInstallCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		age  time.Duration
		want int
	}{
		{"a block short of the default cap stays quiet", slot.DefaultMaxInstallYield - time.Minute, 0},
		{"a block at the default cap escalates", slot.DefaultMaxInstallYield, 1},
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
				t.Fatalf("escalations for a block of %s (the default install cap is %s) = %d, want %d",
					tc.age, slot.DefaultMaxInstallYield, got, tc.want)
			}
		})
	}
}

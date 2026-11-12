package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// keptSeatAttentionAfter is how long a kept sandbox seat may stand before the
// queue wakes the overseer. The record's since is when the keep's reason began,
// so a keep that holds past this window is one the queue shows the overseer
// (gt-l38v7r).
const keptSeatAttentionAfter = 30 * time.Minute

// collectKeptSandboxSeats raises one high item per kept sandbox seat whose
// record's since is more than keptSeatAttentionAfter before now. The item names
// the seat, its bead and the age, and carries the daemon's own keep reason
// verbatim, so the overseer reads the reason the keep gave and never a guess at
// it. The key is the seat, so a seat raises one item however many ticks it stays
// kept, and the item clears when the record is gone.
//
// The record is the only input. A record that cannot be read or decoded, or a
// store that cannot be listed, fails the collection rather than passing as an
// empty queue, so the previous tick's items are kept (collectAttention).
func (s *attentionSources) collectKeptSandboxSeats(_ context.Context) ([]attention.Item, error) {
	seats, bad, err := KeptSandboxSeats(s.d.config.TownRoot)
	if err != nil {
		return nil, err
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("unreadable kept sandbox seat records: %s", strings.Join(bad, ", "))
	}
	var out []attention.Item
	for _, seat := range seats {
		kept := s.now.Sub(seat.Since)
		if kept <= keptSeatAttentionAfter {
			continue
		}
		bead := seat.Bead
		if bead == "" {
			bead = "no recorded bead"
		}
		out = append(out, attention.Item{
			Key:      "sandbox-seat-kept:" + seat.Rig + "/" + seat.Name,
			Kind:     attention.KindSandboxSeatKept,
			Severity: attention.SeverityHigh,
			Rig:      seat.Rig,
			Bead:     seat.Bead,
			Summary: fmt.Sprintf("%s/%s kept %s on %s: %s",
				seat.Rig, seat.Name, parkedAge(kept), bead, seat.Reason),
		})
	}
	return out, nil
}

// keptSandboxSeatItem picks the kept-seat items out of the kind's previous
// items, so a failed collector keeps only its own (collectAttention).
func keptSandboxSeatItem(it attention.Item) bool {
	return it.Kind == attention.KindSandboxSeatKept && strings.HasPrefix(it.Key, "sandbox-seat-kept:")
}

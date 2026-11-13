package daemon

import (
	"context"
	"fmt"
	"sort"
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
// A seat whose gateway count cannot be kept in its record (the tally's alarm) is
// raised at once, naming the alarm, whatever the record says: a record that
// cannot be read is then no collect failure, since the alarm already raised its
// seat (gt-7h38pd). A seat whose tally is the only trace of it (its record cannot
// be read or written) is raised the same way.
//
// The record is the only input. A record that cannot be read or decoded, or a
// store that cannot be listed, fails the collection rather than passing as an
// empty queue, so the previous tick's items are kept (collectAttention).
func (s *attentionSources) collectKeptSandboxSeats(_ context.Context) ([]attention.Item, error) {
	town := s.d.config.TownRoot
	seats, bad, err := KeptSandboxSeats(town)
	if err != nil {
		return nil, err
	}
	tallies := s.d.gatewayTallySnapshot()
	var unalarmed []string
	for _, path := range bad {
		if t, ok := tallies[badKeptSeatKey(town, path)]; ok && t.alarm != "" {
			continue
		}
		unalarmed = append(unalarmed, path)
	}
	if len(unalarmed) > 0 {
		return nil, fmt.Errorf("unreadable kept sandbox seat records: %s", strings.Join(unalarmed, ", "))
	}
	limit := s.d.gatewayRetryLimit()
	var out []attention.Item
	seen := make(map[string]bool, len(seats))
	for _, seat := range seats {
		key := gatewayTallyKey(seat.Rig, seat.Name)
		seen[key] = true
		tally := tallies[key]
		kept := s.now.Sub(seat.Since)
		// A seat whose gateway has stayed silent for the retry limit's ticks is
		// raised whatever its age (gt-7h38pd). The tally counts the same ticks
		// when the record cannot hold the count.
		if tally.alarm == "" && kept <= keptSeatAttentionAfter && max(seat.Tries, tally.tries) < limit {
			continue
		}
		reason := seat.Reason
		if tally.alarm != "" {
			reason = tally.alarm
		}
		out = append(out, keptSeatItem(seat.Rig, seat.Name, seat.Bead, parkedAge(kept), reason))
	}
	alarmOnly := make([]string, 0, len(tallies))
	for key, tally := range tallies {
		if !seen[key] && tally.alarm != "" {
			alarmOnly = append(alarmOnly, key)
		}
	}
	sort.Strings(alarmOnly)
	for _, key := range alarmOnly {
		rig, name, _ := strings.Cut(key, "/")
		out = append(out, keptSeatItem(rig, name, "", "with no readable record", tallies[key].alarm))
	}
	return out, nil
}

// keptSeatItem is the kept-seat item of one seat, carrying reason verbatim.
func keptSeatItem(rig, name, bead, age, reason string) attention.Item {
	shown := bead
	if shown == "" {
		shown = "no recorded bead"
	}
	return attention.Item{
		Key:      "sandbox-seat-kept:" + rig + "/" + name,
		Kind:     attention.KindSandboxSeatKept,
		Severity: attention.SeverityHigh,
		Rig:      rig,
		Bead:     bead,
		Summary:  fmt.Sprintf("%s/%s kept %s on %s: %s", rig, name, age, shown, reason),
	}
}

// keptSandboxSeatItem picks the kept-seat items out of the kind's previous
// items, so a failed collector keeps only its own (collectAttention).
func keptSandboxSeatItem(it attention.Item) bool {
	return it.Kind == attention.KindSandboxSeatKept && strings.HasPrefix(it.Key, "sandbox-seat-kept:")
}

package cmd

import (
	"os"
	"sort"
	"sync"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
)

// dashBackoff reads every rig's landing backoff snapshot and turns each bead
// the landing worker is retrying into a Landings row. A landing that keeps
// failing was otherwise visible only in daemon.log (gt-fn9e6.44).
type dashBackoff struct {
	townRoot string

	mu     sync.Mutex
	stamps map[string]fileStamp
	rows   []dashboard.LandingRow
}

func newDashBackoff(townRoot string) *dashBackoff {
	return &dashBackoff{townRoot: townRoot, stamps: map[string]fileStamp{}}
}

// get returns one row per failing landing, re-reading only the files whose
// size or modification time moved.
func (b *dashBackoff) get() []dashboard.LandingRow {
	b.mu.Lock()
	defer b.mu.Unlock()
	rigs, _ := knownRigNames(b.townRoot)
	changed := false
	next := make(map[string]fileStamp, len(rigs))
	for _, rig := range rigs {
		path, err := landings.BackoffPath(b.townRoot, rig)
		if err != nil {
			continue
		}
		st := fileStamp{}
		if info, err := os.Stat(path); err == nil {
			st = fileStamp{info.Size(), info.ModTime()}
		}
		next[path] = st
		if old, ok := b.stamps[path]; !ok || old != st {
			changed = true
		}
	}
	if !changed {
		return b.rows
	}
	var rows []dashboard.LandingRow
	for _, rig := range rigs {
		path, err := landings.BackoffPath(b.townRoot, rig)
		if err != nil {
			continue
		}
		snap, err := landings.ReadBackoff(path)
		if err != nil {
			// A snapshot that cannot be read is not a landing in trouble; the
			// rows it would have contributed are missing, not wrong.
			continue
		}
		rows = append(rows, backoffRows(rig, snap)...)
	}
	b.rows, b.stamps = rows, next
	return b.rows
}

// backoffRows is one rig's snapshot as Landings rows, newest failure first.
func backoffRows(rig string, snap landings.BackoffState) []dashboard.LandingRow {
	rows := make([]dashboard.LandingRow, 0, len(snap.Beads))
	for _, b := range snap.Beads {
		row := dashboard.LandingRow{
			At: snap.At, Bead: b.Bead, Rig: rig, Outcome: "backoff",
			Stage: b.Stage, Failures: b.Failures, Detail: b.Error,
		}
		if !b.NextTry.IsZero() {
			next := b.NextTry
			row.NextTry = &next
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	return rows
}

// withBackoffRows puts the landings that are failing in backoff above the
// day's rows, like the ones running now: a landing that is not clearing is
// not one of the day's results either. It names each bead's title the way the
// rows it joins are named.
func withBackoffRows(rows, backoff []dashboard.LandingRow, title func(rig, bead string) string) []dashboard.LandingRow {
	if len(backoff) == 0 {
		return rows
	}
	out := make([]dashboard.LandingRow, 0, len(backoff)+len(rows))
	for _, row := range backoff {
		if title != nil {
			row.Title = title(row.Rig, row.Bead)
		}
		out = append(out, row)
	}
	return append(out, rows...)
}

// Package backlog builds the weekly backlog digest: for each rig, the open work
// beads nobody has touched for a number of days, oldest first, with their count
// by priority. It replaces the advisory jev_stale read with a deterministic one
// that needs no model. Build is pure.
package backlog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/stale"
)

// RigBeads is one rig's open beads.
type RigBeads struct {
	Rig   string
	Beads []*beads.Issue
}

// Entry is one stale bead. A bead with no update time is Unknown and sorts as
// the oldest.
type Entry struct {
	ID       string
	Priority int
	Assignee string
	Title    string
	AgeDays  int
	Unknown  bool
}

// Age is the entry's age as text: "unknown" or "<n>d".
func (e Entry) Age() string {
	if e.Unknown {
		return "unknown"
	}
	return fmt.Sprintf("%dd", e.AgeDays)
}

// RigDigest is one rig's stale beads, oldest first, and their count by priority.
type RigDigest struct {
	Rig        string
	Count      int
	ByPriority map[int]int
	Beads      []Entry
}

// Digest is the digest of every rig given, in the order given.
type Digest struct {
	Rigs []RigDigest
}

// Build keeps, per rig, the open work beads (task, bug or feature; not a crew
// seat's bead) last updated more than days ago, and counts them by priority.
// now is a parameter: Build reads no clock. A bead with no update time is
// listed as the oldest, never skipped.
func Build(rigs []RigBeads, now time.Time, days int) Digest {
	d := Digest{Rigs: make([]RigDigest, 0, len(rigs))}
	for _, r := range rigs {
		rd := RigDigest{Rig: r.Rig, ByPriority: map[int]int{}}
		for _, b := range r.Beads {
			if b == nil || !stale.IsWorkType(b.Type) || isCrewSeat(b.ID) {
				continue
			}
			e := Entry{ID: b.ID, Priority: b.Priority, Assignee: b.Assignee, Title: b.Title}
			if updated := beads.ParseIssueTime(b.UpdatedAt); updated.IsZero() {
				e.Unknown = true
			} else if age := now.Sub(updated); age > time.Duration(days)*24*time.Hour {
				e.AgeDays = int(age / (24 * time.Hour))
			} else {
				continue
			}
			rd.Beads = append(rd.Beads, e)
			rd.ByPriority[b.Priority]++
		}
		rd.Count = len(rd.Beads)
		slices.SortFunc(rd.Beads, func(a, b Entry) int {
			if a.Unknown != b.Unknown {
				if a.Unknown {
					return -1
				}
				return 1
			}
			if a.AgeDays != b.AgeDays {
				return b.AgeDays - a.AgeDays
			}
			return strings.Compare(a.ID, b.ID)
		})
		d.Rigs = append(d.Rigs, rd)
	}
	return d
}

// isCrewSeat reports whether a bead ID names a crew seat's task bead,
// "<prefix>-crew-<name>": the marker counts only in the second segment, so a
// work bead such as gt-fix-crew-1 is still listed (the rule of stale.isCrewSeat).
func isCrewSeat(id string) bool {
	_, rest, ok := strings.Cut(id, "-")
	return ok && strings.HasPrefix(rest, "crew-") && len(rest) > len("crew-")
}

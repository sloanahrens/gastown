package cmd

import (
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// poolReconciled is the pool's comparison of the sandboxes the gateway runs
// against the seats the pool counts.
type poolReconciled struct {
	// Uncounted names each running sandbox whose seat no counted session holds.
	Uncounted []string
	// NoSandbox names each counted session seat with no running sandbox. A claim
	// and a landing seat name no sandbox by kind, so neither is listed.
	NoSandbox []string
}

// poolReconcile compares the running sandboxes with the counted seats. A
// sandbox's name is its seat's session name, which poolSession carries in
// sandbox: a live session's own name, and the name a dead-hooked seat's session
// had, which its dead/-prefixed name no longer shows. A parkedStale seat is
// released and counts in no field of poolOccupancy, so it is not counted here
// either.
func poolReconcile(counted []poolSession, running []string) poolReconciled {
	var r poolReconciled
	seats := make(map[string]bool, len(counted))
	for _, s := range counted {
		if s.parkedStale || s.claim || s.landing || s.sandbox == "" {
			continue
		}
		seats[s.sandbox] = true
	}
	sandboxes := make(map[string]bool, len(running))
	for _, name := range running {
		sandboxes[name] = true
		if !seats[name] {
			r.Uncounted = append(r.Uncounted, name)
		}
	}
	for _, s := range counted {
		if s.parkedStale || s.claim || s.landing || s.sandbox == "" {
			continue
		}
		if !sandboxes[s.sandbox] {
			r.NoSandbox = append(r.NoSandbox, s.sandbox)
		}
	}
	return r
}

// poolReconcileNotes is the reconciliation a reading prints: each named seat
// with the disposition word its polecat's own state gives, or the one note that
// the gateway listing could not be read.
type poolReconcileNotes struct {
	Uncounted []specdispatch.HeldSeat
	NoSandbox []specdispatch.HeldSeat
	// Unavailable says the gateway listing could not be read. It prints its own
	// note and no seat lists: an unknown listing is not an empty comparison.
	Unavailable bool
}

// poolDispositionUnknown is the word a seat prints when its own state cannot be
// read. An unknown disposition is not "none", and the seat is still named.
const poolDispositionUnknown = "unknown"

// Lines renders the notes, one per line, and nothing for a clean reading. The
// seats go through specdispatch.HeldSeatList, the one formatter the tick line's
// held-seat list and gt pool show already print through, so a seat reads the
// same way everywhere.
func (n poolReconcileNotes) Lines() []string {
	if n.Unavailable {
		return []string{"sandbox listing unavailable"}
	}
	var out []string
	if list := specdispatch.HeldSeatList(n.Uncounted); list != "" {
		out = append(out, "uncounted sandboxes: "+list)
	}
	if list := specdispatch.HeldSeatList(n.NoSandbox); list != "" {
		out = append(out, "counted without a sandbox: "+list)
	}
	return out
}

// String is the notes joined for one line, the form the tick line appends them
// in: "" for a clean reading.
func (n poolReconcileNotes) String() string {
	return strings.Join(n.Lines(), "; ")
}

// specRosterPicture is the tick's roster with the reconciliation's notes
// appended after "; ", the separator dispatchRosterSeats cuts the note suffix
// at so the seats stay readable. A clean reading is the picture unchanged.
func specRosterPicture(picture string, notes poolReconcileNotes) string {
	if n := notes.String(); n != "" {
		return picture + "; " + n
	}
	return picture
}

// poolReconcileReads is what one reconciliation reads: the gateway's running
// sandboxes, the seat a sandbox name belongs to, and a seat's own disposition.
// They are passed in so a test drives the comparison with fakes and a tick
// reads its town without a second gateway call.
type poolReconcileReads struct {
	// Running returns the names of the sandboxes the gateway lists as Ready. An
	// error is a listing that could not be read, never an empty list.
	Running func() ([]string, error)
	// SeatOf splits a sandbox name ("<prefix>-<polecat>") into its rig and
	// polecat through the town's rig registry. ok is false for a name no rig's
	// prefix matches, which still prints, with an unknown disposition.
	SeatOf func(sandbox string) (rig, polecat string, ok bool)
	// Dispose reads a polecat's own state for its disposition word.
	Dispose func(rig, name string) (polecat.WorkstateDisposition, error)
}

// notes is the reconciliation of counted against what the reads answer. A read
// set with no Running reads nothing and prints nothing (a test's tick, or a
// wiring that was not passed); a listing that fails is the unavailable note and
// not an error, so the tick and the page go on.
func (r poolReconcileReads) notes(counted []poolSession) poolReconcileNotes {
	if r.Running == nil {
		return poolReconcileNotes{}
	}
	running, err := r.Running()
	if err != nil {
		return poolReconcileNotes{Unavailable: true}
	}
	return poolReconcileNotesOf(poolReconcile(counted, running), r.SeatOf, r.Dispose)
}

// poolReconcileNotesOf names the seats a reconciliation found. Each seat prints
// with the disposition word its polecat's own state gives — its reuse status
// when it has one, else its verdict, the words `gt polecat list` reports —
// and "unknown" when that state cannot be read, so a seat is never dropped for
// a failed read.
func poolReconcileNotesOf(r poolReconciled, seatOf func(sandbox string) (rig, polecat string, ok bool), dispose func(rig, polecat string) (polecat.WorkstateDisposition, error)) poolReconcileNotes {
	word := func(rig, polecat string) string {
		if dispose == nil || rig == "" || polecat == "" {
			return poolDispositionUnknown
		}
		d, err := dispose(rig, polecat)
		if err != nil {
			return poolDispositionUnknown
		}
		return poolDispositionWord(d)
	}
	note := func(names []string) []specdispatch.HeldSeat {
		held := make([]specdispatch.HeldSeat, 0, len(names))
		for _, name := range names {
			rig, polecat := "", ""
			if seatOf != nil {
				rig, polecat, _ = seatOf(name)
			}
			held = append(held, specdispatch.HeldSeat{Name: name, State: word(rig, polecat)})
		}
		return held
	}
	return poolReconcileNotes{Uncounted: note(r.Uncounted), NoSandbox: note(r.NoSandbox)}
}

// poolDispositionWord is a seat's disposition as `gt polecat list` names it: the
// reuse status when the state carries one, else the verdict.
func poolDispositionWord(d polecat.WorkstateDisposition) string {
	if d.ReuseStatus != "" {
		return d.ReuseStatus
	}
	if d.Verdict != "" {
		return d.Verdict
	}
	return poolDispositionUnknown
}

// poolReconcileTownReads is the real reads for a town: one census read of the
// gateway's running sandboxes, the rig registry's prefix→rig map, and the pool's
// own seat-state read. The census is built here and read once, the way a sling
// reads it, so a tick pays one gateway listing.
func poolReconcileTownReads(townRoot string) poolReconcileReads {
	return poolReconcileReads{
		Running: func() ([]string, error) {
			return session.NewSandboxCensus(townRoot, nil).Running("")
		},
		SeatOf:  poolSandboxSeatFor(townRoot),
		Dispose: poolReconcileDispositionFor(townRoot),
	}
}

// poolSandboxSeatFor splits a sandbox name at the longest rig prefix the town's
// registry names, the way the session name was built (session.PolecatSessionName).
// A town whose registry cannot be read resolves nothing, which prints every
// seat with an unknown disposition rather than dropping it.
func poolSandboxSeatFor(townRoot string) func(sandbox string) (rig, polecat string, ok bool) {
	rigs, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil
	}
	type prefixRig struct{ prefix, rig string }
	var prefixes []prefixRig
	for rig, entry := range rigs.Rigs {
		if entry.BeadsConfig == nil || entry.BeadsConfig.Prefix == "" {
			continue
		}
		prefixes = append(prefixes, prefixRig{prefix: entry.BeadsConfig.Prefix, rig: rig})
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i].prefix) > len(prefixes[j].prefix) })
	return func(sandbox string) (string, string, bool) {
		for _, p := range prefixes {
			rest, ok := strings.CutPrefix(sandbox, p.prefix+"-")
			if ok && rest != "" {
				return p.rig, rest, true
			}
		}
		return "", "", false
	}
}

// poolReconcileDispositionFor is the pool's seat-state read as the disposition
// word's source. A caller with no town to read answers none, and each seat
// prints "unknown".
func poolReconcileDispositionFor(townRoot string) func(rig, name string) (polecat.WorkstateDisposition, error) {
	read := poolDispositionFor(townRoot)
	if read == nil {
		return nil
	}
	return func(rig, name string) (polecat.WorkstateDisposition, error) {
		state, err := read(rig, name)
		if err != nil {
			return polecat.WorkstateDisposition{}, err
		}
		return state.Disposition, nil
	}
}

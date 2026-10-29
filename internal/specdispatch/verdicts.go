package specdispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// VerdictReasonLimit is how many runes of a verdict's reason the ledger keeps.
const VerdictReasonLimit = 300

// Entry is one bead's verdict from a single dispatch tick: what the dispatcher
// decided about it and why.
type Entry struct {
	Bead   string
	Rig    string
	Class  string
	Reason string
}

// LedgerVerdict is what the ledger remembers of a bead the dispatcher did not
// sling: its latest class and reason, the tick that first saw it, the tick that
// last saw it, and how many consecutive ticks it has stood (gt-n05o0). It is
// named apart from the lint's Verdict, which this package already declares.
type LedgerVerdict struct {
	Bead    string    `json:"bead"`
	Rig     string    `json:"rig"`
	Class   string    `json:"class"`
	Reason  string    `json:"reason"`
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
	Ticks   int       `json:"ticks"`
}

// Ledger is the last dispatch tick's verdicts, keyed by bead id.
type Ledger struct {
	SavedAt  time.Time                `json:"saved_at"`
	Verdicts map[string]LedgerVerdict `json:"verdicts"`
}

// Record replaces the ledger with one tick's entries at at: a bead the ledger
// already holds keeps its FirstAt and gains a tick, and a bead absent from
// entries is dropped, because the ledger states what the tick saw and a bead
// no longer seen is one the dispatcher has taken or stopped considering.
func (l *Ledger) Record(at time.Time, entries []Entry) {
	next := make(map[string]LedgerVerdict, len(entries))
	for _, e := range entries {
		v := LedgerVerdict{
			Bead:    e.Bead,
			Rig:     e.Rig,
			Class:   e.Class,
			Reason:  cutReason(e.Reason),
			FirstAt: at,
			LastAt:  at,
			Ticks:   1,
		}
		if prev, ok := l.Verdicts[e.Bead]; ok {
			v.FirstAt = prev.FirstAt
			v.Ticks = prev.Ticks + 1
		}
		next[e.Bead] = v
	}
	l.Verdicts = next
	l.SavedAt = at
}

// cutReason bounds a reason so one runaway message cannot bloat every tick's
// file; the cut counts runes, so it never splits a character.
func cutReason(s string) string {
	if utf8.RuneCountInString(s) <= VerdictReasonLimit {
		return s
	}
	return string([]rune(s)[:VerdictReasonLimit])
}

// LedgerPath is the file holding townRoot's dispatch verdicts.
func LedgerPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "spec_dispatch", "verdicts.json")
}

// LoadLedger reads the ledger at path: a missing file is an empty ledger and no
// error, and one it cannot read or parse is an empty ledger with the error, so
// a caller never records onto verdicts it failed to see.
func LoadLedger(path string) (*Ledger, error) {
	empty := func() *Ledger { return &Ledger{Verdicts: map[string]LedgerVerdict{}} }
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return empty(), nil
		}
		return empty(), fmt.Errorf("read verdict ledger %s: %w", path, err)
	}
	var l Ledger
	if err := json.Unmarshal(raw, &l); err != nil {
		return empty(), fmt.Errorf("parse verdict ledger %s: %w", path, err)
	}
	if l.Verdicts == nil {
		l.Verdicts = map[string]LedgerVerdict{}
	}
	return &l, nil
}

// Save writes the ledger to path, creating its directory, through a temporary
// file that a rename publishes, so a reader never sees a partial file.
func (l *Ledger) Save(path string) error {
	rec := Ledger{SavedAt: l.SavedAt, Verdicts: l.Verdicts}
	if rec.Verdicts == nil {
		rec.Verdicts = map[string]LedgerVerdict{}
	}
	if err := atomicfile.EnsureDirAndWriteJSON(path, rec); err != nil {
		return fmt.Errorf("save verdict ledger %s: %w", path, err)
	}
	return nil
}

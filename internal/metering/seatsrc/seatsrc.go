// Package seatsrc turns the seat-end ledger into the seat intervals the monthly
// export reads.
//
// The ledger holds one line per removed sandbox record, written to
// session.SandboxSeatLedgerPath. A seat still running is not in the ledger
// until it ends, so a month read before every seat ends undercounts that
// month's seat-hours. Seats is pure over its reader: it opens no file and reads
// no clock or environment. The invoice command reads it (gt-ehbgh).
package seatsrc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
)

// The fixed keys of Result.Skipped, one per reason a ledger line is not used.
const (
	// SkipInvalidJSON counts a line the JSON decoder rejected, including a line
	// whose created_at or ended_at is not RFC 3339: the decoder rejects that
	// line before the zero-time check reads it.
	SkipInvalidJSON    = "invalid JSON"
	SkipEmptyWorkspace = "empty workspace"
	SkipEmptyProfile   = "empty profile"
	SkipZeroTime       = "zero time"
	SkipEndBeforeStart = "ended_at before created_at"
)

// Result is the seat intervals one read of the ledger yields and the counts of
// the lines it did not use.
type Result struct {
	// Seats is the intervals the kept lines name, in ledger order.
	Seats []export.Seat
	// Skipped counts lines left out, keyed by the reason strings above. A
	// reason that did not occur has no entry, and a blank line is ignored
	// without one.
	Skipped map[string]int
}

// line is the part of a seat-end line this package reads; the ledger's other
// fields name the session and the bead, which a seat interval does not need.
type line struct {
	Workspace string    `json:"workspace"`
	Profile   string    `json:"profile"`
	CreatedAt time.Time `json:"created_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// Seats reads the seat-end ledger from r and returns its seat intervals and the
// counts of the lines it did not use. A line it cannot use is skipped and
// counted by reason, never guessed.
func Seats(r io.Reader) (Result, error) {
	res := Result{Skipped: map[string]int{}}
	br := bufio.NewReader(r)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			res.add(raw)
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		return Result{}, fmt.Errorf("seatsrc: reading the seat-end ledger: %w", err)
	}
}

// add keeps raw's interval, or counts the reason raw cannot be used.
func (res *Result) add(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		res.Skipped[SkipInvalidJSON]++
		return
	}
	switch {
	case l.Workspace == "":
		res.Skipped[SkipEmptyWorkspace]++
	case l.Profile == "":
		res.Skipped[SkipEmptyProfile]++
	case l.CreatedAt.IsZero() || l.EndedAt.IsZero():
		res.Skipped[SkipZeroTime]++
	case l.EndedAt.Before(l.CreatedAt):
		res.Skipped[SkipEndBeforeStart]++
	default:
		res.Seats = append(res.Seats, export.Seat{
			Workspace: l.Workspace,
			Profile:   l.Profile,
			Start:     l.CreatedAt,
			End:       l.EndedAt,
		})
	}
}

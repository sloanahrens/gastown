// Package spans records how long each polecat seat spent hooked on a bead and
// summarizes that time per bead (gt-es74m).
//
// The log is append-only JSONL, one file per bead, so recording never touches
// bd or Dolt and concurrent seats (and the daemon) append without a lock.
package spans

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Event names in the log.
const (
	EventStart = "start"
	EventEnd   = "end"
)

// Reason is why an end event was written.
type Reason string

const (
	ReasonDone      Reason = "done"
	ReasonOverrun   Reason = "overrun"
	ReasonEscalated Reason = "escalated"
	// ReasonDead closes an attempt whose seat died without reporting an end:
	// the elapsed time is a lower bound on the work done.
	ReasonDead Reason = "dead"
)

// Clock supplies the time a start was recorded; tests inject a fixed one.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Store appends spans under one directory and reads them back.
type Store struct {
	dir   string
	clock Clock
}

// New returns a store whose logs live in dir. A nil clock means the system
// clock.
func New(dir string, clock Clock) *Store {
	if clock == nil {
		clock = systemClock{}
	}
	return &Store{dir: dir, clock: clock}
}

// DefaultDir is where the daemon keeps spans: <town>/.runtime/polecat-spans.
func DefaultDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "polecat-spans")
}

// Start appends a start event for seat working bead.
//
// A start written while the same seat already has an open attempt is not
// refused here: the appends are the record, and Summarize skips the duplicate.
func (s *Store) Start(bead, seat string) error {
	if err := validateBead(bead); err != nil {
		return err
	}
	if seat == "" {
		return fmt.Errorf("polecat spans: empty seat for bead %q", bead)
	}
	return s.append(bead, event{
		Bead:  bead,
		Seat:  seat,
		Event: EventStart,
		At:    formatTime(s.clock.Now()),
	})
}

// End appends an end event for seat working bead at at. ReasonDead marks the
// attempt as a lower bound: the seat died, so the time to its close is all the
// record can claim.
func (s *Store) End(bead, seat string, reason Reason, at time.Time) error {
	if err := validateBead(bead); err != nil {
		return err
	}
	if seat == "" {
		return fmt.Errorf("polecat spans: empty seat for bead %q", bead)
	}
	if !validReason(reason) {
		return fmt.Errorf("polecat spans: unknown reason %q for bead %q", reason, bead)
	}
	return s.append(bead, event{
		Bead:       bead,
		Seat:       seat,
		Event:      EventEnd,
		At:         formatTime(at),
		Reason:     string(reason),
		LowerBound: reason == ReasonDead,
	})
}

// EndOpen ends seat's open attempt on bead at at and reports whether it had
// one. An at earlier than the attempt's start is moved to the start: the time
// can be second-hand evidence (a dead seat's last-seen sample, say), and an end
// dated before its start would be skipped on read instead of closing the
// attempt.
func (s *Store) EndOpen(bead, seat string, reason Reason, at time.Time) (bool, error) {
	sum, err := s.Summarize(bead)
	if err != nil {
		return false, err
	}
	for _, a := range sum.Attempts {
		if a.Seat != seat || !a.Open {
			continue
		}
		if at.Before(a.StartedAt) {
			at = a.StartedAt
		}
		return true, s.End(bead, seat, reason, at)
	}
	return false, nil
}

// Summary is the time a bead's seats spent hooked on it across every attempt.
type Summary struct {
	// Secs is the sum of the closed attempts' durations.
	Secs float64
	// Attempts is one entry per start event, in the order the starts were
	// recorded. An open attempt is included with zero Secs.
	Attempts []Attempt
	// LowerBound is set when some attempt is open, or was closed by a dead
	// seat, so Secs may understate the real time.
	LowerBound bool
	// Skipped counts events that could not be paired: an end without a start,
	// a duplicate start, or an end earlier than its start.
	Skipped int
	// Corrupt counts lines that did not parse or did not describe this bead.
	Corrupt int
}

// Attempt is one seat's start..end pair.
type Attempt struct {
	Seat string
	// StartedAt is the time of the attempt's start event.
	StartedAt time.Time
	Secs      float64
	// Open marks an attempt its seat has not ended, so a later event can close
	// it.
	Open bool
	// LowerBound marks an attempt whose duration is a floor: it is still open,
	// or a dead seat closed it.
	LowerBound bool
}

// HasOpen reports whether seat has an attempt on this summary's bead that it
// has not ended.
func (sum Summary) HasOpen(seat string) bool {
	for _, a := range sum.Attempts {
		if a.Seat == seat && a.Open {
			return true
		}
	}
	return false
}

// Summarize reads bead's log and pairs its start and end events. A bead with
// no log summarizes to zero attempts; malformed and unpaired lines are counted
// and never fail the read.
func (s *Store) Summarize(bead string) (Summary, error) {
	var sum Summary
	if err := validateBead(bead); err != nil {
		return sum, err
	}
	f, err := os.Open(s.path(bead))
	if errors.Is(err, fs.ErrNotExist) {
		return sum, nil
	}
	if err != nil {
		return sum, fmt.Errorf("reading polecat spans for %q: %w", bead, err)
	}
	defer f.Close()

	open := map[string]openAttempt{}
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		// A final line without its newline is a torn append or a hand-edited
		// file; it is read as a line and counted corrupt if it does not parse.
		if line != "" {
			sum.readLine(line, bead, open)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return sum, fmt.Errorf("reading polecat spans for %q: %w", bead, readErr)
		}
	}
	for _, o := range open {
		// The seat never reported an end, so the attempt contributes no time
		// but makes the bead's total a lower bound.
		sum.Attempts[o.index].LowerBound = true
		sum.LowerBound = true
	}
	for _, a := range sum.Attempts {
		sum.Secs += a.Secs
	}
	return sum, nil
}

// openAttempt is the attempt a seat has started but not ended.
type openAttempt struct {
	index int
	at    time.Time
}

// readLine folds one log line into sum. open holds the attempts still waiting
// for an end, keyed by seat, so seats that interleave their events still pair.
func (sum *Summary) readLine(line, bead string, open map[string]openAttempt) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var ev event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		sum.Corrupt++
		return
	}
	at, err := time.Parse(time.RFC3339, ev.At)
	if err != nil || ev.Bead != bead || ev.Seat == "" {
		sum.Corrupt++
		return
	}
	switch ev.Event {
	case EventStart:
		if _, ok := open[ev.Seat]; ok {
			sum.Skipped++
			return
		}
		sum.Attempts = append(sum.Attempts, Attempt{Seat: ev.Seat, StartedAt: at, Open: true})
		open[ev.Seat] = openAttempt{index: len(sum.Attempts) - 1, at: at}
	case EventEnd:
		o, ok := open[ev.Seat]
		if !ok || at.Before(o.at) {
			sum.Skipped++
			return
		}
		a := &sum.Attempts[o.index]
		a.Secs = at.Sub(o.at).Seconds()
		a.Open = false
		a.LowerBound = ev.LowerBound
		sum.LowerBound = sum.LowerBound || ev.LowerBound
		delete(open, ev.Seat)
	default:
		sum.Corrupt++
	}
}

// event is one line of a bead's log.
type event struct {
	Bead       string `json:"bead"`
	Seat       string `json:"seat"`
	Event      string `json:"event"`
	At         string `json:"at"`
	Reason     string `json:"reason,omitempty"`
	LowerBound bool   `json:"lower_bound"`
}

func (s *Store) append(bead string, ev event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encoding polecat span for %q: %w", bead, err)
	}
	line = append(line, '\n')
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating polecat spans directory %s: %w", s.dir, err)
	}
	f, err := os.OpenFile(s.path(bead), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening polecat span log for %q: %w", bead, err)
	}
	// One O_APPEND write of the whole line keeps concurrent seats from
	// interleaving their records without a lock.
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("appending polecat span for %q: %w", bead, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing polecat span log for %q: %w", bead, err)
	}
	return nil
}

func (s *Store) path(bead string) string {
	return filepath.Join(s.dir, bead+".jsonl")
}

// validateBead keeps the bead id a single safe path component: a separator or
// ".." would write outside the store's directory.
func validateBead(bead string) error {
	if bead == "" || bead == "." || bead == ".." ||
		strings.ContainsAny(bead, `/\`) || strings.ContainsRune(bead, 0) {
		return fmt.Errorf("polecat spans: invalid bead id %q", bead)
	}
	return nil
}

func validReason(reason Reason) bool {
	switch reason {
	case ReasonDone, ReasonOverrun, ReasonEscalated, ReasonDead:
		return true
	}
	return false
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

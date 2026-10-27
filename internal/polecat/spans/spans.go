// Package spans records how long each polecat seat spent hooked on a bead and
// summarizes that time per bead (gt-es74m).
//
// The log is append-only JSONL, one file per bead, so recording never touches
// bd or Dolt. Appends serialize on a per-bead lock file, so concurrent seats
// (and the daemon) cannot interleave a line.
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
	"unicode"

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
	ReasonDeferred  Reason = "deferred"
	ReasonOverrun   Reason = "overrun"
	ReasonEscalated Reason = "escalated"
	// ReasonStopped closes an attempt whose seat a stop or a nuke ended from
	// outside: the seat reported nothing, so the host writes the end.
	ReasonStopped Reason = "stopped"
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
	if err := validateSeat(seat, bead); err != nil {
		return err
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
// record can claim. ReasonStopped is one too: a stop ends the session, not the
// work, and a seat resumed on the same bead continues it with no second start.
func (s *Store) End(bead, seat string, reason Reason, at time.Time) error {
	if err := validateBead(bead); err != nil {
		return err
	}
	if err := requireSeat(seat, bead); err != nil {
		return err
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
		LowerBound: reason == ReasonDead || reason == ReasonStopped,
	})
}

// EndOpen ends seat's open attempt on bead at at and reports whether it had
// one. An at earlier than the attempt's start is moved to the start: the time
// can be second-hand evidence (a dead seat's last-seen sample, say), and an end
// dated before its start would be skipped on read instead of closing the
// attempt.
func (s *Store) EndOpen(bead, seat string, reason Reason, at time.Time) (bool, error) {
	if err := requireSeat(seat, bead); err != nil {
		return false, err
	}
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

// EndSeatOpen ends the newest attempt seat left open in this store, and reports
// the bead whose log it closed.
//
// It is what a stop writes: the host ends a seat's attempt from outside, and
// must do it without asking Dolt which bead the seat held — gt down and gt
// polecat nuke stop seats while the database is unreachable or being stopped.
// The logs are the only record of which bead a seat held, so the store reads
// its own directory (one file per bead) rather than keeping a second index that
// could disagree with them.
//
// Only the newest open attempt is closed. A seat works one bead at a time, so
// an older attempt still open is a stale one from an earlier turn, and closing
// it at this stop would credit it with time the seat did not hold. For the same
// reason a log that cannot be read closes nothing: it could hold the seat's
// newest attempt, and the newest readable one would then be a stale one. The
// attempt stays open, which the summary reports as a lower bound and the
// landing's first-sight close ends, and the read failure goes back to the
// caller, whose stop stands either way.
//
// Every log is read, because any of them could hold the attempt: there is no
// index, and a second one could disagree with the logs it points into. The
// store is one small file per bead worked in this town, and a stop is rare, so
// the read is a stop's cost and not a poll's.
func (s *Store) EndSeatOpen(seat string, reason Reason, at time.Time) (string, bool, error) {
	if seat == "" {
		return "", false, errors.New("polecat spans: empty seat")
	}
	// An absent store is not a read that failed: no bead has a log yet, so the
	// seat holds no attempt and there is nothing to close.
	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("reading the polecat span store %s: %w", s.dir, err)
	}
	var bead string
	var started time.Time
	var skipped []error
	for _, ent := range entries {
		// One file per bead, named for the bead: anything else in the store is
		// a lock or a file another writer left, and is not a seat's attempt.
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		name := strings.TrimSuffix(ent.Name(), ".jsonl")
		if !ValidBeadID(name) {
			continue
		}
		sum, err := s.Summarize(name)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		for _, a := range sum.Attempts {
			if a.Seat != seat || !a.Open || (bead != "" && !a.StartedAt.After(started)) {
				continue
			}
			bead, started = name, a.StartedAt
		}
	}
	if bead == "" || len(skipped) > 0 {
		return "", false, errors.Join(skipped...)
	}
	ended, err := s.EndOpen(bead, seat, reason, at)
	return bead, ended, err
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

// Summarize reads bead's log and pairs its start and end events. Only an absent
// log is read as an empty one: a store that is there but cannot be read is an
// error, and damage found in a readable log is counted in Corrupt and Skipped.
func (s *Store) Summarize(bead string) (Summary, error) {
	var sum Summary
	if err := validateBead(bead); err != nil {
		return sum, err
	}
	path := s.path(bead)
	// Open first, then stat the descriptor: a stat before the open could describe
	// a different file than the one read if the path changes between the two.
	f, err := openLogNoBlock(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A dangling symlink is present in the store but resolves to nothing;
		// only a path with no entry at all is a bead that recorded nothing.
		if _, lerr := os.Lstat(path); lerr == nil {
			return sum, fmt.Errorf("reading polecat spans for %q: %w", bead, err)
		}
		return sum, nil
	case err != nil:
		return sum, fmt.Errorf("reading polecat spans for %q: %w", bead, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sum, fmt.Errorf("reading polecat spans for %q: %w", bead, err)
	}
	if !info.Mode().IsRegular() {
		return sum, fmt.Errorf("reading polecat spans for %q: %s is not a regular file", bead, path)
	}

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
	// The read keeps the rule logs were written under: an empty seat is corrupt.
	// The whitespace rule is for writes (validateSeat in append), so a log written
	// before it still reads as it did.
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
	path := s.path(bead)
	lk, err := lockLog(path + ".lock")
	if err != nil {
		return fmt.Errorf("locking polecat span log for %q: %w", bead, err)
	}
	defer lk.unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening polecat span log for %q: %w", bead, err)
	}
	// The lock, not the single write, is what keeps concurrent seats from
	// interleaving: O_APPEND promises whole-line atomicity only on some
	// platforms.
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

// validateSeat keeps the seat one non-blank token. It keys the pairing of
// starts and ends and is shown to operators, so a space or control character
// would let one actor read as two.
func validateSeat(seat, bead string) error {
	if seat == "" {
		return fmt.Errorf("polecat spans: empty seat for bead %q", bead)
	}
	if strings.IndexFunc(seat, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("polecat spans: seat %q for bead %q contains whitespace or a control character", seat, bead)
	}
	return nil
}

// requireSeat is the check a close applies: the seat is non-empty. A close names
// an attempt the log already holds, and a log written before the whitespace
// rule (validateSeat, applied to new starts only) can hold a seat with a space in
// it; refusing the close would leave that attempt open forever.
func requireSeat(seat, bead string) error {
	if seat == "" {
		return fmt.Errorf("polecat spans: empty seat for bead %q", bead)
	}
	return nil
}

// ValidBeadID reports whether bead is a single safe path component, the only
// shape a span log's file name can take.
func ValidBeadID(bead string) bool {
	return bead != "" && bead != "." && bead != ".." &&
		!strings.ContainsAny(bead, `/\`) && !strings.ContainsRune(bead, 0)
}

// validateBead keeps the bead id a single safe path component: a separator or
// ".." would write outside the store's directory.
func validateBead(bead string) error {
	if !ValidBeadID(bead) {
		return fmt.Errorf("polecat spans: invalid bead id %q", bead)
	}
	return nil
}

func validReason(reason Reason) bool {
	switch reason {
	case ReasonDone, ReasonDeferred, ReasonOverrun, ReasonEscalated, ReasonStopped, ReasonDead:
		return true
	}
	return false
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

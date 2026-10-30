package land

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat/spans"
)

// LandingsFile is a rig's append-only landing log: one JSON LandingRecord per
// line, written by the landing worker after the target's tip is read back.
// It is the record nothing reaps (MR wisps were reaped within a day, so no
// landing survived before D2) and the list the red-main owner and the
// branch-protection audit read "who landed what" from.
type LandingsFile struct {
	Path string
	// Logf, when set, receives one warning per set of unparsable lines a read
	// skipped. The reader that keeps this file for the daemon's life is the
	// one worth warning; a reader that builds its own per poll leaves it nil
	// rather than repeat the same warning every few seconds.
	Logf func(format string, args ...any)
	// Spans, when set, is the span store each appended record's polecat time
	// is read from; the landing worker sets the town's store and no other
	// caller writes. Nil means records carry no polecat time (gt-yxl22).
	Spans SpanSummarizer
	// FirstSeen, when set, answers when the host first saw a bead's submission
	// at a head: the first-sight record the landing worker writes from its own
	// clock before it lands anything. It is the end a landing trusts for a seat
	// that could not write its own; the seat's submission stamp may pull that
	// end earlier, never later (gt-8yk78).
	FirstSeen func(bead, head string) (time.Time, error)

	write func(*os.File, []byte) (int, error) // test seam: writes one record; nil means the file's own Write
	now   func() time.Time                    // test seam: the landing's clock; nil means time.Now

	// mu guards warned: the daemon hands this file to both the landing
	// worker and its post-landing run.
	mu     sync.Mutex
	warned string
}

// RigLandingsFile is <town>/.runtime/landings/<rig>.jsonl.
func RigLandingsFile(townRoot, rig string) (*LandingsFile, error) {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." || rig+".jsonl" == ReviewsFileName {
		return nil, fmt.Errorf("invalid rig name %q for a landings file", rig)
	}
	return &LandingsFile{
		Path:      filepath.Join(constants.TownRuntimePath(townRoot), "landings", rig+".jsonl"),
		FirstSeen: FirstSeenIn(SubmissionsSeenPath(townRoot)),
	}, nil
}

// SpanSummarizer is the part of *spans.Store a landing uses: one bead's polecat
// time, and the close of an attempt the seat left open (AppendSubmitted).
type SpanSummarizer interface {
	Summarize(bead string) (spans.Summary, error)
	EndOpen(bead, seat string, reason spans.Reason, at time.Time) (bool, error)
}

// Append writes rec as one line. The directory must be 0700-safe (owned by
// this user, not group- or world-writable) and the file must not be a symlink,
// so no other account can redirect or forge the log.
//
// The polecat time is Append's to write: it fills rec's three polecat fields
// from Spans when rec carries none, and a caller that set them itself keeps
// what it set rather than having the values replaced under it.
//
// A tail left torn (a crash or a short write stopped mid-record, so the file
// does not end in a newline) is repaired first, and a short write of rec is
// rolled back, so no append can glue its record onto a partial one.
func (f *LandingsFile) Append(rec LandingRecord) error {
	return f.AppendSubmitted(rec, "", time.Time{})
}

// AppendSubmitted is Append for a record whose seat and submission time the
// caller knows: it ends the attempt that seat left open, so the record carries
// the seat's seconds instead of a dash.
//
// A sandboxed seat cannot end its own attempt: its gt done writes the log of
// the sandbox's own town, which is deleted with the sandbox, while the sling's
// start is in this town's log. The host ends the attempt instead, at the time
// it first saw the submission, pulled earlier when the seat's own stamp says
// its turn ended sooner. A seat that already ended the attempt writes nothing
// here (gt-8yk78).
func (f *LandingsFile) AppendSubmitted(rec LandingRecord, seat string, submitted time.Time) error {
	if rec.PolecatSecs == 0 && len(rec.PolecatAttempts) == 0 && !rec.PolecatLowerBound {
		f.closeAttempt(rec.BeadID, rec.Head, seat, submitted, f.landedAt(rec))
		rec.PolecatSecs, rec.PolecatAttempts, rec.PolecatLowerBound = f.PolecatTime(rec.BeadID, time.Time{})
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding landing record: %w", err)
	}
	return appendJSONLine(f.Path, append(line, '\n'), f.write)
}

// landedAt is when this record landed: its own stamp, or the file's clock when
// the caller built the record without one.
func (f *LandingsFile) landedAt(rec LandingRecord) time.Time {
	if !rec.LandedAt.IsZero() {
		return rec.LandedAt
	}
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// closeAttempt ends seat's open attempt on beadID. The end is the earlier of the
// seat's submission stamp, the host's first sight of the head and capped, and
// the attempt stays open when the host never saw the submission (gt-8yk78).
func (f *LandingsFile) closeAttempt(beadID, head, seat string, submitted, capped time.Time) {
	// The end is the host's, not the seat's: the stamp is the seat's own word
	// for when its turn ended, and a seat must not be able to write time into a
	// landing it did not work, so the stamp may only pull the end earlier. With
	// no first-sight record the host has seen no submission at all and the
	// attempt stays open — no credit is better than the seat's word for it. A
	// read or write that fails is logged and the caller carries on: a landing
	// is never worth failing over the time, and an attempt left open is what
	// leaves the reading a lower bound (gt-yxl22).
	if f.Spans == nil || f.FirstSeen == nil || seat == "" || submitted.IsZero() || !spans.ValidBeadID(beadID) {
		return
	}
	seen, err := f.FirstSeen(beadID, head)
	if err != nil {
		if f.Logf != nil {
			f.Logf("landings file %s: no first-sight record to bound %s on %s: %v", f.Path, seat, beadID, err)
		}
		return
	}
	if seen.IsZero() {
		if f.Logf != nil {
			f.Logf("landings file %s: no first-sight record for %s, so %s's end is not written and its time stays a lower bound", f.Path, beadID, seat)
		}
		return
	}
	end := submitted
	if seen.Before(end) {
		end = seen
	}
	if capped.Before(end) {
		end = capped
	}
	if _, err := f.Spans.EndOpen(beadID, seat, spans.ReasonDone, end); err != nil {
		if f.Logf != nil {
			f.Logf("landings file %s: no end for %s on %s: %v", f.Path, seat, beadID, err)
		}
	}
}

// PolecatTime is a bead's polecat time: the time the bead's seats spent hooked
// on it, or nothing for a crew landing, whose bead has no seat to log, and for
// a bead whose log records nothing. A log that cannot be read is logged and its
// time left out, but the total is still a lower bound: the log was there, so
// there is time the reading cannot count. The landing is never worth failing
// over the time (gt-yxl22).
//
// asOf zero is the reading a landed record carries: an attempt still open adds
// nothing. A non-zero asOf is the reading for a landing still running: each open
// attempt counts up to asOf, and the total stays a lower bound while one is
// open. The Landings pane reads both, through the one reader.
func (f *LandingsFile) PolecatTime(bead string, asOf time.Time) (secs float64, attempts []PolecatAttempt, lowerBound bool) {
	// A record's bead id is not trusted: the store names one file per bead, so
	// an id that is not a single path component could only be refused. Check
	// before the id can reach a path at all (Summarize checks again).
	if f.Spans == nil || !spans.ValidBeadID(bead) {
		return 0, nil, false
	}
	sum, err := f.Spans.Summarize(bead)
	if err != nil {
		if f.Logf != nil {
			f.Logf("landings file %s: no polecat time for %s: %v; the record is written with a lower bound", f.Path, bead, err)
		}
		return 0, nil, true
	}
	if len(sum.Attempts) == 0 {
		return 0, nil, false
	}
	attempts = make([]PolecatAttempt, len(sum.Attempts))
	secs, lowerBound = sum.Secs, sum.LowerBound
	for i, a := range sum.Attempts {
		attempts[i] = PolecatAttempt{Seat: a.Seat, Secs: a.Secs, LowerBound: a.LowerBound}
		if !a.Open || asOf.IsZero() {
			continue
		}
		// A landing still running reads the open attempt up to asOf, and it is a
		// floor while the seat keeps working however little time has passed: the
		// reading the summary carries knows the attempt is open, not that this
		// reading counted it.
		lowerBound = true
		if asOf.After(a.StartedAt) {
			attempts[i].Secs = asOf.Sub(a.StartedAt).Seconds()
			secs += attempts[i].Secs
		}
	}
	return secs, attempts, lowerBound
}

// appendJSONLine appends one newline-terminated record to the log at path with
// the guarantees Append documents; write, when set, replaces the one write (a
// test seam). The landings file and the reviews file share it so neither can
// glue a record onto a torn one.
func appendJSONLine(path string, line []byte, write func(*os.File, []byte) (int, error)) error {
	dir := filepath.Dir(path)
	if err := ensureLandingsDir(dir); err != nil {
		return err
	}
	// O_RDWR, not O_WRONLY: repairing a torn tail reads the file's last byte,
	// and a rollback truncates it.
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|oNoFollow, 0o600)
	if err != nil {
		return fmt.Errorf("opening landings file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	size, err := fh.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("measuring landings file: %w", err)
	}
	if size > 0 {
		last := []byte{' '}
		if _, err := fh.ReadAt(last, size-1); err != nil {
			return fmt.Errorf("reading the last byte of the landings file: %w", err)
		}
		if last[0] != '\n' {
			if _, err := fh.Write([]byte{'\n'}); err != nil {
				return fmt.Errorf("repairing the landings file's torn tail: %w", err)
			}
		}
	}
	start, err := fh.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("measuring landings file: %w", err)
	}
	if err := appendRecord(fh, write, line, start, size); err != nil {
		return err
	}
	if err := fh.Sync(); err != nil {
		return fmt.Errorf("syncing landings file: %w", err)
	}
	return fh.Close()
}

// appendRecord writes b, which lands at start, and truncates the file back to
// good — the length it had before this Append — when the write comes up short
// or fails, so the partial record does not survive for the next append to
// glue onto. A writer that appended past b's short end since keeps its bytes:
// the readers skip a torn line, but nothing brings back a deleted record.
func appendRecord(fh *os.File, write func(*os.File, []byte) (int, error), b []byte, start, good int64) error {
	if write == nil {
		write = func(fh *os.File, b []byte) (int, error) { return fh.Write(b) }
	}
	n, err := write(fh, b)
	if err == nil && n == len(b) {
		return nil
	}
	if end, werr := fh.Seek(0, io.SeekEnd); werr == nil && end == start+int64(n) {
		if terr := fh.Truncate(good); terr != nil {
			return fmt.Errorf("rolling back a partial landing record: %w", terr)
		}
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	return fmt.Errorf("appending landing record: %w", err)
}

// Find returns the latest record for beadID landing head, if the file has one.
// Land uses it to finish a landing whose bead record was left incomplete.
func (f *LandingsFile) Find(beadID, head string) (LandingRecord, bool, error) {
	return f.latest(func(rec LandingRecord) bool { return rec.BeadID == beadID && rec.Head == head })
}

// LatestForBead returns the latest record for beadID whatever head it
// landed. The landing worker reads it before a landing, so a bead whose
// record was left incomplete is repaired rather than landed twice even when
// its branch has moved since.
func (f *LandingsFile) LatestForBead(beadID string) (LandingRecord, bool, error) {
	return f.latest(func(rec LandingRecord) bool { return rec.BeadID == beadID })
}

// Recent returns up to n of the file's last records, oldest first.
func (f *LandingsFile) Recent(n int) ([]LandingRecord, error) {
	var recs []LandingRecord
	_, _, err := f.latest(func(rec LandingRecord) bool {
		recs = append(recs, rec)
		if len(recs) > n {
			recs = recs[1:]
		}
		return false
	})
	return recs, err
}

// Since returns the records landed at or after t, oldest first. The
// attention queue's risk-path collector reads a window this way (gt-vsct7.4).
func (f *LandingsFile) Since(t time.Time) ([]LandingRecord, error) {
	var recs []LandingRecord
	_, _, err := f.latest(func(rec LandingRecord) bool {
		if !rec.LandedAt.Before(t) {
			recs = append(recs, rec)
		}
		return false
	})
	return recs, err
}

func (f *LandingsFile) latest(match func(LandingRecord) bool) (LandingRecord, bool, error) {
	fh, err := os.Open(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return LandingRecord{}, false, nil
	}
	if err != nil {
		return LandingRecord{}, false, fmt.Errorf("opening landings file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	var found LandingRecord
	ok := false
	skipped := 0
	first := ""
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec LandingRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			// One torn line is one crash wide; the records around it are
			// still the log, and internal/landings reads the same file that
			// way. Erroring here failed every landing on the rig (gt-jr90x).
			if skipped++; first == "" {
				first = string(raw)
			}
			continue
		}
		if match(rec) {
			found, ok = rec, true
		}
	}
	if err := sc.Err(); err != nil {
		return LandingRecord{}, false, fmt.Errorf("reading landings file %s: %w", f.Path, err)
	}
	f.warnSkipped(skipped, first)
	return found, ok, nil
}

// warnSkipped warns once about the lines a read could not parse. A torn line
// stays torn until the file is rewritten, and the landing worker reads this
// file every pass, so the same set of bad lines logs once; a set that changed
// warns again.
func (f *LandingsFile) warnSkipped(n int, first string) {
	if n == 0 {
		f.mu.Lock()
		f.warned = ""
		f.mu.Unlock()
		return
	}
	if f.Logf == nil {
		return
	}
	msg := fmt.Sprintf("%d unparsable line(s) skipped, starting with %.120q", n, first)
	f.mu.Lock()
	repeat := f.warned == msg
	f.warned = msg
	f.mu.Unlock()
	if repeat {
		return
	}
	f.Logf("landings file %s: %s; the records around them still read", f.Path, msg)
}

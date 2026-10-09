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

	write func(*os.File, []byte) (int, error) // test seam: writes one record; nil means the file's own Write

	// mu guards warned: the daemon hands this file to both the landing
	// worker and its post-landing run.
	mu     sync.Mutex
	warned string
}

// RigLandingsFile is <town>/.runtime/landings/<rig>.jsonl.
func RigLandingsFile(townRoot, rig string) (*LandingsFile, error) {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
		return nil, fmt.Errorf("invalid rig name %q for a landings file", rig)
	}
	return &LandingsFile{Path: filepath.Join(constants.TownRuntimePath(townRoot), "landings", rig+".jsonl")}, nil
}

// Append writes rec as one line. The directory must be 0700-safe (owned by
// this user, not group- or world-writable) and the file must not be a
// symlink, so no other account can redirect or forge the log.
//
// A tail left torn (a crash or a short write stopped mid-record, so the file
// does not end in a newline) is repaired first, and a short write of rec is
// rolled back, so no append can glue its record onto a partial one.
func (f *LandingsFile) Append(rec LandingRecord) error {
	dir := filepath.Dir(f.Path)
	if err := ensureLandingsDir(dir); err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding landing record: %w", err)
	}
	line = append(line, '\n')
	// O_RDWR, not O_WRONLY: repairing a torn tail reads the file's last byte,
	// and a rollback truncates it.
	fh, err := os.OpenFile(f.Path, os.O_RDWR|os.O_APPEND|os.O_CREATE|oNoFollow, 0o600)
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
	if err := f.appendRecord(fh, line, start, size); err != nil {
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
func (f *LandingsFile) appendRecord(fh *os.File, b []byte, start, good int64) error {
	n, err := f.writeTo(fh, b)
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

func (f *LandingsFile) writeTo(fh *os.File, b []byte) (int, error) {
	if f.write != nil {
		return f.write(fh, b)
	}
	return fh.Write(b)
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

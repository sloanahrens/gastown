package land

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

var ErrNoGateRunRoot = errors.New("land: the gate run log needs a town root")

func gateRunsPath(townRoot string) string {
	if townRoot == "" {
		return ""
	}
	return filepath.Join(townRoot, ".runtime", "metrics", "gate-runs.jsonl")
}

// AppendGateRun appends r as one JSON line; one append write keeps two writers from tearing a line.
func AppendGateRun(root string, r GateRun) error { return appendGateRun(osGateRunFS{}, root, r) }

type gateRunFS interface {
	MkdirAll(dir string, perm os.FileMode) error
	OpenAppend(path string) (io.WriteCloser, error)
	// Size is the file's length, 0 when it does not exist.
	Size(path string) (int64, error)
	// EndsInNewline is true when the file is missing or empty, or its last byte is a newline.
	EndsInNewline(path string) (bool, error)
	ReadFile(path string) ([]byte, error)
	Remove(path string) error
	// ReadDir lists the names of the files directly under dir.
	ReadDir(dir string) ([]string, error)
	Rename(from, to string) error
}
type osGateRunFS struct{}

func (osGateRunFS) Size(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
func (osGateRunFS) EndsInNewline(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path is under the town root
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error cannot lose a run
	// One error return for the stat and the read: either failing refuses the append.
	info, err := f.Stat()
	var last [1]byte
	if err == nil && info.Size() > 0 {
		_, err = f.ReadAt(last[:], info.Size()-1)
	}
	if err != nil {
		return false, err
	}
	return info.Size() == 0 || last[0] == '\n', nil
}
func (osGateRunFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) } //nolint:gosec // G304: the path is under the town root
func (osGateRunFS) Remove(path string) error             { return os.Remove(path) }
func (osGateRunFS) ReadDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, err
}
func (osGateRunFS) Rename(from, to string) error { return os.Rename(from, to) }

func (osGateRunFS) MkdirAll(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) }
func (osGateRunFS) OpenAppend(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}
func appendGateRun(fsys gateRunFS, townRoot string, r GateRun) error {
	return appendGateRunLimit(fsys, townRoot, r, gateRunRotateLimit)
}

// gateRunKey is a row's identity: the four fields a run is told apart by.
func gateRunKey(r GateRun) string {
	return r.TS.UTC().Format(time.RFC3339Nano) + "\x00" + r.Bead + "\x00" + r.SHA + "\x00" + r.RunID
}

// readMonthFile returns the decoded rows of every gzip member of a month file,
// and the identities of those rows; a month file that does not exist is empty.
func readMonthFile(fsys gateRunFS, month string) ([]byte, map[string]bool, error) {
	data, err := fsys.ReadFile(month)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("land: reading %s: %w", month, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("land: decoding %s: %w", month, err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		return nil, nil, fmt.Errorf("land: decoding %s: %w", month, err)
	}
	keys := map[string]bool{}
	for _, line := range bytes.Split(plain, []byte{'\n'}) {
		var r *GateRun
		if json.Unmarshal(line, &r) == nil && r != nil {
			keys[gateRunKey(*r)] = true
		}
	}
	return plain, keys, nil
}

// writeMonthFile replaces month with a single gzip member holding plain. The
// member is written to a temp file beside month and renamed over it, so a failed
// write leaves the month file as it was.
func writeMonthFile(fsys gateRunFS, month string, plain []byte) error {
	tmp := month + ".tmp"
	// OpenAppend adds to a file, so a partial temp file a failed write left
	// behind goes first.
	size, err := fsys.Size(tmp)
	if err != nil {
		return fmt.Errorf("land: sizing %s: %w", tmp, err)
	}
	if size > 0 {
		if err := fsys.Remove(tmp); err != nil {
			return fmt.Errorf("land: removing the partial %s: %w", tmp, err)
		}
	}
	f, err := fsys.OpenAppend(tmp)
	if err != nil {
		return fmt.Errorf("land: opening %s: %w", tmp, err)
	}
	zw := gzip.NewWriter(f)
	_, werr := zw.Write(plain)
	werr = errors.Join(werr, zw.Close())
	if werr == nil {
		// Only an *os.File can be synced; a writer without Sync has nothing to flush.
		if s, ok := f.(interface{ Sync() error }); ok {
			werr = s.Sync()
		}
	}
	werr = errors.Join(werr, f.Close())
	if werr != nil {
		return fmt.Errorf("land: writing %s: %w", tmp, werr)
	}
	if err := fsys.Rename(tmp, month); err != nil {
		return fmt.Errorf("land: renaming %s over %s: %w", tmp, month, err)
	}
	return nil
}

// gateRunRejectsFile holds the lines of the gate-run log that did not decode.
const gateRunRejectsFile = "gate-runs-rejects.jsonl"

// appendGateRunRejects appends the lines the rotation could not decode to path.
func appendGateRunRejects(fsys gateRunFS, path string, lines []byte) error {
	f, err := fsys.OpenAppend(path)
	if err != nil {
		return fmt.Errorf("land: opening %s: %w", path, err)
	}
	_, werr := f.Write(lines)
	if werr = errors.Join(werr, f.Close()); werr != nil {
		return fmt.Errorf("land: keeping the undecodable lines in %s: %w", path, werr)
	}
	return nil
}

// pruneGateRunMonths deletes the month files in dir that are past the retention
// and returns the names it deleted, sorted.
// It runs after a rotation has moved the rows, treats a month file as a whole
// (it never reads or counts rows) and keeps deleting after a delete fails, so one
// stuck file does not hold the rest; every failure is returned.
func pruneGateRunMonths(fsys gateRunFS, dir string, now time.Time) ([]string, error) {
	names, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("land: listing %s for the expired month files: %w", dir, err)
	}
	var failed error
	var deleted []string
	for _, name := range names {
		if gateRunMonthQuarantined(name, now) {
			failed = errors.Join(failed, fmt.Errorf("land: %s: month outside the retained years, kept for an operator", name))
			continue
		}
		if !gateRunMonthExpired(name, now, gateRunRetentionDays) {
			continue
		}
		if err := fsys.Remove(filepath.Join(dir, name)); err != nil {
			failed = errors.Join(failed, fmt.Errorf("land: deleting the expired %s: %w", name, err))
			continue
		}
		deleted = append(deleted, name)
	}
	slices.Sort(deleted)
	return deleted, failed
}

// rotateGateRuns moves the rows of the current log into one gzipped file per
// month beside it, a month file that exists being rewritten whole with its rows
// kept, and removes the current log only once every month file is written, so a
// failure leaves the rows where they were. It assumes one writer, the daemon's recorder.
// A line that does not parse is not carried over: ReadGateRuns skips it already.
func rotateGateRuns(fsys gateRunFS, path string) error {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return fmt.Errorf("land: reading %s to rotate it: %w", path, err)
	}
	var rows []GateRun
	var rejects []byte // every line that did not decode, as it was read, each with its newline
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var r *GateRun
		switch {
		case json.Unmarshal(line, &r) == nil && r != nil:
			rows = append(rows, *r)
		case len(bytes.TrimSpace(line)) > 0:
			rejects = append(append(rejects, line...), '\n')
		}
	}
	for _, group := range groupGateRunsByMonth(rows) {
		month := filepath.Join(filepath.Dir(path), group.File)
		// A rotation retried after a failed removal meets rows it already
		// archived: only the rows the month file lacks are written.
		archived, have, err := readMonthFile(fsys, month)
		if err != nil {
			return err
		}
		var batch bytes.Buffer
		for _, row := range group.Rows {
			if have[gateRunKey(row)] {
				continue
			}
			line, _ := json.Marshal(row) // a row that decoded encodes
			batch.Write(append(line, '\n'))
		}
		if batch.Len() == 0 {
			continue
		}
		// The month file is rewritten whole, never appended to: a member that a
		// failed write left half-written would make the whole file unreadable.
		if err := writeMonthFile(fsys, month, append(archived, batch.Bytes()...)); err != nil {
			return err
		}
	}
	if len(rejects) > 0 {
		// Kept, not destroyed with the log: a body that is a whole gate run with a
		// ts that has no zone is real data lost to a format slip (gt-a78yu0). A
		// failed write removes nothing; a retry archives the month files again
		// idempotently, and the rejects file is plain text that is never rotated.
		if err := appendGateRunRejects(fsys, filepath.Join(filepath.Dir(path), gateRunRejectsFile), rejects); err != nil {
			return err
		}
	}
	if err := fsys.Remove(path); err != nil {
		return fmt.Errorf("land: removing %s after rotating it: %w", path, err)
	}
	return nil
}

// gateRunMu serializes every append and rotation of the gate-run log: the
// daemon's landing workers, one per rig, all append to the one town-wide file,
// and an append that landed between a rotation's read and its removal of the log
// would be deleted with it (gt-6wfk0k).
var gateRunMu sync.Mutex

// appendGateRunLimit appends r, rotating the log first when it has reached limit.
// A rotation that fails is returned after the row is appended, never before, so
// the row is not lost.
func appendGateRunLimit(fsys gateRunFS, townRoot string, r GateRun, limit int64) error {
	return appendGateRunAt(fsys, townRoot, r, limit, time.Now(), log.Writer())
}

// appendGateRunAt is appendGateRunLimit with the retention clock and the writer
// of the deletion report parameters, so a test sets both; production passes the
// time of the append and the daemon's log writer (the standard logger's, which
// the daemon's gate run recorder logs through).
func appendGateRunAt(fsys gateRunFS, townRoot string, r GateRun, limit int64, now time.Time, report io.Writer) error {
	gateRunMu.Lock()
	defer gateRunMu.Unlock()
	path := gateRunsPath(townRoot)
	if path == "" {
		return ErrNoGateRunRoot
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("land: encoding the gate run for %s: %w", path, err)
	}
	line = append(line, '\n')
	if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("land: creating the directory for %s: %w", path, err)
	}
	var rotateErr error
	if size, err := fsys.Size(path); err != nil {
		rotateErr = fmt.Errorf("land: sizing %s: %w", path, err)
	} else if gateRunRotationDue(size, limit) {
		if rotateErr = rotateGateRuns(fsys, path); rotateErr == nil {
			var deleted []string
			deleted, rotateErr = pruneGateRunMonths(fsys, filepath.Dir(path), now)
			if len(deleted) > 0 {
				_, _ = fmt.Fprintf(report, "gate runs: deleted %d expired month file(s): %s\n", len(deleted), strings.Join(deleted, ", "))
			}
		}
	}
	// A short write can leave a line with no newline. The row then starts a line
	// of its own, in the same write, so it does not glue onto the fragment
	// (gt-7i2okn). This check runs after the rotation, which removes the log.
	endsNL, err := fsys.EndsInNewline(path)
	if err != nil {
		return errors.Join(fmt.Errorf("land: checking the end of %s: %w", path, err), rotateErr)
	}
	if !endsNL {
		line = append([]byte{'\n'}, line...)
	}
	f, err := fsys.OpenAppend(path)
	if err != nil {
		return errors.Join(fmt.Errorf("land: opening %s: %w", path, err), rotateErr)
	}
	n, writeErr := f.Write(line)
	closeErr := f.Close()
	if writeErr != nil {
		return errors.Join(fmt.Errorf("land: writing %s: %w", path, errors.Join(writeErr, closeErr)), rotateErr)
	}
	if n != len(line) {
		short := fmt.Errorf("short write (%d of %d bytes): %w", n, len(line), io.ErrShortWrite)
		return errors.Join(fmt.Errorf("land: writing %s: %w", path, errors.Join(short, closeErr)), rotateErr)
	}
	if closeErr != nil {
		return errors.Join(fmt.Errorf("land: closing %s: %w", path, closeErr), rotateErr)
	}
	return rotateErr
}
func ReadGateRuns(townRoot string) ([]GateRun, int, error) {
	return readGateRuns(osGateRunFS{}, townRoot)
}

// readGateRuns reads the log's bytes under gateRunMu, so it never sees the log
// between a rotation's removal of it and the new row's append, and parses them
// after the lock is released (gt-vaakuz).
func readGateRuns(fsys gateRunFS, townRoot string) ([]GateRun, int, error) {
	path := gateRunsPath(townRoot)
	if path == "" {
		return nil, 0, ErrNoGateRunRoot
	}
	gateRunMu.Lock()
	data, err := fsys.ReadFile(path)
	gateRunMu.Unlock()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("land: reading %s: %w", path, err)
	}
	var runs []GateRun
	unparsed := 0
	br := bufio.NewReaderSize(bytes.NewReader(data), 64*1024)
	for {
		raw, over, err := readGateRunLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("land: reading %s: %w", path, err)
		}
		// An over-long line is skipped and counted, as a torn one is: the rows
		// after it still read (gt-hykdcz).
		if over {
			unparsed++
			continue
		}
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		// A torn line is skipped and counted so the runs around it still read;
		// a JSON null decodes to a nil pointer without error, so it counts too.
		var r *GateRun
		if json.Unmarshal([]byte(line), &r) != nil || r == nil {
			unparsed++
			continue
		}
		runs = append(runs, *r)
	}
	return runs, unparsed, nil
}

// gateRunLineMax is the longest line, newline excluded, that ReadGateRuns
// parses. A longer line is skipped to its newline and counted as unparsed.
const gateRunLineMax = 1 << 20

// readGateRunLine returns the next line without its newline. A line over
// gateRunLineMax comes back with over set and no bytes, once the reader has
// read past its newline or the end of the file. At end of file it returns
// io.EOF, or the final line if the file does not end in a newline.
func readGateRunLine(br *bufio.Reader) (line []byte, over bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		body := chunk
		if rerr == nil {
			body = chunk[:len(chunk)-1]
		}
		if !over {
			if len(line)+len(body) > gateRunLineMax {
				over, line = true, nil
			} else {
				line = append(line, body...)
			}
		}
		switch {
		case rerr == nil:
			return line, over, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			// The line runs past the read buffer; keep reading it.
		case errors.Is(rerr, io.EOF):
			if !over && len(line) == 0 && len(chunk) == 0 {
				return nil, false, io.EOF
			}
			return line, over, nil
		default:
			return nil, false, rerr
		}
	}
}

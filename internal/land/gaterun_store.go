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
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	ReadFile(path string) ([]byte, error)
	Remove(path string) error
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
func (osGateRunFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) } //nolint:gosec // G304: the path is under the town root
func (osGateRunFS) Remove(path string) error             { return os.Remove(path) }

func (osGateRunFS) MkdirAll(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) }
func (osGateRunFS) OpenAppend(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}
func appendGateRun(fsys gateRunFS, townRoot string, r GateRun) error {
	return appendGateRunLimit(fsys, townRoot, r, gateRunRotateLimit)
}

// rotateGateRuns moves the rows of the current log into one gzipped file per
// month beside it, a month file that exists gaining a further gzip member, and
// removes the current log only once every month file is written, so a failure
// leaves the rows where they were. It assumes one writer, the daemon's recorder.
// A line that does not parse is not carried over: ReadGateRuns skips it already.
func rotateGateRuns(fsys gateRunFS, path string) error {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return fmt.Errorf("land: reading %s to rotate it: %w", path, err)
	}
	var rows []GateRun
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var r *GateRun
		if json.Unmarshal(line, &r) == nil && r != nil {
			rows = append(rows, *r)
		}
	}
	for _, group := range groupGateRunsByMonth(rows) {
		month := filepath.Join(filepath.Dir(path), group.File)
		f, err := fsys.OpenAppend(month)
		if err != nil {
			return fmt.Errorf("land: opening %s: %w", month, err)
		}
		var batch bytes.Buffer
		for _, row := range group.Rows {
			line, _ := json.Marshal(row) // a row that decoded encodes
			batch.Write(append(line, '\n'))
		}
		zw := gzip.NewWriter(f)
		_, werr := zw.Write(batch.Bytes())
		werr = errors.Join(werr, zw.Close(), f.Close())
		if werr != nil {
			return fmt.Errorf("land: writing %s: %w", month, werr)
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
		rotateErr = rotateGateRuns(fsys, path)
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
	path := gateRunsPath(townRoot)
	if path == "" {
		return nil, 0, ErrNoGateRunRoot
	}
	f, err := os.Open(path) //nolint:gosec // G304: the path is under the town root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("land: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only: a close error cannot lose a run
	var runs []GateRun
	unparsed := 0
	br := bufio.NewReaderSize(f, 64*1024)
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

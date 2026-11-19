package land

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
}
type osGateRunFS struct{}

func (osGateRunFS) MkdirAll(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) }
func (osGateRunFS) OpenAppend(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}
func appendGateRun(fsys gateRunFS, townRoot string, r GateRun) error {
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
	f, err := fsys.OpenAppend(path)
	if err != nil {
		return fmt.Errorf("land: opening %s: %w", path, err)
	}
	n, writeErr := f.Write(line)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("land: writing %s: %w", path, errors.Join(writeErr, closeErr))
	}
	if n != len(line) {
		short := fmt.Errorf("short write (%d of %d bytes): %w", n, len(line), io.ErrShortWrite)
		return fmt.Errorf("land: writing %s: %w", path, errors.Join(short, closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("land: closing %s: %w", path, closeErr)
	}
	return nil
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
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
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("land: reading %s: %w", path, err)
	}
	return runs, unparsed, nil
}

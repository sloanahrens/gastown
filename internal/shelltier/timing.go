package shelltier

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// TimingFiles appends one line to the timing log at path.
type TimingFiles interface {
	// Append adds line to the log at path, creating the file and its directory
	// when they do not exist.
	Append(path string, line []byte) error
}

// TimingPath is <townRoot>/.runtime/tiertiming/shell.jsonl: the log each
// script's wall time is appended to, one JSON object per script. An empty town
// root has no log at all, so a caller that lost the town writes nothing instead
// of writing under the filesystem root.
func TimingPath(townRoot string) string {
	if townRoot == "" {
		return ""
	}
	return filepath.Join(constants.TownRuntimePath(townRoot), "tiertiming", "shell.jsonl")
}

// timingRecord is one script's line in the timing log. At is the run's clock at
// the script's end, in UTC at whole seconds, so the next ranking can order the
// lines by when they were measured.
type timingRecord struct {
	Script  string  `json:"script"`
	Seconds float64 `json:"seconds"`
	Exit    int     `json:"exit"`
	At      string  `json:"at"`
}

// recordTiming appends e's line, as out ran it, to cfg's timing log. It reports
// nothing to Run: the timing log is a measurement of the run, never part of its
// verdict, so a line written, lost or refused changes no result and no exit
// code. A line that cannot be written is said on cfg.Err, or on the process's
// error stream when cfg.Err is nil, so no failure is dropped in silence.
func recordTiming(cfg Config, e Entry, out outcome) {
	if cfg.Timing == nil || cfg.TimingPath == "" {
		return
	}
	line, err := timingLine(timingRecord{
		Script:  e.Path,
		Seconds: out.ended.Sub(out.began).Seconds(),
		Exit:    out.exit,
		At:      out.ended.UTC().Format(time.RFC3339),
	})
	if err == nil {
		err = cfg.Timing.Append(cfg.TimingPath, line)
	}
	if err != nil {
		fmt.Fprintf(errorStream(cfg.Err), "shelltier: record %s's wall time in %s: %v\n", e.Path, cfg.TimingPath, err)
	}
}

// errorStream is the stream a timing failure is said on: w when it is set, the
// process's error stream when it is not.
func errorStream(w io.Writer) io.Writer {
	if w == nil {
		return os.Stderr
	}
	return w
}

// timingLine renders rec as one line, newline included.
func timingLine(rec timingRecord) ([]byte, error) {
	line, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encode the line for %s: %w", rec.Script, err)
	}
	return append(line, '\n'), nil
}

// OSTimingFiles is the timing log on the local filesystem. Its zero value
// writes through the os package; mkdirAll and openFile are the seam a test
// drives, so no unit test touches the disk.
type OSTimingFiles struct {
	mkdirAll func(path string, perm os.FileMode) error
	openFile func(path string, flag int, perm os.FileMode) (appendTarget, error)
}

// Append adds line with one write to a file opened for appending, so two
// runners writing to the same log add whole lines rather than interleaving part
// of a line each.
func (o OSTimingFiles) Append(path string, line []byte) error {
	mkdirAll := o.mkdirAll
	if mkdirAll == nil {
		mkdirAll = os.MkdirAll
	}
	openFile := o.openFile
	if openFile == nil {
		openFile = func(path string, flag int, perm os.FileMode) (appendTarget, error) {
			return os.OpenFile(path, flag, perm)
		}
	}
	if err := mkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("make the log's directory: %w", err)
	}
	fh, err := openFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	return appendLine(fh, path, line)
}

// appendTarget is the open log appendLine writes one line to.
type appendTarget interface {
	Write(p []byte) (int, error)
	Close() error
}

// appendLine writes line to w and closes it, reporting a failed write or close.
func appendLine(w appendTarget, path string, line []byte) error {
	_, writeErr := w.Write(line)
	closeErr := w.Close()
	if writeErr != nil {
		return fmt.Errorf("write %s: %w", path, errors.Join(writeErr, closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}
	return nil
}

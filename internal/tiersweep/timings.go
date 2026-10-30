package tiersweep

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// TimingsName is the timings file a run writes into its log directory: one
// JSON line per package the run started, in run order, so a sweep's minutes can
// be read per package without parsing its logs.
const TimingsName = "timings.jsonl"

// timingsGoesOn ends every timings failure line: the run's verdict is the
// tiers', not the instrument's.
const timingsGoesOn = "the sweep goes on"

// Timing is one package's line in timings.jsonl. WallMS is the time the
// package's command took, Exit the code it returned, and Start the clock
// reading taken before it started, in RFC 3339 with nanoseconds.
type Timing struct {
	Tier    string `json:"tier"`
	Package string `json:"package"`
	WallMS  int64  `json:"wall_ms"`
	Exit    int    `json:"exit"`
	Start   string `json:"start"`
}

// TimingsKey is the key a timing is held under: the tier and the package name
// together, since integration and race both run ./internal/cmd.
func TimingsKey(tier, name string) string { return tier + " " + name }

// ParseWalls reads timings lines, in timings.jsonl's format, into the wall time
// each tier and package took, keyed by TimingsKey. A line that is not a timing
// is dropped: the file is an instrument, and a torn line must not reorder a run.
func ParseWalls(data []byte) map[string]int64 {
	walls := make(map[string]int64)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var tm Timing
		if err := json.Unmarshal([]byte(line), &tm); err != nil {
			continue
		}
		walls[TimingsKey(tm.Tier, tm.Package)] = tm.WallMS
	}
	return walls
}

// Timings is one run's timings file and the lines it holds. A file that cannot
// be created or written never fails the run: the problem is named on stderr
// once, since the file is dropped after a failure, and no tier's verdict
// changes.
type Timings struct {
	// path is the run's own file, under the log directory.
	path string
	// out is the open file; nil once it could not be opened or written.
	out *os.File
	// stderr receives the line a failure prints.
	stderr io.Writer
	// lines holds every line this run wrote, in run order, so the persistent
	// file (TIER_SWEEP_TIMINGS) can be replaced at the end.
	lines []byte
}

// OpenTimings creates the run's timings file at path, replacing whatever an
// earlier run left there. A file that cannot be created returns a Timings that
// writes nothing and has already named the file on stderr.
func OpenTimings(path string, stderr io.Writer) *Timings {
	t := &Timings{path: path, stderr: stderr}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.report(err)
		return t
	}
	t.out = f
	return t
}

// Add records one package's timing and writes its line. A package the run did
// not start records nothing. A write that fails drops the file, so the failure
// is named once rather than once per package, and the line is still kept for
// the persistent file.
func (t *Timings) Add(tm Timing) {
	line, err := json.Marshal(tm)
	if err != nil {
		t.report(err)
		return
	}
	line = append(line, '\n')
	t.lines = append(t.lines, line...)
	if t.out == nil {
		return
	}
	if _, err := t.out.Write(line); err != nil {
		t.out.Close()
		t.out = nil
		t.report(err)
	}
}

// Bytes is every line this run wrote, in run order.
func (t *Timings) Bytes() []byte { return t.lines }

// Close closes the run's file. A close that fails is named like any other
// timings failure and changes no verdict.
func (t *Timings) Close() {
	if t.out == nil {
		return
	}
	f := t.out
	t.out = nil
	if err := f.Close(); err != nil {
		t.report(err)
	}
}

// Replace replaces path with this run's lines: a temp file beside it, then a
// rename over it, so a reader sees the earlier run's whole file or this one's,
// never a torn file. A failed replace is one stderr line and changes no
// verdict: the next run can still read what it replaced.
func (t *Timings) Replace(path string, host Host) {
	tmp := path + ".tmp"
	if err := host.WriteFile(tmp, t.lines, 0o644); err != nil {
		t.reportAt(path, "write", err)
		return
	}
	if err := host.Rename(tmp, path); err != nil {
		t.reportAt(path, "replace", err)
	}
}

// report names a failure of the run's own file on stderr. It is reached at most
// once per kind: an open, a write that dropped the file, a close.
func (t *Timings) report(err error) {
	t.reportAt(t.path, "", err)
}

// reportAt names a failure of one timings file on stderr; verb says what was
// being done to it, and is empty for the run's own file. No verdict changes.
func (t *Timings) reportAt(path, verb string, err error) {
	if t.stderr == nil {
		return
	}
	if verb != "" {
		verb += " "
	}
	fmt.Fprintf(t.stderr, "%s: timings: %s%s: %v (%s)\n", Name, verb, path, err, timingsGoesOn)
}

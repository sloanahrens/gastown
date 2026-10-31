package tiersweep

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// reading taken before it started, in RFC 3339 with nanoseconds. Kind is empty
// on a package line and names the line's sort on any other, so a reader that
// wants walls cannot mistake a test line for one (gt-juo5e).
type Timing struct {
	Kind    string `json:"kind,omitempty"`
	Tier    string `json:"tier"`
	Package string `json:"package"`
	WallMS  int64  `json:"wall_ms"`
	Exit    int    `json:"exit"`
	Start   string `json:"start"`
	// Shard is "i/K" on a shard's line, the shard's place in its package (gt-rw118).
	Shard string `json:"shard,omitempty"`
}

// kindTest is the kind of a slow test's line in timings.jsonl.
const kindTest = "test"

// kindShard is the kind of a shard's line: one per shard of a package run as
// shards, so the package has one line for each of its K shards (gt-rw118).
const kindShard = "shard"

// TestWallKey is the key a listed test's previous wall is held under: its
// package's TimingsKey and the test's name, which a shard partition reads.
func TestWallKey(tier, name, test string) string { return TimingsKey(tier, name) + " " + test }

// TestTiming is one slow test's line in timings.jsonl: the package the sweep
// ran, the test's own name and how long it took. It names no wall time, since
// the test's time is inside the package's (gt-juo5e).
type TestTiming struct {
	Kind      string `json:"kind"`
	Tier      string `json:"tier"`
	Package   string `json:"package"`
	Test      string `json:"test"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// TimingsKey is the key a timing is held under: the tier and the package name
// together, since integration and race both run ./internal/cmd.
func TimingsKey(tier, name string) string { return tier + " " + name }

// ParseWalls reads timings lines, in timings.jsonl's format, into the wall time
// each tier and package took, keyed by TimingsKey. A package run as shards takes
// the widest of its shards' walls, since its shards ran beside each other. A
// line that is not a timing is dropped: the file is an instrument, and a torn
// line must not reorder a run.
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
		key := TimingsKey(tm.Tier, tm.Package)
		switch tm.Kind {
		case "":
			walls[key] = tm.WallMS
		case kindShard:
			if cur, ok := walls[key]; !ok || tm.WallMS > cur {
				walls[key] = tm.WallMS
			}
		default:
			// A slow test's line names its package too, and its elapsed time is
			// not the package's wall (gt-juo5e).
		}
	}
	return walls
}

// ParseTestWalls reads the slow-test lines of timings.jsonl into each listed
// test's wall in seconds, keyed by TestWallKey. A shard partition uses them to
// balance a package's shards; a test below the slow threshold has no line and
// takes the partition's default (gt-rw118).
func ParseTestWalls(data []byte) map[string]float64 {
	walls := make(map[string]float64)
	for _, line := range strings.Split(string(data), "\n") {
		var tt TestTiming
		if json.Unmarshal([]byte(line), &tt) != nil || tt.Kind != kindTest {
			continue
		}
		walls[TestWallKey(tt.Tier, tt.Package, tt.Test)] = float64(tt.ElapsedMS) / 1000
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
	// host is the filesystem the file is created, appended to and replaced
	// through, so a run's timings never reach past the host it was given.
	host Host
	// open is true while the file takes lines: false once it could not be
	// created or written, and after Close.
	open bool
	// stderr receives the line a failure prints.
	stderr io.Writer
	// lines holds every line this run wrote, in run order, so the persistent
	// file (TIER_SWEEP_TIMINGS) can be replaced at the end.
	lines []byte
	// random is the entropy a replace's temp name is drawn from; OpenTimings
	// sets it to crypto/rand, and a test swaps in a reader that fails.
	random io.Reader
}

// OpenTimings creates the run's timings file at path through host, replacing
// whatever an earlier run left there. A file that cannot be created returns a
// Timings that writes nothing and has already named the file on stderr.
func OpenTimings(path string, host Host, stderr io.Writer) *Timings {
	if host.WriteFile == nil {
		host.WriteFile = os.WriteFile
	}
	if host.AppendFile == nil {
		host.AppendFile = appendToFile
	}
	t := &Timings{path: path, host: host, stderr: stderr, random: rand.Reader}
	if err := host.WriteFile(path, nil, 0o644); err != nil {
		t.report(err)
		return t
	}
	t.open = true
	return t
}

// Add records one package's timing and writes its line. A package the run did
// not start records nothing. A write that fails drops the file, so the failure
// is named once rather than once per package, and the line is still kept for
// the persistent file.
func (t *Timings) Add(tm Timing) { t.write(tm) }

// AddTest records one slow test's line beside the package lines, in the order
// the run's packages finished, so a timings file names the tests that carry a
// package's minutes (gt-juo5e). A write that fails is Add's failure.
func (t *Timings) AddTest(tt TestTiming) { t.write(tt) }

// write appends one line in timings.jsonl's format.
func (t *Timings) write(v any) {
	line, err := json.Marshal(v)
	if err != nil {
		t.report(err)
		return
	}
	line = append(line, '\n')
	t.lines = append(t.lines, line...)
	if !t.open {
		return
	}
	if err := t.host.AppendFile(t.path, line); err != nil {
		t.open = false
		t.report(err)
	}
}

// Bytes is every line this run wrote, in run order.
func (t *Timings) Bytes() []byte { return t.lines }

// Close ends the run's writes to its file: a line added after it is kept for
// the persistent file and written nowhere. Each line reaches the file as it is
// added, so there is no handle left to close and Close cannot fail.
func (t *Timings) Close() { t.open = false }

// Replace replaces path with this run's lines: a temp file beside it, then a
// rename over it, so a reader sees the earlier run's whole file or this one's,
// never a torn file. The temp name is random, so two runs replacing one file
// never share a temp file, and a temp file a failed replace leaves is removed.
// A failed replace is one stderr line and changes no verdict: the next run can
// still read what it replaced.
func (t *Timings) Replace(path string, host Host) {
	tmp, err := tempBeside(path, t.random)
	if err != nil {
		t.reportAt(path, "write", err)
		return
	}
	if err := host.WriteFile(tmp, t.lines, 0o644); err != nil {
		t.reportAt(path, "write", err)
		t.removeTemp(tmp, host)
		return
	}
	if err := host.Rename(tmp, path); err != nil {
		t.reportAt(path, "replace", err)
		t.removeTemp(tmp, host)
	}
}

// tempBeside names a temp file beside path that no other run is using: path, a
// random suffix drawn from random, and .tmp.
func tempBeside(path string, random io.Reader) (string, error) {
	var b [8]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%x.tmp", path, b[:]), nil
}

// removeTemp deletes the temp file a failed replace left behind. A file that is
// not there is nothing to remove; a host with no Remove keeps it, named once.
func (t *Timings) removeTemp(tmp string, host Host) {
	if host.Remove == nil {
		t.reportAt(tmp, "remove", errors.New("skipped, host has no Remove"))
		return
	}
	if err := host.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.reportAt(tmp, "remove", err)
	}
}

// report names a failure of the run's own file on stderr. It is reached at most
// once per run: an open, or the write that drops the file.
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

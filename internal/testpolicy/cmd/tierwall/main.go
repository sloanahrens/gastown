// Command tierwall reads a `go test -json` stream and reports the integration
// tier's wall: one line per package, slowest first, then the tier's own wall
// against a bar.
//
//	make test-integration-wall            # the report on a real tier run
//	go test -json ./... | tierwall        # ad hoc
//
// make test-integration-wall feeds both of that target's go test invocations
// into one run, so the tier line spans them.
//
// Failing tests keep their output and passing ones do not, so a red tier stays
// readable. The exit code is go test's verdict: 1 when anything failed, 0
// otherwise. Wall time depends on host load, so nothing fails on it by
// default (gt-z7qtk); -max-wall turns the tier's wall into a failure on
// request.
//
// -jsonl <path> -run <id> also appends one line per package to a series, so
// "where does the gate's time go" can be read over weeks rather than from one
// run's report (gt-u0up0). The report and the exit code do not change; a
// series the tool cannot write is exit 2, never a silent success.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxLine is the longest stdin line read: one event, which carries a whole
// line of test output, so it can be long.
const maxLine = 1 << 20

// event is the part of a go test -json event this report reads. A package
// that does not build reports through ImportPath and FailedBuild instead of
// Package, and its compiler errors arrive as build-output events.
type event struct {
	Time        time.Time
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	ImportPath  string
	FailedBuild string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, host{now: time.Now, load1: hostLoad1}))
}

// host is what run reads outside the stream: the clock a stream with no
// timestamps is stamped from, the load a series line carries, and the file the
// series appends to. main wires the real ones; a test injects them, so no test
// reads the host and every failure path the series can hit has a case.
type host struct {
	now   func() time.Time
	load1 func() (float64, bool)
	// open appends to a path; nil means the real file.
	open func(path string) (io.WriteCloser, error)
}

// run reads a go test -json stream from stdin and writes the report to
// stdout. The exit code is 1 when any package or test failed, else 3 when
// -max-wall is set and the tier is over it, else 0; 2 means the runner
// itself failed, a series it could not write included.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, h host) int {
	fs := flag.NewFlagSet("tierwall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bar := fs.Duration("bar", 90*time.Second, "the wall the tier line is judged against (OK or OVER); it fails nothing")
	maxWall := fs.Duration("max-wall", 0, "exit 3 when the tier is over this wall; 0 turns the check off")
	jsonl := fs.String("jsonl", "", "append one JSON line per package to this file (needs -run)")
	runID := fs.String("run", "", "the run id each -jsonl line carries")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *jsonl != "" && *runID == "" {
		fmt.Fprintln(stderr, "tierwall: -jsonl needs -run <id>: a series line with no run cannot be placed")
		return 2
	}

	r := newReport(stdout)
	if err := r.read(stdin); err != nil {
		fmt.Fprintln(stderr, "tierwall:", err)
		return 2
	}
	var seriesErr error
	if *jsonl != "" {
		data, err := r.series(*runID, h)
		switch {
		case err != nil:
			seriesErr = err
		case len(data) == 0:
			fmt.Fprintf(stderr, "tierwall: no package events in the stream; nothing appended to %s\n", *jsonl)
		default:
			seriesErr = appendLines(*jsonl, data, h.open)
		}
	}
	reportErr := r.print(*bar)
	failedRunner := false
	if reportErr != nil {
		fmt.Fprintln(stderr, "tierwall:", reportErr)
		failedRunner = true
	}
	if seriesErr != nil {
		fmt.Fprintln(stderr, "tierwall:", seriesErr)
		failedRunner = true
	}
	if failedRunner {
		return 2
	}
	switch {
	case r.failed:
		return 1
	case *maxWall > 0 && r.tier() > *maxWall:
		return 3
	default:
		return 0
	}
}

// report holds what the stream is read into: the verdicts seen so far, the
// elapsed time each package reported, and the output whose verdict has not
// arrived. go test -json emits a test's lines before it says whether that
// test passed, so output waits in a bucket until its verdict closes it.
type report struct {
	out io.Writer
	err error

	failed bool
	first  time.Time
	last   time.Time

	tests   map[string][]string // output held per running test, keyed by testKey
	pkgOut  map[string][]string // package-level output held until the package reports
	builds  map[string][]string // build-output held per import path
	elapsed map[string]float64  // each package's pass/fail Elapsed, in seconds
	results map[string]string   // each package's verdict: pass, fail or skip
}

func newReport(out io.Writer) *report {
	return &report{
		out:     out,
		tests:   map[string][]string{},
		pkgOut:  map[string][]string{},
		builds:  map[string][]string{},
		elapsed: map[string]float64{},
		results: map[string]string{},
	}
}

// write remembers the first write error, so a stdout that went away stops the
// report instead of losing the rest of it silently.
func (r *report) write(s string) {
	if r.err != nil {
		return
	}
	_, r.err = io.WriteString(r.out, s)
}

func (r *report) read(in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// Not a go test event: a runner that prints its own line, or a
			// diagnostic go wrote to stdout. Pass it through — dropping a
			// line is how a red run loses the reason it was red.
			r.write(line + "\n")
			continue
		}
		r.handle(e)
	}
	return sc.Err()
}

func (r *report) handle(e event) {
	if !e.Time.IsZero() {
		if r.first.IsZero() {
			r.first = e.Time
		}
		r.last = e.Time
	}
	switch e.Action {
	case "build-output":
		r.builds[e.ImportPath] = append(r.builds[e.ImportPath], e.Output)
	case "output":
		switch {
		case e.Package == "":
			r.builds[e.ImportPath] = append(r.builds[e.ImportPath], e.Output)
		case e.Test == "":
			r.pkgOut[e.Package] = append(r.pkgOut[e.Package], e.Output)
		default:
			key := testKey(e.Package, e.Test)
			r.tests[key] = append(r.tests[key], e.Output)
		}
	case "pass", "fail", "skip":
		r.verdict(e)
	}
}

// verdict closes the bucket that a package's or a test's event ends, printing
// it when the verdict is a failure.
func (r *report) verdict(e event) {
	if e.Test != "" {
		key := testKey(e.Package, e.Test)
		lines := r.tests[key]
		delete(r.tests, key)
		if e.Action == "fail" {
			r.failed = true
			r.printLines(lines)
		}
		return
	}
	lines := r.pkgOut[e.Package]
	delete(r.pkgOut, e.Package)
	r.results[e.Package] = e.Action
	if e.Action == "skip" {
		// A package with no test files ran nothing and earns no line in the
		// report. It is still a package the run covered, so the series keeps
		// it, at its zero wall.
		return
	}
	r.elapsed[e.Package] = e.Elapsed
	if e.Action == "fail" {
		r.failed = true
		r.printLines(lines)
		r.printLines(r.builds[e.FailedBuild])
	}
	delete(r.builds, e.FailedBuild)
}

// print writes the per-package lines, slowest first, then the tier line. Ties
// go by package name, so one stream always prints one report.
func (r *report) print(bar time.Duration) error {
	pkgs := make([]string, 0, len(r.elapsed))
	for pkg := range r.elapsed {
		pkgs = append(pkgs, pkg)
	}
	sort.Slice(pkgs, func(i, j int) bool {
		if a, b := r.elapsed[pkgs[i]], r.elapsed[pkgs[j]]; a != b {
			return a > b
		}
		return pkgs[i] < pkgs[j]
	})
	for _, pkg := range pkgs {
		r.write(fmt.Sprintf("tierwall: %s %s\n", pkg, secs(seconds(r.elapsed[pkg]))))
	}
	verdict := "OK"
	if r.tier() > bar {
		verdict = "OVER"
	}
	r.write(fmt.Sprintf("tierwall: tier %s bar %s %s\n", secs(r.tier()), secs(bar), verdict))
	return r.err
}

// tier is the wall from the first event read to the last, which spans every
// go test invocation that fed this stream.
func (r *report) tier() time.Duration {
	if r.first.IsZero() || r.last.IsZero() {
		return 0
	}
	return r.last.Sub(r.first)
}

// wallLine is one package's wall in one run: the JSON object -jsonl appends.
// load1 is left out when the host reports none, so a line never carries a load
// it did not read.
type wallLine struct {
	TS     time.Time `json:"ts"`
	Run    string    `json:"run"`
	Pkg    string    `json:"pkg"`
	WallS  float64   `json:"wall_s"`
	Result string    `json:"result"`
	Load1  *float64  `json:"load1,omitempty"`
}

// series renders one line per package the stream reported a verdict for,
// sorted by package so one stream always renders one file. ts is the run's own
// start, the stream's first event; a stream with no timestamps is stamped from
// the clock. The load is read once for the run, and left out when the host
// reports none.
func (r *report) series(runID string, h host) ([]byte, error) {
	pkgs := make([]string, 0, len(r.results))
	for pkg := range r.results {
		pkgs = append(pkgs, pkg)
	}
	if len(pkgs) == 0 {
		return nil, nil
	}
	sort.Strings(pkgs)
	ts := r.first
	if ts.IsZero() {
		ts = h.now()
	}
	var load *float64
	if v, ok := h.load1(); ok {
		load = &v
	}
	var buf bytes.Buffer
	for _, pkg := range pkgs {
		line, err := json.Marshal(wallLine{
			TS:     ts,
			Run:    runID,
			Pkg:    pkg,
			WallS:  r.elapsed[pkg],
			Result: r.results[pkg],
			Load1:  load,
		})
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// appendLines appends data to path in one write, opened O_APPEND so two runs
// writing at once cannot interleave their lines. The parent directory is
// created first: the town's runtime directory need not exist before the first
// run records. open names the file's opener; nil means the real appending
// file.
func appendLines(path string, data []byte, open func(string) (io.WriteCloser, error)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if open == nil {
		open = openAppend
	}
	f, err := open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("%s: %w", path, werr)
	}
	return nil
}

// openAppend opens path for appending, creating it.
func openAppend(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
}

// hostLoad1 reads the host's 1-minute load average, the number a slow wall is
// read against. It mirrors internal/land's reader rather than importing that
// package: land carries the whole landing path, and this tool is built inside
// the gate it measures.
func hostLoad1() (float64, bool) {
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		var load float64
		if _, err := fmt.Sscanf(string(data), "%f", &load); err == nil {
			return load, true
		}
	}
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, false
	}
	// macOS prints "{ 1.23 4.56 7.89 }".
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{ }"))
	if len(fields) == 0 {
		return 0, false
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return load, true
}

func (r *report) printLines(lines []string) {
	for _, line := range lines {
		r.write(line)
	}
}

// testKey keeps one package's TestFoo out of another's bucket. NUL cannot
// appear in either half.
func testKey(pkg, test string) string {
	return pkg + "\x00" + test
}

// seconds reads a go test event's Elapsed, which is in seconds, as a
// Duration.
func seconds(elapsed float64) time.Duration {
	return time.Duration(elapsed * float64(time.Second))
}

// secs renders a wall in seconds at the tenth of a second this report is read
// at: 90s, 203.4s, 0.8s.
func secs(d time.Duration) string {
	s := strconv.FormatFloat(d.Round(100*time.Millisecond).Seconds(), 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + "s"
}

// Command tierwall reads a `go test -json` stream and reports the integration
// tier's wall: one line per package, slowest first, then the tier's own wall
// against a bar.
//
//	make test-integration-wall            # the report on a real tier run
//	go test -json ./... | tierwall        # ad hoc
//
// It exists because scripts/tier-sweep.sh runs one package at a time and logs
// only go test's ok line, so the tier's real wall under make test-integration
// (every package at once, bounded by the go test -p cap) was never measured
// (gt-ik4a1.4.5). make test-integration-wall feeds both of that target's go
// test invocations into one run, so the tier line spans them.
//
// Failing tests keep their output and passing ones do not, so a red tier stays
// readable. The exit code is go test's verdict: 1 when anything failed, 0
// otherwise. Wall time depends on host load, so nothing fails on it by
// default (gt-z7qtk); -max-wall turns the tier's wall into a failure on
// request.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run reads a go test -json stream from stdin and writes the report to
// stdout. The exit code is 1 when any package or test failed, else 3 when
// -max-wall is set and the tier is over it, else 0; 2 means the runner
// itself failed.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tierwall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bar := fs.Duration("bar", 90*time.Second, "the wall the tier line is judged against (OK or OVER); it fails nothing")
	maxWall := fs.Duration("max-wall", 0, "exit 3 when the tier is over this wall; 0 turns the check off")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	r := newReport(stdout)
	if err := r.read(stdin); err != nil {
		fmt.Fprintln(stderr, "tierwall:", err)
		return 2
	}
	if err := r.print(*bar); err != nil {
		fmt.Fprintln(stderr, "tierwall:", err)
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
}

func newReport(out io.Writer) *report {
	return &report{
		out:     out,
		tests:   map[string][]string{},
		pkgOut:  map[string][]string{},
		builds:  map[string][]string{},
		elapsed: map[string]float64{},
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
	if e.Action == "skip" {
		// A package with no test files ran nothing and earns no line.
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

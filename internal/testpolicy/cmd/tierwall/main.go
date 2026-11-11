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
// -slow-tests lists, after the package lines, each top-level test that took
// tiersweep.SlowTest (5s) or more, under the package that ran it, so a slow
// package can be explained test by test from the gate's log alone (gt-vijwck).
// With -jsonl the same tests are appended to the series as timings lines.
//
// -slow-tests also prints, after that list, a phases line for each of the three
// slowest packages: `tierwall: phases <package> serial <a>s parallel <b>s`. go
// test runs every top-level test that does not call t.Parallel one at a time,
// and releases the paused parallel tests only after them. So serial is the sum
// of the elapsed of the top-level tests that never reported pause, and parallel
// is the package wall minus that sum, floored at 0, and 0 for a package that ran
// no test (gt-0vbi8h).
//
// -jsonl <path> -run <id> also appends one line per package to a series, so
// "where does the gate's time go" can be read over weeks rather than from one
// run's report (gt-u0up0). An absolute path is used as given; a relative one
// is taken from the town root the checkout sits in, so the gate names the
// town's series without being told where the town is. A package go test
// served from its result cache is still recorded, with "cached": true and no
// wall_s, because a cache hit measured nothing. The report and the exit code
// do not change: a series the tool cannot write, or a run it finds no town
// for, is reported on stderr and fails nothing, because the gate's verdict is
// go test's alone.
//
// The last line of the report, `tierwall: cpus: NumCPU=<n> GOMAXPROCS=<n>
// cgroup=<value>`, says what CPUs the run had: the count Go saw, the Go scheduler
// width, and the cgroup CPU quota, so a gate log shows the limits it ran under
// (gt-50ujsx). It prints on every run and fails nothing.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hoststat"
	"github.com/steveyegge/gastown/internal/tiersweep"
	"github.com/steveyegge/gastown/internal/workspace"
)

// maxLine is the longest stdin line read: one event, which carries a whole
// line of test output, so it can be long.
const maxLine = 1 << 20

// slowTier labels the slow-test lines -slow-tests appends to the series. It is
// the gate's unit tier, the only caller that passes the flag; the sweep's tiers
// (shell, integration, race) name their own.
const slowTier = "unit"

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
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, host{
		now:      time.Now,
		load1:    hostLoad1,
		town:     townRoot,
		mkdir:    mkdirAll,
		open:     openAppend,
		numCPU:   runtime.NumCPU,
		maxProcs: func() int { return runtime.GOMAXPROCS(0) },
		readFile: os.ReadFile,
	}))
}

// host is what run reads outside the stream: the clock a stream with no
// timestamps is stamped from, the load a series line carries, the town a
// relative series path is taken from, the directory and file the series
// appends through, and the CPU facts the report's cpus line prints. main wires
// the real ones; a test injects them, so no test reads the host and every path
// the series can take has a case.
type host struct {
	now   func() time.Time
	load1 func() (float64, bool)
	// town is the town root a relative -jsonl path is taken from; "" means no
	// town was found.
	town  func() string
	mkdir func(dir string) error
	open  func(path string) (io.WriteCloser, error)
	// numCPU and maxProcs are runtime.NumCPU and runtime.GOMAXPROCS(0).
	numCPU   func() int
	maxProcs func() int
	// readFile reads the cgroup quota files; any read error counts the file as
	// unreadable, and the cpus line says so.
	readFile func(path string) ([]byte, error)
}

// run reads a go test -json stream from stdin and writes the report to
// stdout. The exit code is 1 when any package or test failed, else 3 when
// -max-wall is set and the tier is over it, else 4 when the -jsonl series could
// not be written, else 0; 2 means the runner itself failed. The series code is
// last in that order so it never hides a verdict: the gate, which keeps go
// test's verdict, reads it as the alarm that the wall series is incomplete.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, h host) int {
	fs := flag.NewFlagSet("tierwall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bar := fs.Duration("bar", 90*time.Second, "the wall the tier line is judged against (OK or OVER); it fails nothing")
	maxWall := fs.Duration("max-wall", 0, "exit 3 when the tier is over this wall; 0 turns the check off")
	jsonl := fs.String("jsonl", "", "append one JSON line per package to this file (needs -run); a relative path is taken from the town root")
	runID := fs.String("run", "", "the run id each -jsonl line carries")
	slowTests := fs.Bool("slow-tests", false, "list each test of 5s or more under its package, and tag them in -jsonl")
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

	// The slow tests come from tiersweep's parser, which reads the whole stream,
	// so with -slow-tests the stream is buffered and read twice: once for the
	// packages, once for the tests. Without the flag it streams, as before.
	in := stdin
	r := newReport(stdout)
	if *slowTests {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "tierwall:", err)
			return 2
		}
		_, tests, err := tiersweep.ParseTestJSON(bytes.NewReader(data))
		if err != nil {
			// The error names a line number, never the line, so no event text is
			// echoed, and the run stops rather than print a list short of tests.
			fmt.Fprintln(stderr, "tierwall: slow tests:", err)
			return 2
		}
		r.listSlow = true
		r.slow = tiersweep.SlowTests(tests)
		in = bytes.NewReader(data)
	}
	if err := r.read(in); err != nil {
		fmt.Fprintln(stderr, "tierwall:", err)
		return 2
	}
	seriesFailed := false
	if *jsonl != "" {
		// A series that cannot be written is reported on stderr and by exit
		// code 4, and it fails no verdict: the gate's verdict is go test's, and
		// wall time never fails a run. The code is how the gate learns the
		// series is incomplete, since stderr is not something its recipe reads.
		if err := appendSeries(*jsonl, *runID, r, h, stderr); err != nil {
			fmt.Fprintln(stderr, "tierwall:", err)
			seriesFailed = true
		}
	}
	if err := r.print(*bar, cpuLine(h)); err != nil {
		fmt.Fprintln(stderr, "tierwall:", err)
		return 2
	}
	switch {
	case r.failed:
		return 1
	case *maxWall > 0 && r.tier() > *maxWall:
		return 3
	case seriesFailed:
		return 4
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
	cached  map[string]bool     // packages go test served from its result cache

	// paused marks the tests that reported pause, which go test emits when one
	// calls t.Parallel. serial sums, per package, the elapsed of its top-level
	// tests that never paused: the phases line reads it. tested marks each
	// package that reported any test event, so the phases line can tell a
	// package that ran tests from one that ran none.
	paused map[string]bool
	serial map[string]time.Duration
	tested map[string]bool

	// listSlow is set by -slow-tests; slow then holds the tests of SlowTest or
	// longer that the list and the series name.
	listSlow bool
	slow     []tiersweep.TestTime
}

func newReport(out io.Writer) *report {
	return &report{
		out:     out,
		tests:   map[string][]string{},
		pkgOut:  map[string][]string{},
		builds:  map[string][]string{},
		elapsed: map[string]float64{},
		results: map[string]string{},
		cached:  map[string]bool{},
		paused:  map[string]bool{},
		serial:  map[string]time.Duration{},
		tested:  map[string]bool{},
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
	if e.Test != "" {
		r.tested[e.Package] = true
	}
	switch e.Action {
	case "build-output":
		r.builds[e.ImportPath] = append(r.builds[e.ImportPath], e.Output)
	case "output":
		switch {
		case e.Package == "":
			r.builds[e.ImportPath] = append(r.builds[e.ImportPath], e.Output)
		case e.Test == "":
			// A cached package reports its pass with Elapsed 0 and says so on
			// the package's own "ok  \t<pkg>\t(cached)" line. A test's output
			// never reaches this branch, so a test that happens to print the
			// word cannot mark its package.
			if strings.HasSuffix(strings.TrimSpace(e.Output), "(cached)") {
				r.cached[e.Package] = true
			}
			r.pkgOut[e.Package] = append(r.pkgOut[e.Package], e.Output)
		default:
			key := testKey(e.Package, e.Test)
			r.tests[key] = append(r.tests[key], e.Output)
		}
	case "pause":
		if e.Test != "" {
			r.paused[testKey(e.Package, e.Test)] = true
		}
	case "pass", "fail", "skip":
		r.verdict(e)
	}
}

// verdict closes the bucket that a package's or a test's event ends, printing
// it when the verdict is a failure. A top-level test that never paused ran in
// go test's serial phase, so its elapsed joins the package's serial sum; a
// subtest's time sits inside its parent's and is not counted again.
func (r *report) verdict(e event) {
	if e.Test != "" {
		key := testKey(e.Package, e.Test)
		lines := r.tests[key]
		delete(r.tests, key)
		if !r.paused[key] && !strings.Contains(e.Test, "/") {
			r.serial[e.Package] += seconds(e.Elapsed)
		}
		delete(r.paused, key)
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

// print writes the per-package lines, slowest first, then the tier line, then
// cpus, the line cpuLine renders. Ties go by package name, so one stream always
// prints one report.
func (r *report) print(bar time.Duration, cpus string) error {
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
	if r.listSlow {
		// The header prints even when no test was slow, so an empty list reads
		// as a check that ran and found nothing, not as a flag left off.
		r.write("tierwall: slow tests\n")
		for _, tt := range slowest(r.slow) {
			r.write(fmt.Sprintf("tierwall:   %s %s %s\n", tt.Package, tt.Test, secs(seconds(tt.Elapsed))))
		}
		r.phases(pkgs)
	}
	verdict := "OK"
	if r.tier() > bar {
		verdict = "OVER"
	}
	r.write(fmt.Sprintf("tierwall: tier %s bar %s %s\n", secs(r.tier()), secs(bar), verdict))
	r.write(cpus)
	return r.err
}

// cgroup quota files: the v2 unified file, then the v1 CPU controller's quota
// and period. A missing or empty file is not a quota.
const (
	cgroupV2Max    = "/sys/fs/cgroup/cpu.max"
	cgroupV1Quota  = "/sys/fs/cgroup/cpu/cpu.cfs_quota_us"
	cgroupV1Period = "/sys/fs/cgroup/cpu/cpu.cfs_period_us"
)

// cpuLine is the report's last line: the CPUs the run had, as Go counts them
// (NumCPU and GOMAXPROCS) and as the cgroup quota states them.
func cpuLine(h host) string {
	return fmt.Sprintf("tierwall: cpus: NumCPU=%d GOMAXPROCS=%d cgroup=%s\n",
		h.numCPU(), h.maxProcs(), cgroupCPU(h.readFile))
}

// cgroupCPU is the CPU quota the cgroup gives, as the file states it: the v2
// cpu.max content, else the v1 quota and period joined by a space, else
// "unreadable". A read that fails or comes back empty falls through to the
// next source.
func cgroupCPU(read func(path string) ([]byte, error)) string {
	if v, ok := readField(read, cgroupV2Max); ok {
		return v
	}
	quota, qok := readField(read, cgroupV1Quota)
	period, pok := readField(read, cgroupV1Period)
	if qok && pok {
		return quota + " " + period
	}
	return "unreadable"
}

// readField reads one file and returns its content trimmed, and whether there
// was any content to read.
func readField(read func(path string) ([]byte, error), path string) (string, bool) {
	b, err := read(path)
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(b))
	return v, v != ""
}

// phasesShown is how many of the slowest packages -slow-tests splits into a
// serial and a parallel phase.
const phasesShown = 3

// phases writes, for the slowest packages in pkgs (already slowest first), the
// serial and parallel phases of each package's wall. Parallel is the wall less
// the serial sum, floored at 0: a serial sum that outruns the wall reads as no
// parallel phase, not a negative one. A package with no test events has no
// parallel phase at all, so its parallel is 0.
func (r *report) phases(pkgs []string) {
	for i, pkg := range pkgs {
		if i == phasesShown {
			break
		}
		serial := r.serial[pkg]
		var parallel time.Duration
		if r.tested[pkg] {
			parallel = seconds(r.elapsed[pkg]) - serial
		}
		if parallel < 0 {
			parallel = 0
		}
		r.write(fmt.Sprintf("tierwall: phases %s serial %s parallel %s\n", pkg, secs(serial), secs(parallel)))
	}
}

// slowest returns the tests slowest first, ties by package and then test name,
// so one stream always prints one list. It sorts a copy and leaves tests as
// they were.
func slowest(tests []tiersweep.TestTime) []tiersweep.TestTime {
	out := append([]tiersweep.TestTime(nil), tests...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Elapsed != out[j].Elapsed {
			return out[i].Elapsed > out[j].Elapsed
		}
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Test < out[j].Test
	})
	return out
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
// wall_s is left out for a package served from go test's result cache, and
// cached marks that line, so a reader never takes a cache hit's zero for a
// measured wall. load1 is left out when the host reports none, so a line never
// carries a load it did not read.
type wallLine struct {
	TS     time.Time `json:"ts"`
	Run    string    `json:"run"`
	Pkg    string    `json:"pkg"`
	WallS  *float64  `json:"wall_s,omitempty"`
	Result string    `json:"result"`
	Load1  *float64  `json:"load1,omitempty"`
	Cached bool      `json:"cached,omitempty"`
}

// series renders one line per package the stream reported a verdict for,
// sorted by package so one stream always renders one file, then one timings
// line per slow test when -slow-tests listed any. ts is the run's own
// start, the stream's first event; a stream with no timestamps is stamped from
// the clock. The load is read once for the run, and left out when the host
// reports none. A cached package keeps its line and its verdict but carries no
// wall: go test reported no time for it.
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
	var lines []any
	for _, pkg := range pkgs {
		var wall *float64
		if !r.cached[pkg] {
			w := r.elapsed[pkg]
			wall = &w
		}
		lines = append(lines, wallLine{
			TS:     ts,
			Run:    runID,
			Pkg:    pkg,
			WallS:  wall,
			Result: r.results[pkg],
			Load1:  load,
			Cached: r.cached[pkg],
		})
	}
	// A slow test follows the packages in timings.jsonl's own format, with its
	// kind set, so a reader of walls skips it (tiersweep.ParseWalls).
	for _, tt := range slowest(r.slow) {
		lines = append(lines, tiersweep.TestTimingOf(slowTier, tt.Package, tt))
	}
	var buf bytes.Buffer
	for _, v := range lines {
		line, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// appendSeries renders the run's series and appends it to the file -jsonl
// names. A stream with no package verdicts appends nothing and says so: an
// empty run must not read as a run with nothing slow to report. An absolute
// path is taken as given; a relative one is joined to the town root, so the
// gate names the town's series without knowing where the town is.
func appendSeries(path, runID string, r *report, h host, stderr io.Writer) error {
	data, err := r.series(runID, h)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		fmt.Fprintf(stderr, "tierwall: no package events in the stream; nothing appended to %s\n", path)
		return nil
	}
	if !filepath.IsAbs(path) {
		town := h.town()
		if town == "" {
			return fmt.Errorf("%s is relative and no town root was found above the checkout; nothing appended", path)
		}
		path = filepath.Join(town, path)
	}
	return appendLines(path, data, h.mkdir, h.open)
}

// appendLines appends data to path in one write, opened O_APPEND so two runs
// writing at once cannot interleave their lines. The parent directory is
// created first: the town's runtime directory need not exist before the first
// run records.
func appendLines(path string, data []byte, mkdir func(string) error, open func(string) (io.WriteCloser, error)) error {
	if err := mkdir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%s: %w", path, err)
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

// mkdirAll makes a series file's directory, creating it when the town's
// runtime directory does not exist yet.
func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// hostLoad1 reads the host's 1-minute load average, the number a slow wall is
// read against, through the host reading the rest of the town uses (gt-auiji).
func hostLoad1() (float64, bool) {
	load1, _, _ := hoststat.SampleLoad(context.Background())
	if load1 == nil {
		return 0, false
	}
	return *load1, true
}

// townRoot is the town the checkout sits in, or "" when there is none. It
// reads no environment of its own: the town walk is workspace.Find, whose only
// read is the test harness's GT_TEST_FORBIDDEN_TOWN_ROOT guard. The town is the
// outermost ancestor holding mayor/town.json. The working directory answers
// for a gate run inside the town (a polecat worktree); a landing's throwaway
// worktree sits outside the town, so when the directory finds nothing the
// repository it belongs to is asked instead.
func townRoot() string {
	return townRootAt(os.Getwd, gitCommonDir)
}

// townRootAt is townRoot over the working-directory and git readers it uses,
// so a test reads a tree it built rather than the host.
func townRootAt(getwd func() (string, error), commonDir func() (string, error)) string {
	if cwd, err := getwd(); err == nil {
		if town := townAbove(cwd); town != "" {
			return town
		}
	}
	gitDir, err := commonDir()
	if err != nil {
		return ""
	}
	// The common dir is <repo>/.git for a checkout and a worktree alike, so
	// the repository is its parent.
	return townAbove(filepath.Dir(gitDir))
}

// townAbove returns the outermost ancestor of dir holding mayor/town.json, or
// "" when none does. The outermost marker is the town: a rig can carry a
// mayor/ of its own (gt-y3pgh.2).
func townAbove(dir string) string {
	root, err := workspace.Find(dir)
	if err != nil || root == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, workspace.PrimaryMarker)); err != nil {
		return ""
	}
	return root
}

// gitTimeout bounds the one git call the town lookup makes: a repository that
// cannot answer costs the series, never the run.
const gitTimeout = 2 * time.Second

// gitCommonDir is the repository's shared .git directory, which a linked
// worktree points at even when its own directory sits outside the town.
func gitCommonDir() (string, error) {
	return gitCommonDirFrom(runGitRevParse)
}

// gitCommonDirFrom is gitCommonDir over the command it runs, so a test can
// drive the failure path without a repository.
func gitCommonDirFrom(run func() ([]byte, error)) (string, error) {
	out, err := run()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runGitRevParse runs the one git command the town lookup makes. It is bounded
// so a repository that cannot answer costs the series, never the run.
func runGitRevParse() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
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

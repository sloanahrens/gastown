package tiersweep

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitBound is how long a worker test waits for the pool to reach the state it
// is driving. It is a hang guard, not the behaviour under test: a correct pool
// reaches every state at once, and a regressed one fails here instead of
// hanging the suite.
const waitBound = 10 * time.Second

// waitOrFail is how these tests wait: the next value on ch, or a failure naming
// what never arrived. No test waits on a channel without this bound.
func waitOrFail[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitBound): //testpolicy:allow no-sleep — a hang guard only: the event is what the test waits for, and a pool that never reaches it fails here rather than hanging the suite
	}
	t.Fatalf("waited %s for %s", waitBound, what)
	var zero T
	return zero
}

// poolGo is the Exec a worker test injects. A command for a watched package
// records its start, reports it on started, waits for its release and reports
// its return on finished, so the test drives the pool one event at a time with
// no polling and no scheduler yields.
//
// The fake also holds the pool's own invariants at the moment a command begins:
// no more than jobs in flight, a package that must run alone with nothing
// beside it, and nothing starting beside it while it runs. A breach fails the
// test at that start, so the failure does not depend on when the test looks.
type poolGo struct {
	// jobs is the most commands that may be in flight at once, 0 for no bound.
	jobs int
	// alone names the packages that must run alone, as serial.txt does.
	alone func(pkg string) bool
	// hold returns the channel a watched package's command waits on; nil runs
	// it at once.
	hold func(pkg string) <-chan struct{}
	// watch names the packages the fake records. A command for any other
	// package, such as git, returns its scripted code and is not one of the
	// run's children; nil watches every command.
	watch    map[string]bool
	codes    map[string]int
	output   map[string]string
	started  chan string
	finished chan string
	t        *testing.T

	mu       sync.Mutex
	inFlight int
	serial   int
	starts   []string
	// begins is the order the pool hands commands to Exec, as Runner.Began
	// reports it, recorded before the pool's handshake lets the next one start.
	begins []string
}

// began records a package the pool has handed to Exec. It is the Runner.Began
// hook, so a test compares the first wave against the order the pool keeps.
func (f *poolGo) began(pkg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins = append(f.begins, pkg)
}

// beginOrder is the order the pool handed its commands to Exec.
func (f *poolGo) beginOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.begins...)
}

func (f *poolGo) exec(_ string, _ []string, argv []string, out io.Writer) int {
	pkg := argv[len(argv)-1]
	if f.watch != nil && !f.watch[pkg] {
		return f.codes[pkg]
	}
	f.enter(pkg)
	if f.started != nil {
		f.started <- pkg
	}
	if f.hold != nil {
		if gate := f.hold(pkg); gate != nil {
			<-gate
		}
	}
	fmt.Fprint(out, f.output[pkg])
	f.leave(pkg)
	if f.finished != nil {
		f.finished <- pkg
	}
	return f.codes[pkg]
}

// enter records a command's start and fails the test when the start breaks an
// invariant of the pool.
func (f *poolGo) enter(pkg string) {
	f.mu.Lock()
	inFlight, serial := f.inFlight, f.serial
	alone := f.alone != nil && f.alone(pkg)
	f.inFlight++
	if alone {
		f.serial++
	}
	f.starts = append(f.starts, pkg)
	f.mu.Unlock()
	switch {
	case f.jobs > 0 && inFlight >= f.jobs:
		f.t.Errorf("%s started with %d commands already in flight, want at most %d at once", pkg, inFlight, f.jobs)
	case alone && inFlight > 0:
		f.t.Errorf("%s must run alone, but started with %d commands in flight", pkg, inFlight)
	case !alone && serial > 0:
		f.t.Errorf("%s started while a serial package ran, want a serial package alone", pkg)
	}
}

// leave records a command's end.
func (f *poolGo) leave(pkg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	if f.alone != nil && f.alone(pkg) {
		f.serial--
	}
}

// startOrder lists the packages in the order their commands began, as the fake
// itself recorded them.
func (f *poolGo) startOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...)
}

// ranPackages lists the packages the fake ran, sorted, so a test compares the
// set whatever order the workers took them in.
func (f *poolGo) ranPackages() []string {
	pkgs := f.startOrder()
	sort.Strings(pkgs)
	return pkgs
}

// lineFeed is a Runner's stderr in a test: it keeps the text and wakes every
// waiter on each write, so a write never blocks on a reader and a test waits
// for the line it wants without polling.
type lineFeed struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	notify chan struct{}
}

func newLineFeed() *lineFeed { return &lineFeed{notify: make(chan struct{})} }

func (f *lineFeed) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.buf.Write(p)
	close(f.notify)
	f.notify = make(chan struct{})
	return n, err
}

func (f *lineFeed) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// waitForLine waits until the text written so far holds want.
func (f *lineFeed) waitForLine(t *testing.T, want string) {
	t.Helper()
	for {
		f.mu.Lock()
		written := strings.Contains(f.buf.String(), want)
		notified := f.notify
		f.mu.Unlock()
		if written {
			return
		}
		select {
		case <-notified:
		case <-time.After(waitBound): //testpolicy:allow no-sleep — a hang guard only: the write is the event waited for, and a report that never comes fails here rather than hanging the suite
			t.Fatalf("waited %s for a stderr line holding %s", waitBound, want)
		}
	}
}

// newWorkerRunner is a Runner over exec with jobs workers, logging into a fresh
// temporary directory and timed by a clock that never moves.
func newWorkerRunner(t *testing.T, exec Exec, jobs int) *Runner {
	t.Helper()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	return &Runner{Exec: exec, LogDir: t.TempDir(), Timeout: "7m", Stderr: io.Discard,
		Jobs: jobs, Now: func() time.Time { return at }}
}

// workerPackages is n package names a fake run uses, sorted.
func workerPackages(n int) []string {
	var pkgs []string
	for i := 0; i < n; i++ {
		pkgs = append(pkgs, fmt.Sprintf("./p%d", i))
	}
	return pkgs
}

// gates is one channel per package a test releases by hand, and a release that
// may be called twice for an already released package.
type gates struct {
	byPkg    map[string]chan struct{}
	released map[string]bool
}

func newGates(pkgs []string) *gates {
	g := &gates{byPkg: map[string]chan struct{}{}, released: map[string]bool{}}
	for _, pkg := range pkgs {
		g.byPkg[pkg] = make(chan struct{})
	}
	return g
}

func (g *gates) channel(pkg string) <-chan struct{} { return g.byPkg[pkg] }

func (g *gates) release(pkg string) {
	if !g.released[pkg] {
		g.released[pkg] = true
		close(g.byPkg[pkg])
	}
}

func (g *gates) releaseAll() {
	for pkg := range g.byPkg {
		g.release(pkg)
	}
}

// TestWorkersNeverExceedJobs: Jobs bounds the packages in flight, and every
// package still completes once the held ones are released.
func TestWorkersNeverExceedJobs(t *testing.T) {
	t.Parallel()
	const jobs = 3
	pkgs := workerPackages(8)
	gates := newGates(pkgs)
	fake := &poolGo{
		jobs:     jobs,
		t:        t,
		hold:     gates.channel,
		started:  make(chan string, len(pkgs)),
		finished: make(chan string, len(pkgs)),
	}
	r := newWorkerRunner(t, fake.exec, jobs)

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()

	// Fill the pool: the first Jobs commands start together and are held, so a
	// pool that ran them one at a time never reaches Jobs and fails here.
	held := make([]string, 0, jobs)
	for i := 0; i < jobs; i++ {
		held = append(held, waitOrFail(t, fake.started, "a command of the first wave to start"))
	}
	// Keep it full: each release frees a slot and the pool starts the next
	// package into it, so a pool that handed out one worker too many starts it
	// beside Jobs held commands and the fake fails that start.
	for i := jobs; i < len(pkgs); i++ {
		gates.release(held[0])
		held = append(held[1:], waitOrFail(t, fake.started, "a command to take the freed slot"))
		waitOrFail(t, fake.finished, "the released command to finish")
	}
	gates.releaseAll()
	for range held {
		waitOrFail(t, fake.finished, "a command to finish")
	}

	if got := waitOrFail(t, finished, "the tier to count its packages"); !reflect.DeepEqual(got, Counts{Passed: len(pkgs)}) {
		t.Errorf("Tier() = %+v, want all %d passed", got, len(pkgs))
	}
	if ran := fake.ranPackages(); !reflect.DeepEqual(ran, pkgs) {
		t.Errorf("ran %q, want each of %q once", ran, pkgs)
	}
}

// TestWorkersOneAtATime: a run of one job starts one package at a time, and
// every package completes once its own command is released.
func TestWorkersOneAtATime(t *testing.T) {
	t.Parallel()
	pkgs := workerPackages(3)
	gates := newGates(pkgs)
	fake := &poolGo{
		jobs:     1,
		t:        t,
		hold:     gates.channel,
		started:  make(chan string, len(pkgs)),
		finished: make(chan string, len(pkgs)),
	}
	r := newWorkerRunner(t, fake.exec, 1)

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()

	// A second command starting while the first is held fails in the fake.
	for range pkgs {
		gates.release(waitOrFail(t, fake.started, "the next command to start"))
		waitOrFail(t, fake.finished, "the command to finish")
	}

	if got := waitOrFail(t, finished, "the tier to count its packages"); !reflect.DeepEqual(got, Counts{Passed: len(pkgs)}) {
		t.Errorf("Tier() = %+v, want all %d passed", got, len(pkgs))
	}
	if got := fake.startOrder(); !reflect.DeepEqual(got, pkgs) {
		t.Errorf("start order = %q, want the list order %q", got, pkgs)
	}
}

// TestWorkersOneJobKeepsTheSerialOutput: a run of one job prints and starts
// every package where it is listed — announced, skipped or refused — as the
// sweep's serial run always has, whatever walls the last run left, so the
// daemon's parse and the log order do not change under the default.
func TestWorkersOneJobKeepsTheSerialOutput(t *testing.T) {
	t.Parallel()
	pkgs := []string{"./a", "-weird", "./blocked", "./c"}
	fake := &poolGo{jobs: 1, t: t}
	r := newWorkerRunner(t, fake.exec, 1)
	r.SkipList = "integration:./blocked"
	r.Walls = map[string]int64{
		TimingsKey(TierIntegration, "./a"):    1000,
		TimingsKey(TierIntegration, "./c"):    50000,
		TimingsKey(TierIntegration, "./okay"): 9000,
	}
	stderr := &bytes.Buffer{}
	r.Stderr = stderr

	got := r.Tier(TierIntegration, ".", nil, nil, pkgs)

	want := "tier-sweep: integration ./a ...\n" +
		"-weird: package argument starts with a dash\n" +
		"tier-sweep: integration ./blocked SKIPPED (TIER_SWEEP_SKIP)\n" +
		"tier-sweep: integration ./c ...\n"
	if stderr.String() != want {
		t.Errorf("stderr =\n%s\nwant the serial run's lines\n%s", stderr, want)
	}
	if want := []string{"./a", "./c"}; !reflect.DeepEqual(fake.startOrder(), want) {
		t.Errorf("start order = %q, want the list order %q, not the walls'", fake.startOrder(), want)
	}
	if want := (Counts{Passed: 2, Skipped: 1, Failed: 1, FailedNames: []string{"-weird"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("Tier() = %+v, want %+v", got, want)
	}
}

// TestWorkersStartLongestFirst: a run given the previous walls starts the
// longest packages first, in that order even inside the first wave, and a run
// given none starts them in list order. Each place is the fake's own record of
// the command's start.
func TestWorkersStartLongestFirst(t *testing.T) {
	t.Parallel()
	const jobs = 3
	cases := []struct {
		name  string
		pkgs  []string
		walls map[string]int64
		want  []string
	}{
		{
			name: "walls start the longest first",
			pkgs: []string{"./a", "./b", "./c", "./d", "./e"},
			walls: wallsFor(map[string]int64{
				"./a": 5000, "./b": 50000, "./c": 20000, "./d": 1000, "./e": 30000,
			}),
			want: []string{"./b", "./e", "./c", "./a", "./d"},
		},
		{
			name: "no walls keep the list order",
			pkgs: []string{"./d", "./c", "./b", "./a", "./e"},
			want: []string{"./d", "./c", "./b", "./a", "./e"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gates := newGates(tc.pkgs)
			fake := &poolGo{
				jobs:     jobs,
				t:        t,
				hold:     gates.channel,
				started:  make(chan string, len(tc.pkgs)),
				finished: make(chan string, len(tc.pkgs)),
			}
			r := newWorkerRunner(t, fake.exec, jobs)
			r.Walls = tc.walls

			finished := make(chan Counts, 1)
			go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, tc.pkgs) }()

			first := make([]string, 0, jobs)
			for i := 0; i < jobs; i++ {
				first = append(first, waitOrFail(t, fake.started, "a command of the first wave to start"))
			}
			if !reflect.DeepEqual(first, tc.want[:jobs]) {
				t.Errorf("the first wave started %q, want the longest %q in that order", first, tc.want[:jobs])
			}

			// Freeing a slot starts the next package the plan names, one at a
			// time, so each later place is the fake's own record.
			for i := jobs; i < len(tc.want); i++ {
				gates.release(tc.want[i-jobs])
				pkg := waitOrFail(t, fake.started, fmt.Sprintf("start %d", i+1))
				if pkg != tc.want[i] {
					t.Errorf("start %d = %s, want %s", i+1, pkg, tc.want[i])
					break
				}
			}
			gates.releaseAll()
			if got := waitOrFail(t, finished, "the tier to count its packages"); !reflect.DeepEqual(got, Counts{Passed: len(tc.pkgs)}) {
				t.Errorf("Tier() = %+v, want all %d passed", got, len(tc.pkgs))
			}
		})
	}
}

// TestStartOrderKeepsARecordedZeroWall: a package the previous run timed at
// zero still has a wall, so it starts ahead of one the file does not name.
func TestStartOrderKeepsARecordedZeroWall(t *testing.T) {
	t.Parallel()
	tasks := []*task{{name: "./unmeasured"}, {name: "./zero"}, {name: "./long"}}
	walls := map[string]int64{
		TimingsKey(TierIntegration, "./zero"): 0,
		TimingsKey(TierIntegration, "./long"): 9000,
	}
	var order []string
	for _, task := range startOrder(tasks, walls, TierIntegration) {
		order = append(order, task.name)
	}
	want := []string{"./long", "./zero", "./unmeasured"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("start order = %q, want %q", order, want)
	}
}

// wallsFor keys each package's wall as the run reads it: by the tier and the
// package name together.
func wallsFor(walls map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(walls))
	for pkg, ms := range walls {
		out[TimingsKey(TierIntegration, pkg)] = ms
	}
	return out
}

// TestSerialPackagesRunAlone: a package serial.txt names starts only once no
// other command is in flight. The fake fails the start if anything runs beside
// it, so the test only has to drive the run to the state where a package that
// shared the workers would have started.
func TestSerialPackagesRunAlone(t *testing.T) {
	t.Parallel()
	const serial = "./internal/cmd"
	if !serialPackage(serial) {
		t.Fatalf("%s is not on serial.txt, so the run has no serial package to keep alone", serial)
	}
	parallel := make(chan struct{})
	alone := make(chan struct{})
	fake := &poolGo{
		jobs:  3,
		t:     t,
		alone: serialPackage,
		hold: func(pkg string) <-chan struct{} {
			if pkg == serial {
				return alone
			}
			return parallel
		},
		started:  make(chan string, 3),
		finished: make(chan string, 3),
	}
	r := newWorkerRunner(t, fake.exec, 3)
	pkgs := []string{"./a", serial, "./b"}

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()

	// The two packages beside it are held, so a serial package that shared
	// their workers would start now, beside two commands in flight.
	for i := 0; i < len(pkgs)-1; i++ {
		waitOrFail(t, fake.started, "a package beside the serial one to start")
	}
	close(parallel)
	if pkg := waitOrFail(t, fake.started, "the serial package to start"); pkg != serial {
		t.Errorf("started %s after the parallel drain, want the serial package %s", pkg, serial)
	}
	close(alone)
	for range pkgs {
		waitOrFail(t, fake.finished, "a command to finish")
	}

	if got := waitOrFail(t, finished, "the tier to count its packages"); !reflect.DeepEqual(got, Counts{Passed: len(pkgs)}) {
		t.Errorf("Tier() = %+v, want all %d passed", got, len(pkgs))
	}
	// The two beside it start first, in list order, and the serial package only
	// once both are done.
	if order, want := fake.startOrder(), []string{"./a", "./b", serial}; !reflect.DeepEqual(order, want) {
		t.Errorf("start order = %q, want %q", order, want)
	}
}

// TestWorkersKeepTheSerialResult: a mixed pass, fail and skip fixture reports
// the same summary line on three workers as on one.
func TestWorkersKeepTheSerialResult(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	logDir := t.TempDir()
	summary := func(jobs int) string {
		repo := baseRepo()
		repo.lists[". *_test.go"] = []string{"a/a_test.go", "b/b_test.go", "c/c_test.go"}
		for _, f := range repo.lists[". *_test.go"] {
			repo.files[f] = taggedTest
		}
		repo.files[dockerList] = "d\n"
		fake := &poolGo{codes: map[string]int{"./b": 1, "./d": 1}}
		s := &Sweep{
			Opts:    Options{Jobs: jobs, SkipList: "integration:./c", Timeout: "7m", LogDir: logDir},
			Exec:    fake.exec,
			Src:     repo,
			Environ: []string{"PATH=/fake/bin"},
			Stderr:  io.Discard,
			Now:     func() time.Time { return at },
		}
		line, _ := s.Integration()
		return line
	}

	serial := summary(1)
	parallel := summary(3)
	if parallel != serial {
		t.Errorf("summary at Jobs=3 = %q, want the Jobs=1 line %q", parallel, serial)
	}
	if !strings.Contains(serial, "failed=2 skipped=1 failed: ./b untagged:./d") {
		t.Errorf("summary %q does not name the failures in package-list order", serial)
	}
}

// TestWorkersKeepTheFailureOrder: a failure is named where its package was
// listed, whatever order the workers finished in.
func TestWorkersKeepTheFailureOrder(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	fake := &poolGo{
		jobs:  3,
		t:     t,
		codes: map[string]int{"./b": 1, "./e": 1},
		hold: func(pkg string) <-chan struct{} {
			if pkg == "./b" {
				return gate
			}
			return nil
		},
		started:  make(chan string, 3),
		finished: make(chan string, 3),
	}
	r := newWorkerRunner(t, fake.exec, 3)
	pkgs := []string{"./a", "./b", "./e"}

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()

	// ./e finishes first though ./b is listed before it.
	for {
		if pkg := waitOrFail(t, fake.finished, "./e to finish ahead of ./b"); pkg == "./e" {
			break
		}
	}
	close(gate)

	want := Counts{Passed: 1, Failed: 2, FailedNames: []string{"./b", "./e"}}
	if got := waitOrFail(t, finished, "the tier to count its packages"); !reflect.DeepEqual(got, want) {
		t.Errorf("Tier() = %+v, want %+v", got, want)
	}
}

// TestWorkersReportFailures: a pool reports a failure the way a serial run
// does, with the log's excerpt, and a package whose log cannot be created is
// counted failed rather than run.
func TestWorkersReportFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		logDir   func(t *testing.T) string
		output   map[string]string
		want     Counts
		wantLine string
	}{
		{
			name:     "a failing package shows its excerpt",
			logDir:   func(t *testing.T) string { return t.TempDir() },
			output:   map[string]string{"./bad": "--- FAIL: TestBad (0.00s)\n"},
			want:     Counts{Passed: 1, Failed: 1, FailedNames: []string{"./bad"}},
			wantLine: "    --- FAIL: TestBad (0.00s)",
		},
		{
			name:     "a log that cannot be created fails the package",
			logDir:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
			want:     Counts{Failed: 2, FailedNames: []string{"./ok", "./bad"}},
			wantLine: "    create log ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &poolGo{jobs: 2, t: t, codes: map[string]int{"./bad": 1}, output: tc.output}
			stderr := &bytes.Buffer{}
			r := newWorkerRunner(t, fake.exec, 2)
			r.LogDir = tc.logDir(t)
			r.Stderr = stderr
			got := r.Tier(TierIntegration, ".", nil, nil, []string{"./ok", "./bad"})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Tier() = %+v, want %+v", got, tc.want)
			}
			if !strings.Contains(stderr.String(), tc.wantLine) {
				t.Errorf("stderr has no %q line:\n%s", tc.wantLine, stderr)
			}
		})
	}
}

// TestWorkersKeepEachPackagesLogAndTiming: three packages on three workers each
// write their own output to their own log, and each records its own line in the
// timings file in the order its command finished.
func TestWorkersKeepEachPackagesLogAndTiming(t *testing.T) {
	t.Parallel()
	pkgs := []string{"./a", "./b", "./c"}
	codes := map[string]int{"./a": 1, "./b": 1, "./c": 1}
	output := map[string]string{
		"./a": "--- FAIL: TestA (0.00s)\n",
		"./b": "--- FAIL: TestB (0.00s)\n",
		"./c": "--- FAIL: TestC (0.00s)\n",
	}
	gates := newGates(pkgs)
	fake := &poolGo{
		jobs:     3,
		t:        t,
		codes:    codes,
		output:   output,
		hold:     gates.channel,
		started:  make(chan string, len(pkgs)),
		finished: make(chan string, len(pkgs)),
	}
	logDir := t.TempDir()
	feed := newLineFeed()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	r := &Runner{
		Exec: fake.exec, LogDir: logDir, Timeout: "7m", Stderr: feed,
		Timings: OpenTimings(filepath.Join(logDir, TimingsName), feed),
		Now:     func() time.Time { return at }, Jobs: 3,
		// The plan starts ./a, ./b, ./c in that order; the test finishes them
		// the other way round, so the file's order is read as completion order.
		Walls: wallsFor(map[string]int64{"./a": 30000, "./b": 20000, "./c": 10000}),
	}

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()
	for range pkgs {
		waitOrFail(t, fake.started, "a command to start")
	}

	// Each package's excerpt is printed after its timings line is recorded, so
	// releasing them one at a time reads the completion order the file holds.
	order := []string{"./a", "./c", "./b"}
	for _, pkg := range order {
		gates.release(pkg)
		feed.waitForLine(t, strings.TrimSpace(output[pkg]))
	}
	if got := waitOrFail(t, finished, "the tier to count its packages"); got.Failed != len(pkgs) {
		t.Errorf("Tier() = %+v, want every package failed", got)
	}
	r.Timings.Close()

	var timed []string
	for _, tm := range readTimings(t, filepath.Join(logDir, TimingsName)) {
		timed = append(timed, tm.Package)
		if tm.Tier != TierIntegration || tm.Exit != codes[tm.Package] {
			t.Errorf("timing %+v does not name its own tier and exit code", tm)
		}
	}
	if !reflect.DeepEqual(timed, order) {
		t.Errorf("timings order = %q, want the completion order %q", timed, order)
	}
	for _, pkg := range pkgs {
		data, err := os.ReadFile(filepath.Join(logDir, LogName(TierIntegration, pkg)))
		if err != nil {
			t.Fatalf("read %s's log: %v", pkg, err)
		}
		if got := string(data); got != output[pkg] {
			t.Errorf("%s's log holds %q, want only its own %q", pkg, got, output[pkg])
		}
	}
}

// timingsText is a timings file's lines for walls, keyed by package.
func timingsText(walls map[string]int64, pkgs []string) string {
	var out strings.Builder
	for _, pkg := range pkgs {
		fmt.Fprintf(&out, `{"tier":%q,"package":%q,"wall_ms":%d,"exit":0,"start":"2026-10-30T00:00:00Z"}`+"\n",
			TierIntegration, pkg, walls[pkg])
	}
	return out.String()
}

// taggedRepo is a fake repository whose integration tier discovers each of
// pkgs as a tagged package.
func taggedRepo(pkgs []string) *fakeRepo {
	repo := baseRepo()
	var files []string
	for _, pkg := range pkgs {
		name := strings.TrimPrefix(pkg, "./") + "/x_test.go"
		files = append(files, name)
		repo.files[name] = taggedTest
	}
	repo.lists[". *_test.go"] = files
	return repo
}

// runTierInBackground runs the integration tier of repo through Run on the
// environment env, timed by a clock that never moves, and returns the exit code
// it ends with.
func runTierInBackground(repo *fakeRepo, fake *poolGo, env map[string]string, stderr io.Writer) <-chan int {
	done := make(chan int, 1)
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	go func() {
		done <- Run([]string{TierIntegration}, io.Discard, stderr, Deps{
			Host: Host{
				Env:      func(name string) string { return env[name] },
				MkdirAll: func(string, fs.FileMode) error { return nil },
			},
			Exec:  fake.exec,
			Src:   repo,
			Now:   func() time.Time { return at },
			Began: fake.began,
		})
	}()
	return done
}

// TestWorkersTimingsFileFallback: a run on two workers starts the longest
// packages first when TIER_SWEEP_TIMINGS names a file it can read, and keeps
// the list order when the file is the first run's or cannot be read, naming the
// latter once, so the instrument never decides the order.
func TestWorkersTimingsFileFallback(t *testing.T) {
	t.Parallel()
	pkgs := []string{"./a", "./b", "./c", "./d"}
	walls := map[string]int64{"./a": 5000, "./b": 1000, "./c": 50000, "./d": 20000}
	cases := []struct {
		name     string
		path     func(t *testing.T) string
		want     []string
		wantLine bool
	}{
		{
			name: "a file that is not there is the first run",
			path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "timings.jsonl") },
			want: pkgs,
		},
		{
			name:     "a file that cannot be read is named once",
			path:     func(t *testing.T) string { return t.TempDir() },
			want:     pkgs,
			wantLine: true,
		},
		{
			name: "the file's walls start the longest first",
			path: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "timings.jsonl")
				if err := os.WriteFile(path, []byte(timingsText(walls, pkgs)), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: []string{"./c", "./d", "./a", "./b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tc.path(t)
			gates := newGates(pkgs)
			fake := &poolGo{
				jobs:     2,
				t:        t,
				watch:    watchAll(pkgs),
				hold:     gates.channel,
				started:  make(chan string, len(pkgs)),
				finished: make(chan string, len(pkgs)),
			}
			env := map[string]string{EnvLogDir: t.TempDir(), EnvJobs: "2", EnvTimings: path}
			stderr := &bytes.Buffer{}
			done := runTierInBackground(taggedRepo(pkgs), fake, env, stderr)

			// The order is read where the pool hands each command out, not where
			// its Exec begins: the pool's handshake orders the first, so the
			// record is complete once both commands have started.
			for i := 0; i < 2; i++ {
				waitOrFail(t, fake.started, "a command of the first wave to start")
			}
			if first := fake.beginOrder(); !reflect.DeepEqual(first, tc.want[:2]) {
				t.Errorf("the first wave started %q, want %q in that order", first, tc.want[:2])
			}
			for i := 2; i < len(tc.want); i++ {
				gates.release(tc.want[i-2])
				pkg := waitOrFail(t, fake.started, fmt.Sprintf("start %d", i+1))
				if pkg != tc.want[i] {
					t.Errorf("start %d = %s, want %s", i+1, pkg, tc.want[i])
					break
				}
			}
			gates.releaseAll()
			if code := waitOrFail(t, done, "the run to exit"); code != 0 {
				t.Errorf("Run exit = %d, want 0:\n%s", code, stderr)
			}
			if named := strings.Contains(stderr.String(), "timings: read "+path); named != tc.wantLine {
				t.Errorf("stderr names the timings file = %t, want %t:\n%s", named, tc.wantLine, stderr)
			}
			if got := len(fake.startOrder()); got != len(pkgs) {
				t.Errorf("ran %d packages, want %d: %q", got, len(pkgs), fake.startOrder())
			}
		})
	}
}

// watchAll is the set of packages a Run-level fake records.
func watchAll(pkgs []string) map[string]bool {
	watch := make(map[string]bool, len(pkgs))
	for _, pkg := range pkgs {
		watch[pkg] = true
	}
	return watch
}

// undiscoveredSerial lists the packages serial.txt names that the plan does not
// run, so a renamed or deleted package cannot silently drop out of the list
// that keeps it alone. The integration tier runs it against the repository
// through the production listing; the unit tier checks the test that reads it.
func undiscoveredSerial(serial []string, found map[string]bool) []string {
	var missing []string
	for _, pkg := range serial {
		if !found[pkg] {
			missing = append(missing, pkg)
		}
	}
	return missing
}

// TestSerialGuardNamesUndiscoveredPackages: the guard reports a serial package
// the plan does not run and keeps the ones it does.
func TestSerialGuardNamesUndiscoveredPackages(t *testing.T) {
	t.Parallel()
	found := map[string]bool{"./internal/tmux": true, "./internal/lock": true}
	serial := []string{"./internal/tmux", "./gone", "./internal/lock"}
	if got, want := undiscoveredSerial(serial, found), []string{"./gone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("undiscoveredSerial = %q, want %q", got, want)
	}
	if got := undiscoveredSerial(serial[:1], found); len(got) != 0 {
		t.Errorf("undiscoveredSerial = %q, want none", got)
	}
}

// TestSerialPackagesParse: serial.txt holds package names, not repeats, and the
// guard has a list to read.
func TestSerialPackagesParse(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for _, pkg := range SerialPackages() {
		if strings.HasPrefix(pkg, "./") || pkg == "" {
			t.Errorf("serial.txt names %q, want a name without ./", pkg)
		}
		if seen[pkg] {
			t.Errorf("serial.txt names %s twice", pkg)
		}
		seen[pkg] = true
	}
	if len(seen) == 0 {
		t.Error("serial.txt names no package")
	}
}

// TestDiscoveredPackagesCountsOnlyTaggedTests: the plan the serial guard reads
// discovers a package that adds an integration-tagged file and leaves one whose
// test carries no tag out.
func TestDiscoveredPackagesCountsOnlyTaggedTests(t *testing.T) {
	t.Parallel()
	repo := taggedRepo([]string{"./a"})
	repo.lists[". *_test.go"] = append(repo.lists[". *_test.go"], "b/b_test.go")
	repo.files["b/b_test.go"] = plainTest
	found := discoveredPackages(t, repo)
	if !found["a"] {
		t.Error("a package with an integration-tagged test reads as undiscovered")
	}
	if found["b"] {
		t.Error("a package with only a plain test reads as discovered")
	}
}

// discoveredPackages is the set of packages the integration tier's plan runs,
// by the name serial.txt uses: the plan's own discovery, not a re-reading of
// it.
func discoveredPackages(t *testing.T, src Source) map[string]bool {
	t.Helper()
	plan, err := (&Sweep{Src: src}).planIntegration()
	if err != nil {
		t.Fatalf("plan the integration tier: %v", err)
	}
	found := make(map[string]bool)
	for _, pkg := range plan.root {
		found[strings.TrimPrefix(pkg, "./")] = true
	}
	for _, m := range plan.modules {
		for _, pkg := range m.pkgs {
			found[strings.TrimPrefix(packageName(m.dir, pkg), "./")] = true
		}
	}
	return found
}

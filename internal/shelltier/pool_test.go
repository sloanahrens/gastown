package shelltier

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitFor is how these tests wait: the next value on ch, or a failure naming
// what never arrived. A correct pool reaches the state at once; a regressed one
// fails here instead of hanging the suite.
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(hangGuard): //testpolicy:allow no-sleep — a hang guard only: the event is what the test waits for, and a pool that never reaches it fails here rather than hanging the suite
	}
	t.Fatalf("waited %s for %s", hangGuard, what)
	var zero T
	return zero
}

// endRun releases every held script and waits, within the hang guard, for the
// run to end: a run that does not end once its scripts are released is a
// regression, so the test fails here rather than hanging the suite.
func endRun(t *testing.T, held *gates, done <-chan struct{}) {
	t.Helper()
	held.releaseAll()
	waitFor(t, done, "the run to finish once every script was released")
}

// clockAt is a Now that always answers at, safe to call from the pool's workers
// at once.
func clockAt(at time.Time) func() time.Time { return func() time.Time { return at } }

// scriptsIn builds n script paths, distinct and in list order.
func scriptsIn(n int) []string {
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		paths = append(paths, fmt.Sprintf("scripts/p%d_test.sh", i))
	}
	return paths
}

// poolEntries builds one entry per path: Parallel unless modes names another
// mode for it.
func poolEntries(paths []string, modes map[string]Mode) []Entry {
	var out []Entry
	for _, p := range paths {
		mode := Parallel
		if named, ok := modes[p]; ok {
			mode = named
		}
		out = append(out, Entry{Path: p, Mode: mode})
	}
	return out
}

// poolConfig is a Config over entries whose fakes never touch the host.
func poolConfig(entries []Entry, jobs int, exec Exec) Config {
	return Config{
		Entries:     entries,
		Root:        "/repo",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Jobs:        jobs,
		Exec:        exec,
		Now:         clockAt(time.Unix(0, 0)),
		WithTimeout: liveTimeout,
	}
}

// gates is one release channel per script, each closeable once.
type gates struct {
	byPath   map[string]chan struct{}
	released map[string]bool
}

func newGates(paths []string) *gates {
	g := &gates{byPath: make(map[string]chan struct{}, len(paths)), released: map[string]bool{}}
	for _, p := range paths {
		g.byPath[p] = make(chan struct{})
	}
	return g
}

func (g *gates) channel(path string) <-chan struct{} { return g.byPath[path] }

func (g *gates) release(path string) {
	if !g.released[path] {
		g.released[path] = true
		close(g.byPath[path])
	}
}

func (g *gates) releaseAll() {
	for path := range g.byPath {
		g.release(path)
	}
}

// poolFake is the Exec these tests inject. A script records its start, signals
// started, waits for its release and signals finished, so a test drives the
// pool one event at a time with no polling and no scheduler yields.
//
// The fake also holds the pool's invariants at the moment a script begins: no
// more than jobs in flight, and a script the serial set names with nothing
// beside it. A breach fails the test at that start, so the failure does not
// depend on when the test looks.
type poolFake struct {
	// jobs is the most scripts that may be in flight at once, 0 for no bound.
	jobs int
	// serial names the scripts that must run alone.
	serial func(path string) bool
	// hold returns the channel a script's run waits on; nil runs it at once.
	hold     func(path string) <-chan struct{}
	codes    map[string]int
	started  chan string
	finished chan string
	t        *testing.T

	mu       sync.Mutex
	inFlight int
	serials  int
	starts   []string
}

func (f *poolFake) exec(_ context.Context, c Command) (int, error) {
	path := c.Argv[1]
	f.enter(path)
	f.started <- path
	if f.hold != nil {
		if gate := f.hold(path); gate != nil {
			<-gate
		}
	}
	f.leave(path)
	f.finished <- path
	return f.codes[path], nil
}

// enter records a script's start and fails the test when the start breaks an
// invariant of the pool.
func (f *poolFake) enter(path string) {
	f.mu.Lock()
	inFlight, serials := f.inFlight, f.serials
	alone := f.serial != nil && f.serial(path)
	f.inFlight++
	if alone {
		f.serials++
	}
	f.starts = append(f.starts, path)
	f.mu.Unlock()
	switch {
	case f.jobs > 0 && inFlight >= f.jobs:
		f.t.Errorf("%s started with %d scripts already in flight, want at most %d at once", path, inFlight, f.jobs)
	case alone && inFlight > 0:
		f.t.Errorf("%s must run alone, but started with %d scripts in flight", path, inFlight)
	case !alone && serials > 0:
		f.t.Errorf("%s started while a serial script ran, want a serial script alone", path)
	}
}

// leave records a script's end.
func (f *poolFake) leave(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	if f.serial != nil && f.serial(path) {
		f.serials--
	}
}

// ranScripts lists the scripts the fake ran, sorted, so a test compares the set
// whatever order the workers took them in.
func (f *poolFake) ranScripts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.starts...)
	sort.Strings(out)
	return out
}

// TestPoolNeverExceedsTheCap: Jobs bounds the scripts in flight, and every
// script still runs once the held ones are released. Mutation: use Jobs+1.
func TestPoolNeverExceedsTheCap(t *testing.T) {
	t.Parallel()
	const jobs = 3
	paths := scriptsIn(8)
	gates := newGates(paths)
	fake := &poolFake{
		jobs:     jobs,
		t:        t,
		hold:     gates.channel,
		started:  make(chan string, len(paths)),
		finished: make(chan string, len(paths)),
	}
	var result Result
	done := make(chan struct{})
	go func() {
		result = Run(t.Context(), poolConfig(poolEntries(paths, nil), jobs, fake.exec))
		close(done)
	}()
	t.Cleanup(func() { endRun(t, gates, done) })

	// Fill the pool: the first Jobs scripts start together and are held, so a
	// pool that runs them one at a time never reaches Jobs and fails here.
	held := make([]string, 0, jobs)
	for range jobs {
		held = append(held, waitFor(t, fake.started, "a script of the first wave to start"))
	}
	// Keep it full: each release frees a slot and the pool starts the next
	// script into it, so a pool that handed out one worker too many starts it
	// beside Jobs held scripts and the fake fails that start.
	for i := jobs; i < len(paths); i++ {
		gates.release(held[0])
		held = append(held[1:], waitFor(t, fake.started, "a script to take the freed slot"))
		waitFor(t, fake.finished, "the released script to finish")
	}
	gates.releaseAll()
	for range held {
		waitFor(t, fake.finished, "a script to finish")
	}
	waitFor(t, done, "the run to count its scripts")

	if result.Passed != len(paths) || result.Failed != 0 {
		t.Errorf("counts passed=%d failed=%d, want %d 0", result.Passed, result.Failed, len(paths))
	}
	if got := fake.ranScripts(); !slices.Equal(got, paths) {
		t.Errorf("ran %q, want each of %q once", got, paths)
	}
}

// TestPoolRunsJobsBelowOneAsOneWorker: a Config whose Jobs is unset runs one
// worker rather than a pool of no workers, which would start no script.
func TestPoolRunsJobsBelowOneAsOneWorker(t *testing.T) {
	t.Parallel()
	var cmds []Command
	r := Run(t.Context(), poolConfig(poolEntries([]string{"scripts/a_test.sh"}, nil), 0, fakeExec(nil, &cmds)))
	if r.Passed != 1 || r.Failed != 0 {
		t.Errorf("counts passed=%d failed=%d, want 1 0", r.Passed, r.Failed)
	}
	if len(cmds) != 1 {
		t.Errorf("ran %d scripts, want the one", len(cmds))
	}
}

// TestSerialEntriesRunAlone: no script runs beside a Serial one. Mutation:
// start serial entries inside the pool.
func TestSerialEntriesRunAlone(t *testing.T) {
	t.Parallel()
	paths := []string{"scripts/p0_test.sh", "scripts/s0_test.sh", "scripts/p1_test.sh"}
	const serial = "scripts/s0_test.sh"
	isSerial := func(path string) bool { return path == serial }
	gates := newGates(paths)
	fake := &poolFake{
		jobs:     2, // the two Parallel entries
		t:        t,
		serial:   isSerial,
		hold:     gates.channel,
		started:  make(chan string, len(paths)),
		finished: make(chan string, len(paths)),
	}
	var result Result
	done := make(chan struct{})
	go func() {
		result = Run(t.Context(), poolConfig(poolEntries(paths, map[string]Mode{serial: Serial}), 2, fake.exec))
		close(done)
	}()
	t.Cleanup(func() { endRun(t, gates, done) })

	// The two parallel scripts are held, so a serial script that shared their
	// workers would start beside them and the fake fails that start.
	began := make([]string, 0, 2)
	for range 2 {
		began = append(began, waitFor(t, fake.started, "a parallel script to start"))
	}
	for _, path := range began {
		gates.release(path)
	}
	if got := waitFor(t, fake.started, "the serial script to start after the pool drained"); got != serial {
		t.Errorf("started %s after the parallel drain, want the serial script", got)
	}
	gates.releaseAll()
	for range paths {
		waitFor(t, fake.finished, "a script to finish")
	}
	waitFor(t, done, "the run to count its scripts")

	if result.Passed != len(paths) {
		t.Errorf("passed=%d, want %d", result.Passed, len(paths))
	}
}

// TestResultOrderIsTheManifestOrder: a failure is named where its script was
// listed, whatever order the workers finished in.
func TestResultOrderIsTheManifestOrder(t *testing.T) {
	t.Parallel()
	paths := []string{"scripts/a_test.sh", "scripts/b_test.sh", "scripts/c_test.sh", "scripts/d_test.sh", "scripts/e_test.sh"}
	codes := map[string]int{"scripts/a_test.sh": 1, "scripts/c_test.sh": 1, "scripts/e_test.sh": 1}
	gates := newGates(paths)
	fake := &poolFake{
		jobs:     len(paths),
		t:        t,
		codes:    codes,
		hold:     gates.channel,
		started:  make(chan string, len(paths)),
		finished: make(chan string, len(paths)),
	}
	var result Result
	done := make(chan struct{})
	go func() {
		result = Run(t.Context(), poolConfig(poolEntries(paths, nil), len(paths), fake.exec))
		close(done)
	}()
	t.Cleanup(func() { endRun(t, gates, done) })

	for range paths {
		waitFor(t, fake.started, "a script to start")
	}
	// Finish in reverse manifest order: every failure finishes before the
	// failure listed ahead of it.
	for i := len(paths) - 1; i >= 0; i-- {
		gates.release(paths[i])
		waitFor(t, fake.finished, "the released script to finish")
	}
	waitFor(t, done, "the run to count its scripts")

	want := []string{"scripts/a_test.sh", "scripts/c_test.sh", "scripts/e_test.sh"}
	if !slices.Equal(result.FailedNames, want) {
		t.Errorf("FailedNames = %q, want the manifest order %q", result.FailedNames, want)
	}
	if result.Passed != 2 || result.Failed != 3 {
		t.Errorf("counts passed=%d failed=%d, want 2 3", result.Passed, result.Failed)
	}
}

// TestOneFailureDoesNotCancelTheOthers: a failed script leaves the rest to run
// and to pass.
func TestOneFailureDoesNotCancelTheOthers(t *testing.T) {
	t.Parallel()
	const failing = "scripts/fail_test.sh"
	paths := []string{failing, "scripts/b_test.sh", "scripts/c_test.sh", "scripts/d_test.sh"}
	release := make(chan struct{})
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(release) }) }
	fake := &poolFake{
		jobs:  len(paths),
		t:     t,
		codes: map[string]int{failing: 1},
		hold: func(path string) <-chan struct{} {
			if path == failing {
				return nil
			}
			return release
		},
		started:  make(chan string, len(paths)),
		finished: make(chan string, len(paths)),
	}
	var result Result
	done := make(chan struct{})
	go func() {
		result = Run(t.Context(), poolConfig(poolEntries(paths, nil), len(paths), fake.exec))
		close(done)
	}()
	t.Cleanup(func() {
		releaseAll()
		waitFor(t, done, "the run to finish once every script was released")
	})

	// Every script starts although the first fails at once: a run that canceled
	// the others on that failure would never reach this many starts.
	for range paths {
		waitFor(t, fake.started, "a script to start")
	}
	releaseAll()
	for range paths {
		waitFor(t, fake.finished, "a script to finish")
	}
	waitFor(t, done, "the run to count its scripts")

	if result.Passed != 3 || result.Failed != 1 {
		t.Errorf("counts passed=%d failed=%d, want 3 1", result.Passed, result.Failed)
	}
	if want := []string{failing}; !slices.Equal(result.FailedNames, want) {
		t.Errorf("FailedNames = %q, want %q", result.FailedNames, want)
	}
}

// TestRunNamesEachScriptItNeverRuns: a canceled run counts the scripts it never
// started as failed and names each one on the error stream, so no failure is
// counted in silence.
func TestRunNamesEachScriptItNeverRuns(t *testing.T) {
	t.Parallel()
	entries := entriesAt("scripts/a_test.sh", "scripts/b_test.sh")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var errs bytes.Buffer
	r := Run(ctx, Config{
		Entries: entries,
		Root:    "/repo",
		LogDir:  "/logs",
		Budget:  time.Minute,
		Exec: func(context.Context, Command) (int, error) {
			t.Error("a canceled run started a script")
			return 0, nil
		},
		Now:         clockAt(time.Unix(0, 0)),
		WithTimeout: liveTimeout,
		Err:         &errs,
	})
	for _, e := range entries {
		if !strings.Contains(errs.String(), e.Path+": not run") {
			t.Errorf("error stream = %q, want %s named as not run", errs.String(), e.Path)
		}
	}
	if r.Failed != len(entries) || r.TimedOut != 0 {
		t.Errorf("failed=%d timedOut=%d, want %d 0", r.Failed, r.TimedOut, len(entries))
	}
}

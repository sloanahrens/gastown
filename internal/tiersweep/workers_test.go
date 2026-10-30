package tiersweep

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// concurrentGo is the Exec a concurrency test injects. It records the commands
// in flight, sends each start on started when a test wired one, and waits on
// the channel hold names for that package before it returns.
type concurrentGo struct {
	codes   map[string]int
	output  map[string]string
	hold    func(pkg string) (chan struct{}, bool)
	started chan string
	// finished, when a test wires one, receives each package whose command
	// returned.
	finished chan string
	// jobs and t fail the run the moment one command too many starts, whatever
	// the scheduling.
	jobs int
	t    *testing.T

	mu        sync.Mutex
	inFlight  int
	max       int
	completed int
	starts    []startedCall
}

// startedCall is one Exec start: the package, the commands in flight before it
// began and the commands that had finished by then.
type startedCall struct {
	pkg      string
	inFlight int
	finished int
}

func (f *concurrentGo) exec(_ string, _ []string, argv []string, out io.Writer) int {
	pkg := argv[len(argv)-1]
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.max {
		f.max = f.inFlight
	}
	over, now, limit := f.jobs > 0 && f.inFlight > f.jobs, f.inFlight, f.jobs
	f.starts = append(f.starts, startedCall{pkg: pkg, inFlight: f.inFlight - 1, finished: f.completed})
	f.mu.Unlock()
	if over && f.t != nil {
		f.t.Errorf("%d commands in flight at once, want at most %d", now, limit)
	}
	if f.started != nil {
		f.started <- pkg
	}
	if f.hold != nil {
		if gate, ok := f.hold(pkg); ok {
			<-gate
		}
	}
	fmt.Fprint(out, f.output[pkg])
	f.mu.Lock()
	f.inFlight--
	f.completed++
	f.mu.Unlock()
	if f.finished != nil {
		f.finished <- pkg
	}
	return f.codes[pkg]
}

// maxInFlight is the most commands the fake ever saw in flight at once.
func (f *concurrentGo) maxInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.max
}

// ranPackages lists the packages the fake ran, sorted, so a test compares the
// set whatever order the workers took them in.
func (f *concurrentGo) ranPackages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pkgs []string
	for _, s := range f.starts {
		pkgs = append(pkgs, s.pkg)
	}
	sort.Strings(pkgs)
	return pkgs
}

// record is the start the fake recorded for pkg.
func (f *concurrentGo) record(pkg string) (startedCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.starts {
		if s.pkg == pkg {
			return s, true
		}
	}
	return startedCall{}, false
}

// newWorkerRunner is a Runner over exec with jobs workers, logging into a fresh
// temporary directory.
func newWorkerRunner(t *testing.T, exec Exec, jobs int) *Runner {
	t.Helper()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	return &Runner{Exec: exec, LogDir: t.TempDir(), Timeout: "7m", Stderr: io.Discard,
		Jobs: jobs, Now: func() time.Time { return at }}
}

// TestWorkersNeverExceedJobs: Jobs bounds the packages in flight, and every
// package still completes once the held ones are released.
func TestWorkersNeverExceedJobs(t *testing.T) {
	t.Parallel()
	const jobs = 3
	pkgs := workerPackages(8)
	release := make(chan struct{})
	fake := &concurrentGo{
		started: make(chan string, len(pkgs)),
		hold:    func(string) (chan struct{}, bool) { return release, true },
		jobs:    jobs,
		t:       t,
	}
	r := newWorkerRunner(t, fake.exec, jobs)

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()

	// Every command is held, so the pool is full after its first three. Let
	// the scheduler hand the other goroutines their turns before the release:
	// a pool with one worker too many starts a fourth here, while a bounded one
	// starts nothing until a slot frees.
	for i := 0; i < jobs; i++ {
		<-fake.started
	}
	for i := 0; i < releaseYields; i++ {
		runtime.Gosched()
	}
	if max := fake.maxInFlight(); max != jobs {
		t.Fatalf("commands in flight at once = %d, want %d", max, jobs)
	}
	close(release)
	got := <-finished

	if want := (Counts{Passed: len(pkgs)}); !reflect.DeepEqual(got, want) {
		t.Errorf("Tier() = %+v, want %+v", got, want)
	}
	if ran := fake.ranPackages(); !reflect.DeepEqual(ran, workerPackages(len(pkgs))) {
		t.Errorf("ran %q, want each of %q once", ran, pkgs)
	}
}

// releaseYields is how many scheduler turns a bound test gives the pool before
// it reads the commands in flight. Yielding is not a wait: a worker the pool
// started takes its turn within a few of them.
const releaseYields = 10000

// workerPackages is n package names a fake run uses, sorted.
func workerPackages(n int) []string {
	var pkgs []string
	for i := 0; i < n; i++ {
		pkgs = append(pkgs, fmt.Sprintf("./p%d", i))
	}
	return pkgs
}

// TestSerialPackagesRunAlone: a package serial.txt names starts only once no
// other command is in flight.
func TestSerialPackagesRunAlone(t *testing.T) {
	t.Parallel()
	const serial = "./internal/cmd"
	gate := make(chan struct{})
	serialGate := make(chan struct{})
	fake := &concurrentGo{
		started: make(chan string, 3),
		hold: func(pkg string) (chan struct{}, bool) {
			if pkg == serial {
				return serialGate, true
			}
			return gate, true
		},
	}
	r := newWorkerRunner(t, fake.exec, 3)
	pkgs := []string{"./a", serial, "./b"}

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()

	// The two packages beside it are held, so a serial package that shared
	// their workers would start now, with two commands in flight.
	for i := 0; i < len(pkgs)-1; i++ {
		<-fake.started
	}
	for i := 0; i < releaseYields; i++ {
		runtime.Gosched()
	}
	if _, ok := fake.record(serial); ok {
		t.Fatalf("%s started while %d commands were in flight, want it alone", serial, fake.maxInFlight())
	}
	close(gate)
	<-fake.started

	record, ok := fake.record(serial)
	if !ok {
		t.Fatal("the serial package never started")
	}
	if record.inFlight != 0 {
		t.Errorf("%s started with %d commands in flight, want alone", serial, record.inFlight)
	}
	if record.finished != len(pkgs)-1 {
		t.Errorf("%s started after %d commands finished, want %d", serial, record.finished, len(pkgs)-1)
	}
	close(serialGate)
	if got := <-finished; !reflect.DeepEqual(got, Counts{Passed: len(pkgs)}) {
		t.Errorf("Tier() = %+v, want all %d passed", got, len(pkgs))
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
		fake := &concurrentGo{codes: map[string]int{"./b": 1, "./d": 1}}
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
	fake := &concurrentGo{
		codes:    map[string]int{"./b": 1, "./e": 1},
		started:  make(chan string, 3),
		finished: make(chan string, 3),
		hold:     func(pkg string) (chan struct{}, bool) { return gate, pkg == "./b" },
	}
	r := newWorkerRunner(t, fake.exec, 3)
	pkgs := []string{"./a", "./b", "./e"}

	finished := make(chan Counts, 1)
	go func() { finished <- r.Tier(TierIntegration, ".", nil, nil, pkgs) }()

	// ./e finishes first though ./b is listed before it.
	for pkg := range fake.finished {
		if pkg == "./e" {
			break
		}
	}
	close(gate)
	got := <-finished

	want := Counts{Passed: 1, Failed: 2, FailedNames: []string{"./b", "./e"}}
	if !reflect.DeepEqual(got, want) {
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
			fake := &concurrentGo{codes: map[string]int{"./bad": 1}, output: tc.output}
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

// timingsText is a timings file's lines for walls, keyed by package.
func timingsText(walls map[string]int64) string {
	var out strings.Builder
	for _, pkg := range []string{"./a", "./b", "./c", "./d"} {
		fmt.Fprintf(&out, `{"tier":%q,"package":%q,"wall_ms":%d,"exit":0,"start":"2026-10-30T00:00:00Z"}`+"\n",
			TierIntegration, pkg, walls[pkg])
	}
	return out.String()
}

// progressPackages lists the packages a run announced, in the order it started
// them: the stderr progress lines the dispatcher prints.
func progressPackages(stderr string) []string {
	var pkgs []string
	for _, line := range strings.Split(stderr, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == Name+":" && fields[3] == "..." {
			pkgs = append(pkgs, fields[2])
		}
	}
	return pkgs
}

// TestWorkersStartLongestFirst: a run whose timings file gives walls starts the
// longest packages first, and a run without one starts them in list order.
func TestWorkersStartLongestFirst(t *testing.T) {
	t.Parallel()
	pkgs := []string{"./a", "./b", "./c", "./d"}
	walls := map[string]int64{"./a": 5000, "./b": 50000, "./c": 20000, "./d": 1000}
	cases := []struct {
		name string
		file string
		want []string
	}{
		{name: "a timings file starts the longest first", file: timingsText(walls),
			want: []string{"./b", "./c", "./a", "./d"}},
		{name: "no timings file keeps the list order", want: pkgs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := workerRun(t, pkgs, tc.file); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("start order = %q, want %q", got, tc.want)
			}
		})
	}
}

// workerRun runs the integration tier of a fake repository on four packages of
// one directory, with the timings file file when it names one, and returns the
// packages' start order.
func workerRun(t *testing.T, pkgs []string, timings string) []string {
	t.Helper()
	logDir := t.TempDir()
	env := map[string]string{EnvLogDir: logDir, EnvJobs: "3", EnvTimeout: "7m"}
	if timings != "" {
		path := filepath.Join(t.TempDir(), "timings.jsonl")
		if err := os.WriteFile(path, []byte(timings), 0o644); err != nil {
			t.Fatal(err)
		}
		env[EnvTimings] = path
	}
	repo := baseRepo()
	var files []string
	for _, pkg := range pkgs {
		name := strings.TrimPrefix(pkg, "./")
		files = append(files, name+"/x_test.go")
		repo.files[name+"/x_test.go"] = taggedTest
	}
	repo.lists[". *_test.go"] = files
	fake := &concurrentGo{}
	var stderr bytes.Buffer
	code := Run([]string{TierIntegration}, io.Discard, &stderr, Deps{
		Host: Host{
			Env:      func(name string) string { return env[name] },
			MkdirAll: func(string, fs.FileMode) error { return nil },
		},
		Exec: fake.exec,
		Src:  repo,
	})
	if code != 0 {
		t.Fatalf("Run exit = %d, want 0:\n%s", code, stderr.String())
	}
	return progressPackages(stderr.String())
}

// TestWorkersTimingsFileFallback: a timings file that is not there starts the
// packages in list order and prints nothing, and one that cannot be read prints
// one line and does the same, so the instrument never decides the verdict.
func TestWorkersTimingsFileFallback(t *testing.T) {
	t.Parallel()
	pkgs := []string{"./a", "./b"}
	cases := []struct {
		name     string
		path     func(t *testing.T) string
		wantLine bool
	}{
		{
			name: "a file that is not there is the first run",
			path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "timings.jsonl") },
		},
		{
			name:     "a file that cannot be read is named once",
			path:     func(t *testing.T) string { return t.TempDir() },
			wantLine: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tc.path(t)
			var stderr bytes.Buffer
			repo := baseRepo()
			repo.lists[". *_test.go"] = []string{"a/x_test.go", "b/x_test.go"}
			for _, f := range repo.lists[". *_test.go"] {
				repo.files[f] = taggedTest
			}
			env := map[string]string{EnvLogDir: t.TempDir(), EnvJobs: "2", EnvTimings: path}
			code := Run([]string{TierIntegration}, io.Discard, &stderr, Deps{
				Host: Host{
					Env:      func(name string) string { return env[name] },
					MkdirAll: func(string, fs.FileMode) error { return nil },
				},
				Exec: (&concurrentGo{}).exec,
				Src:  repo,
			})
			if code != 0 {
				t.Fatalf("Run exit = %d, want 0:\n%s", code, stderr.String())
			}
			if got := progressPackages(stderr.String()); !reflect.DeepEqual(got, pkgs) {
				t.Errorf("start order = %q, want the list order %q", got, pkgs)
			}
			named := strings.Contains(stderr.String(), "timings: read "+path)
			if named != tc.wantLine {
				t.Errorf("stderr names the timings file = %t, want %t:\n%s", named, tc.wantLine, stderr.String())
			}
		})
	}
}

// TestSerialListNamesOnlyDiscoveredPackages: every package serial.txt names has
// an integration-tagged test where discovery looks for one, so a renamed or
// deleted package cannot silently drop out of the list that keeps it alone.
func TestSerialListNamesOnlyDiscoveredPackages(t *testing.T) {
	t.Parallel()
	for _, pkg := range SerialPackages() {
		dir := filepath.Join("..", "..", pkg)
		if !discoversIntegrationTest(dir) {
			t.Errorf("serial.txt names %s, which the sweep does not discover: no integration-tagged test under %s", pkg, pkg)
		}
	}
	// The check fails a package that has only a plain test.
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "x_test.go"), []byte(plainTest), 0o644); err != nil {
		t.Fatal(err)
	}
	if discoversIntegrationTest(plain) {
		t.Error("a package with only a plain test reads as discovered")
	}
}

// discoversIntegrationTest reports whether dir holds a test file the
// integration build tag selects, read through the discovery the sweep uses.
func discoversIntegrationTest(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	return len(TaggedDirs(files, func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, name))
	})) > 0
}

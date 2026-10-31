package tiersweep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// jsonRun renders a go test -json stream: one event per line, in the order a
// run reports them, as cmd/test2json writes it.
func jsonRun(t *testing.T, events ...testEvent) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal %+v: %v", ev, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// jsonFlags counts the -json arguments in one command line.
func jsonFlags(argv []string) int {
	n := 0
	for _, a := range argv {
		if a == "-json" {
			n++
		}
	}
	return n
}

// goTestCall is the command line the Sweep ran pkg with in dir.
func goTestCall(t *testing.T, fake *fakeTier, dir, pkg string) []string {
	t.Helper()
	for _, c := range fake.calls {
		if c.dir == dir && c.argv[0] == "gt" && c.argv[len(c.argv)-1] == pkg {
			return c.argv
		}
	}
	t.Fatalf("no go test call for %s in %s: %v", pkg, dir, fake.calls)
	return nil
}

// printingTier is fakeTier with the text each go test call prints, keyed by
// "dir pkg": the fake is asked for neither go test's output nor its flags, so a
// test can hand a package the stream a -json run reports or the plain text a run
// prints.
func printingTier(f *fakeTier, prints map[string]string) Exec {
	return func(dir string, env []string, argv []string, w io.Writer) int {
		if argv[0] == "gt" {
			fmt.Fprint(w, prints[dir+" "+argv[len(argv)-1]])
		}
		return f.exec(dir, env, argv, w)
	}
}

// readKind reads a timings file's lines whose kind is kind, so a test can name
// the slow-test lines apart from the package lines beside them.
func readKind(t *testing.T, path, kind string) []TestTiming {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got []TestTiming
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var tt TestTiming
		if err := json.Unmarshal([]byte(line), &tt); err != nil {
			t.Fatalf("line %q of %s: %v", line, path, err)
		}
		if tt.Kind == kind {
			got = append(got, tt)
		}
	}
	return got
}

// TestIntegrationRunsWithJSON: the integration run of a host-side package, the
// root's and a nested module's, reads go test's -json stream, and the container
// list and the race run do not, since their logs and their times are not the
// host sweep's timings.
func TestIntegrationRunsWithJSON(t *testing.T) {
	t.Parallel()
	repo := baseRepo()
	setTracker(repo)
	oneTagged(repo)
	repo.files[dockerList] = "internal/beads\n"
	fake := &fakeTier{}
	s, _ := newIntegration(t, repo, fake, "", "")
	if _, red := s.Integration(); red {
		t.Fatal("integration run is RED, want GREEN")
	}

	root := goTestCall(t, fake, ".", "./r")
	if got := jsonFlags(root); got != 1 {
		t.Errorf("integration argv %v has -json %d times, want once", root, got)
	}
	module := goTestCall(t, fake, "tracker", "./store/pgtest")
	if got := jsonFlags(module); got != 1 {
		t.Errorf("nested module argv %v has -json %d times, want once", module, got)
	}
	docker := goTestCall(t, fake, ".", "./internal/beads")
	if got := jsonFlags(docker); got != 0 {
		t.Errorf("container argv %v has -json %d times, want none", docker, got)
	}

	race := &fakeTier{}
	rs := raceSweep(t, race)
	rs.Race()
	if len(race.calls) != 1 {
		t.Fatalf("race ran %d commands, want 1: %v", len(race.calls), race.calls)
	}
	if got := jsonFlags(race.calls[0].argv); got != 0 {
		t.Errorf("race argv %v has -json %d times, want none", race.calls[0].argv, got)
	}
}

// TestSlowTestsReachTimings: a -json run writes one test line for each top-level
// test that took the threshold or longer, with the package's counted name, the
// test's name and its elapsed time, and no line for a faster test or for a
// subtest. The package's own wall stays the only wall ParseWalls reads.
func TestSlowTestsReachTimings(t *testing.T) {
	t.Parallel()
	const pkg = "./internal/slow"
	logDir := t.TempDir()
	stderr := &bytes.Buffer{}
	stream := jsonRun(t,
		testEvent{Action: "run", Package: pkg, Test: "TestFast"},
		testEvent{Action: "pass", Package: pkg, Test: "TestFast", Elapsed: 1},
		testEvent{Action: "run", Package: pkg, Test: "TestAtThreshold"},
		testEvent{Action: "pass", Package: pkg, Test: "TestAtThreshold", Elapsed: 5},
		testEvent{Action: "run", Package: pkg, Test: "TestSlow"},
		testEvent{Action: "output", Package: pkg, Test: "TestSlow", Output: "    --- FAIL: TestSlow/sub (30.00s)\n"},
		testEvent{Action: "fail", Package: pkg, Test: "TestSlow/sub", Elapsed: 30},
		testEvent{Action: "fail", Package: pkg, Test: "TestSlow", Elapsed: 31},
		testEvent{Action: "fail", Package: pkg, Elapsed: 37.5},
	)
	fake := &fakeGo{
		codes:  map[string]int{pkg: 1},
		output: map[string]string{pkg: stream},
	}
	clock := &fakeClock{at: time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)}
	r := &Runner{
		Exec: func(dir string, env []string, argv []string, out io.Writer) int {
			// The package's own run, not its tests, is what moves the clock.
			clock.at = clock.at.Add(42 * time.Second)
			return fake.exec(dir, env, argv, out)
		},
		LogDir:  logDir,
		Timeout: "7m",
		Stderr:  stderr,
		JSON:    true,
		Now:     clock.now,
		Timings: OpenTimings(filepath.Join(logDir, TimingsName), ProcessHost, stderr),
	}
	r.Tier(TierIntegration, ".", nil, nil, []string{pkg})
	r.Timings.Close()

	want := []TestTiming{
		{Kind: kindTest, Tier: TierIntegration, Package: pkg, Test: "TestAtThreshold", ElapsedMS: 5000},
		{Kind: kindTest, Tier: TierIntegration, Package: pkg, Test: "TestSlow", ElapsedMS: 31000},
	}
	if got := readKind(t, filepath.Join(logDir, TimingsName), kindTest); !reflect.DeepEqual(got, want) {
		t.Errorf("test timings = %+v, want %+v", got, want)
	}

	data, err := os.ReadFile(filepath.Join(logDir, TimingsName))
	if err != nil {
		t.Fatal(err)
	}
	wantWalls := map[string]int64{TimingsKey(TierIntegration, pkg): 42000}
	if got := ParseWalls(data); !reflect.DeepEqual(got, wantWalls) {
		t.Errorf("ParseWalls over the run's file = %v, want the package's own wall %v", got, wantWalls)
	}
}

// passingRunText is what go test prints for one passing test in ./r, and
// passingRunStream the same run as go test -json reports it: every byte of the
// text is an Output event, and the test's own event carries its time.
const passingRunText = "=== RUN   TestA\n--- PASS: TestA (1.02s)\nPASS\nok  \t./r\t1.030s\n"

// failingRunText is the same run with a failing test, which no longer passes.
const failingRunText = "=== RUN   TestB\n--- FAIL: TestB (0.01s)\nFAIL\nFAIL\t./r\t0.012s\n"

// buildFailure is what go prints when the package does not compile: no stream,
// since the test binary never ran.
const buildFailure = "# ./r\n./r/x_test.go:3:2: undefined: foo\nFAIL\t./r [build failed]\n"

// passingRunStream is passingRunText's stream, and failingRunStream
// failingRunText's.
func passingRunStream(t *testing.T) string {
	return jsonRun(t,
		testEvent{Action: "start", Package: "./r"},
		testEvent{Action: "run", Package: "./r", Test: "TestA"},
		testEvent{Action: "output", Package: "./r", Test: "TestA", Output: "=== RUN   TestA\n"},
		testEvent{Action: "output", Package: "./r", Test: "TestA", Output: "--- PASS: TestA (1.02s)\n"},
		testEvent{Action: "pass", Package: "./r", Test: "TestA", Elapsed: 1.02},
		testEvent{Action: "output", Package: "./r", Output: "PASS\n"},
		testEvent{Action: "output", Package: "./r", Output: "ok  \t./r\t1.030s\n"},
		testEvent{Action: "pass", Package: "./r", Elapsed: 1.03},
	)
}

// notAStream is buildFailure as the fake prints it: the same text, since the
// build failed before test2json had a stream to write.
func notAStream(*testing.T) string { return buildFailure }

func failingRunStream(t *testing.T) string {
	return jsonRun(t,
		testEvent{Action: "run", Package: "./r", Test: "TestB"},
		testEvent{Action: "output", Package: "./r", Test: "TestB", Output: "=== RUN   TestB\n"},
		testEvent{Action: "output", Package: "./r", Test: "TestB", Output: "--- FAIL: TestB (0.01s)\n"},
		testEvent{Action: "fail", Package: "./r", Test: "TestB", Elapsed: 0.01},
		testEvent{Action: "output", Package: "./r", Output: "FAIL\n"},
		testEvent{Action: "output", Package: "./r", Output: "FAIL\t./r\t0.012s\n"},
		testEvent{Action: "fail", Package: "./r", Elapsed: 0.02},
	)
}

// TestBrokenStreamFailsThePackage: a -json stream that cannot be read leaves no
// log to vouch for what ran, so the package is failed even though its command
// exited zero, and the log it could not fill is named on stderr.
func TestBrokenStreamFailsThePackage(t *testing.T) {
	t.Parallel()
	const pkg = "./internal/broken"
	stderr := &bytes.Buffer{}
	exec := func(_ string, _ []string, _ []string, _ io.Writer) int { return 0 }
	stream := func() (io.ReadCloser, io.WriteCloser) {
		pr, pw := io.Pipe()
		_ = pw.CloseWithError(errors.New("copy from the child failed"))
		return pr, pw
	}
	r := &Runner{Exec: exec, LogDir: t.TempDir(), Timeout: "7m", Stderr: stderr, JSON: true, Stream: stream}
	counts := r.Tier(TierIntegration, ".", nil, nil, []string{pkg})

	if want := (Counts{Failed: 1, FailedNames: []string{pkg}}); !reflect.DeepEqual(counts, want) {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	logLine := "    log " + filepath.Join(r.LogDir, LogName(TierIntegration, pkg))
	if !strings.Contains(stderr.String(), logLine) {
		t.Errorf("stderr = %q, want it to name %q", stderr, logLine)
	}
}

// unreadable is the read half of a stream whose capture has given up: every
// read fails while the write half stays open, and close is the pipe's.
type unreadable struct{ *io.PipeReader }

func (unreadable) Read([]byte) (int, error) { return 0, errors.New("copy from the child failed") }

// TestCaptureFailureReleasesTheCommand: when a -json capture gives up, a command
// still writing gets its write refused, so the package ends rather than hanging
// on a pipe nothing reads.
func TestCaptureFailureReleasesTheCommand(t *testing.T) {
	t.Parallel()
	const pkg = "./internal/stuck"
	wrote := make(chan error, 1)
	exec := func(_ string, _ []string, _ []string, out io.Writer) int {
		_, err := out.Write([]byte("{\"Action\":\"output\"}\n"))
		wrote <- err
		return 0
	}
	stream := func() (io.ReadCloser, io.WriteCloser) {
		pr, pw := io.Pipe()
		return unreadable{pr}, pw
	}
	r := &Runner{Exec: exec, LogDir: t.TempDir(), Timeout: "7m", Stderr: &bytes.Buffer{}, JSON: true, Stream: stream}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		r.Tier(TierIntegration, ".", nil, nil, []string{pkg})
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the package did not finish: its command blocked writing a stream nothing reads")
	}
	if err := <-wrote; err == nil {
		t.Error("the command's write into the abandoned stream succeeded, want an error")
	}
}

// failingLog is the log a Runner is given by the write-failure test: every write
// fails the way a full disk's does, and the close succeeds.
type failingLog struct{}

func (failingLog) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }
func (failingLog) Close() error              { return nil }

// TestUnwritableJSONLogFailsThePackage: a -json stream whose plain text cannot
// be written to the log fails the package even though its command exited zero,
// since the log cannot vouch for what ran.
func TestUnwritableJSONLogFailsThePackage(t *testing.T) {
	t.Parallel()
	const pkg = "./internal/nolog"
	stderr := &bytes.Buffer{}
	stream := jsonRun(t,
		testEvent{Action: "run", Package: pkg, Test: "TestA"},
		testEvent{Action: "pass", Package: pkg, Test: "TestA", Elapsed: 1},
	)
	r := &Runner{
		Exec:      (&fakeGo{output: map[string]string{pkg: stream}}).exec,
		CreateLog: func(string) (io.WriteCloser, error) { return failingLog{}, nil },
		LogDir:    t.TempDir(),
		Timeout:   "7m",
		Stderr:    stderr,
		JSON:      true,
	}
	counts := r.Tier(TierIntegration, ".", nil, nil, []string{pkg})

	if want := (Counts{Failed: 1, FailedNames: []string{pkg}}); !reflect.DeepEqual(counts, want) {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	if got := stderr.String(); !strings.Contains(got, "capture -json output: no space left on device") {
		t.Errorf("stderr = %q, want the write failure named", got)
	}
}

// TestJSONRunKeepsTheLogAndSummary: the log of a -json run is the plain text the
// same run prints, a failing stream still leaves the failure excerpt, and the
// summary line and verdict are byte for byte those of a run that kept go test's
// own output.
func TestJSONRunKeepsTheLogAndSummary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		stream      func(*testing.T) string
		plain       string
		code        int
		wantExcerpt string
	}{
		{name: "a passing package", stream: passingRunStream, plain: passingRunText},
		{name: "a failing package", stream: failingRunStream, plain: failingRunText, code: 1,
			wantExcerpt: "    --- FAIL: TestB (0.01s)"},
		// go failed before the test binary ran, so the output is not a stream and
		// is written to the log as it came.
		{name: "output that is not a stream", stream: notAStream, plain: buildFailure,
			code: 1, wantExcerpt: "    FAIL\t./r [build failed]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const pkg = "./r"
			logDir := t.TempDir()
			fake := &fakeTier{codes: map[string]int{". " + pkg: tc.code}}
			exec := printingTier(fake, map[string]string{". " + pkg: tc.stream(t)})
			stderr := &bytes.Buffer{}
			at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
			s := &Sweep{
				Opts:    Options{Timeout: "7m", LogDir: logDir},
				Exec:    exec,
				Src:     taggedRepo([]string{pkg}),
				Environ: []string{"PATH=/fake/bin"},
				Stderr:  stderr,
				Now:     func() time.Time { return at },
			}
			line, red := s.Integration()

			log, err := os.ReadFile(filepath.Join(logDir, LogName(TierIntegration, pkg)))
			if err != nil {
				t.Fatalf("read the package's log: %v", err)
			}
			if string(log) != tc.plain {
				t.Errorf("log = %q, want the plain text %q", log, tc.plain)
			}
			excerpt := strings.Join(stderrLines(stderr), "\n")
			switch {
			case tc.wantExcerpt == "" && excerpt != "":
				t.Errorf("stderr shows the excerpt %q, want none", excerpt)
			case tc.wantExcerpt != "" && !strings.Contains(excerpt, tc.wantExcerpt):
				t.Errorf("stderr excerpt = %q, want %q", excerpt, tc.wantExcerpt)
			}

			// The same package through the run that keeps go test's own output,
			// in the same log directory, so the two summary lines differ only if
			// the -json run changed a count or the verdict.
			plainFake := &fakeGo{codes: map[string]int{pkg: tc.code}, output: map[string]string{pkg: tc.plain}}
			pr := &Runner{Exec: plainFake.exec, LogDir: logDir, Timeout: "7m", Stderr: io.Discard}
			counts := pr.Tier(TierIntegration, ".", nil, nil, []string{pkg})
			want, wantRed := Summary(TierIntegration, logDir, counts.Passed, counts.Failed,
				counts.Skipped, strings.Join(counts.FailedNames, " "), 0)
			if line != want || red != wantRed {
				t.Errorf("summary = %q (RED %v), want the non-JSON run's %q (RED %v)", line, red, want, wantRed)
			}
		})
	}
}

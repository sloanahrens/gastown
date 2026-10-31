package tiersweep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fixturePkg is the package the shard tests run, and fixtureTests the ten tests
// its test binary lists.
const fixturePkg = "./fixture"

var fixtureTests = []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG", "TestH", "TestI", "TestJ"}

// shardFake is the Exec a shard test injects. The build succeeds, -test.list
// prints the listed tests, and a -test.run command reports each listed test its
// pattern selects, as a test binary does with -test.v=test2json. A dropped test
// is never reported; a failing one is reported as failed, and its shard exits 1.
// buildFails and listFails make the build or the list exit 1 instead. Every
// command is recorded with the directory it ran in.
type shardFake struct {
	mu         sync.Mutex
	listed     []string
	dropped    map[string]bool
	failing    map[string]bool
	buildFails bool
	listFails  bool
	patterns   []string
	calls      []shardCall
}

// shardCall is one command the fake was asked to run, and where it ran.
type shardCall struct {
	dir  string
	argv []string
}

func (f *shardFake) exec(dir string, _ []string, argv []string, out io.Writer) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, shardCall{dir: dir, argv: argv})
	switch {
	case slices.Contains(argv, "-c"):
		if f.buildFails {
			fmt.Fprintln(out, "build broke")
			return 1
		}
		return 0
	case slices.Contains(argv, "-test.list"):
		if f.listFails {
			fmt.Fprintln(out, "list broke")
			return 1
		}
		for _, n := range f.listed {
			fmt.Fprintln(out, n)
		}
		return 0
	case slices.Contains(argv, "-test.run"):
		pattern := argv[slices.Index(argv, "-test.run")+1]
		f.patterns = append(f.patterns, pattern)
		selects := regexp.MustCompile(pattern)
		code := 0
		for _, n := range f.listed {
			if !selects.MatchString(n) || f.dropped[n] {
				continue
			}
			writeShardEvent(out, testEvent{Action: "run", Test: n})
			if f.failing[n] {
				code = 1
				writeShardEvent(out, testEvent{Action: "output", Test: n, Output: "--- FAIL: " + n + " (0.00s)\n"})
				writeShardEvent(out, testEvent{Action: "fail", Test: n, Elapsed: 0.01})
				continue
			}
			writeShardEvent(out, testEvent{Action: "output", Test: n, Output: "--- PASS: " + n + " (0.00s)\n"})
			writeShardEvent(out, testEvent{Action: "pass", Test: n, Elapsed: 0.01})
		}
		return code
	case slices.Contains(argv, "go"):
		// A package run whole, which passes.
		return 0
	}
	return 1
}

// writeShardEvent writes one line of a test2json stream. A testEvent of strings
// and numbers always marshals, so the error is not checked.
func writeShardEvent(out io.Writer, ev testEvent) {
	line, _ := json.Marshal(ev)
	fmt.Fprintf(out, "%s\n", line)
}

// shardRunner is a Runner over f that logs into a fresh directory and runs the
// fixture package as three shards.
func shardRunner(t *testing.T, f *shardFake, stderr *bytes.Buffer) *Runner {
	t.Helper()
	return &Runner{
		Exec:    f.exec,
		LogDir:  t.TempDir(),
		Timeout: "7m",
		Stderr:  stderr,
		Shards:  map[string]int{fixturePkg: 3},
	}
}

// readShardLog reads the fixture package's one log from logDir.
func readShardLog(t *testing.T, logDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, LogName(TierIntegration, fixturePkg)))
	if err != nil {
		t.Fatalf("read the package log: %v", err)
	}
	return string(data)
}

// TestShardedPackageRunsEveryListedTestOnce: a fixture package with ten listed
// tests and K=3 runs three shard commands whose -test.run patterns select each
// test exactly once, and the package counts as one pass.
func TestShardedPackageRunsEveryListedTestOnce(t *testing.T) {
	t.Parallel()
	f := &shardFake{listed: fixtureTests}
	var stderr bytes.Buffer
	r := shardRunner(t, f, &stderr)
	counts := r.Tier(TierIntegration, ".", nil, taggedArgs, []string{fixturePkg})
	if counts.Passed != 1 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want one package passed; stderr:\n%s", counts, stderr.String())
	}
	if len(f.patterns) != 3 {
		t.Fatalf("shard runs = %d, want 3: %q", len(f.patterns), f.patterns)
	}
	for _, n := range fixtureTests {
		hits := 0
		for _, p := range f.patterns {
			if regexp.MustCompile(p).MatchString(n) {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("%s selected by %d shard patterns, want 1: %q", n, hits, f.patterns)
		}
	}
}

// TestShardedPackageRunsOnThePool: with several jobs a sharded package's shards
// join the pool beside a package run whole, and the sharded package still counts
// once, with its shards' results merged into its one log.
func TestShardedPackageRunsOnThePool(t *testing.T) {
	t.Parallel()
	f := &shardFake{listed: fixtureTests}
	var stderr bytes.Buffer
	r := shardRunner(t, f, &stderr)
	r.Jobs = 3
	counts := r.Tier(TierIntegration, ".", nil, taggedArgs, []string{"./plain", fixturePkg})
	if counts.Passed != 2 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want two packages passed; stderr:\n%s", counts, stderr.String())
	}
	if len(f.patterns) != 3 {
		t.Fatalf("shard runs = %d, want 3: %q", len(f.patterns), f.patterns)
	}
	if log := readShardLog(t, r.LogDir); !strings.Contains(log, "--- PASS: TestJ") {
		t.Errorf("log lacks the last shard's output:\n%s", log)
	}
}

// TestShardCoverageFailureIsRed: a shard that never reports one listed test
// makes the package RED, and the missing name is in the failure text.
func TestShardCoverageFailureIsRed(t *testing.T) {
	t.Parallel()
	f := &shardFake{listed: fixtureTests, dropped: map[string]bool{"TestD": true}}
	var stderr bytes.Buffer
	r := shardRunner(t, f, &stderr)
	counts := r.Tier(TierIntegration, ".", nil, taggedArgs, []string{fixturePkg})
	if counts.Failed != 1 || !slices.Equal(counts.FailedNames, []string{fixturePkg}) {
		t.Fatalf("counts = %+v, want the package failed; stderr:\n%s", counts, stderr.String())
	}
	if !strings.Contains(stderr.String(), "TestD") {
		t.Errorf("stderr does not name the test that did not run:\n%s", stderr.String())
	}
	if log := readShardLog(t, r.LogDir); !strings.Contains(log, "did not run: TestD") {
		t.Errorf("log does not name the test that did not run:\n%s", log)
	}
}

// TestOneFailingShardFailsThePackage: one failing shard fails the package, its
// failure excerpt is kept, and the other shards' output is still in the log.
func TestOneFailingShardFailsThePackage(t *testing.T) {
	t.Parallel()
	f := &shardFake{listed: fixtureTests, failing: map[string]bool{"TestE": true}}
	var stderr bytes.Buffer
	r := shardRunner(t, f, &stderr)
	counts := r.Tier(TierIntegration, ".", nil, taggedArgs, []string{fixturePkg})
	if counts.Failed != 1 {
		t.Fatalf("counts = %+v, want the package failed; stderr:\n%s", counts, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--- FAIL: TestE") {
		t.Errorf("excerpt does not keep the failure:\n%s", stderr.String())
	}
	log := readShardLog(t, r.LogDir)
	for _, n := range fixtureTests {
		want := "--- PASS: " + n
		if n == "TestE" {
			want = "--- FAIL: TestE"
		}
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q: the other shards' output is not merged:\n%s", want, log)
		}
	}
}

// TestShardedPackageWritesOneLinePerShard: a sharded package writes one timings
// line for each of its shards, each naming its place, not one for the package.
func TestShardedPackageWritesOneLinePerShard(t *testing.T) {
	t.Parallel()
	f := &shardFake{listed: fixtureTests}
	var stderr bytes.Buffer
	r := shardRunner(t, f, &stderr)
	path := filepath.Join(t.TempDir(), TimingsName)
	timings := OpenTimings(path, ProcessHost, &stderr)
	r.Timings = timings
	r.Tier(TierIntegration, ".", nil, taggedArgs, []string{fixturePkg})
	timings.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read timings: %v", err)
	}
	var places []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var tm Timing
		if err := json.Unmarshal([]byte(line), &tm); err != nil {
			t.Fatalf("timings line %q: %v", line, err)
		}
		if tm.Kind == kindShard && tm.Package == fixturePkg {
			places = append(places, tm.Shard)
		}
	}
	if want := []string{"1/3", "2/3", "3/3"}; !slices.Equal(places, want) {
		t.Errorf("shard lines = %q, want %q; timings:\n%s", places, want, data)
	}
}

// TestListedTests: only top-level Test names are shards' work, in listed order.
func TestListedTests(t *testing.T) {
	t.Parallel()
	text := "TestA\nTestlower\nBenchmarkX\nExampleY\nTest\nTestB2\n  TestC  \n"
	if got, want := ListedTests(text), []string{"TestA", "Test", "TestB2", "TestC"}; !slices.Equal(got, want) {
		t.Errorf("ListedTests = %q, want %q", got, want)
	}
}

// TestParseShards: a plan reads one "./package K" per line, and any other shape
// is an error, so a plan that cannot be read runs nothing.
func TestParseShards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		want    map[string]int
		wantErr bool
	}{
		{name: "empty", text: "", want: map[string]int{}},
		{name: "comment only", text: "# ./internal/cmd 3\n", want: map[string]int{}},
		{name: "entry with comment", text: "./internal/cmd 3 # the seed\n", want: map[string]int{"./internal/cmd": 3}},
		{name: "no count", text: "./internal/cmd\n", wantErr: true},
		{name: "no ./ prefix", text: "internal/cmd 3\n", wantErr: true},
		{name: "zero count", text: "./internal/cmd 0\n", wantErr: true},
		{name: "count not a number", text: "./internal/cmd three\n", wantErr: true},
		{name: "named twice", text: "./internal/cmd 3\n./internal/cmd 2\n", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseShards(c.text)
			if (err != nil) != c.wantErr {
				t.Fatalf("ParseShards(%q) error = %v, want error %v", c.text, err, c.wantErr)
			}
			if !c.wantErr && !maps.Equal(got, c.want) {
				t.Errorf("ParseShards(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

// TestShardPrepFailureIsRed: a build that fails, a list that fails, or a binary
// that lists no tests makes the package RED before any shard runs, and the log
// names the step that failed.
func TestShardPrepFailureIsRed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(f *shardFake)
		want  string
	}{
		{
			name:  "the build fails",
			setup: func(f *shardFake) { f.buildFails = true },
			want:  "build the test binary: exit 1",
		},
		{
			name:  "the list fails",
			setup: func(f *shardFake) { f.listFails = true },
			want:  "list the tests: exit 1",
		},
		{
			name:  "the binary lists no tests",
			setup: func(f *shardFake) { f.listed = nil },
			want:  "the test binary lists no tests",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &shardFake{listed: fixtureTests}
			tc.setup(f)
			var stderr bytes.Buffer
			r := shardRunner(t, f, &stderr)
			counts := r.Tier(TierIntegration, ".", nil, taggedArgs, []string{fixturePkg})
			if counts.Failed != 1 || !slices.Equal(counts.FailedNames, []string{fixturePkg}) {
				t.Fatalf("counts = %+v, want the package failed; stderr:\n%s", counts, stderr.String())
			}
			if len(f.patterns) != 0 {
				t.Errorf("shards ran %q after the step failed", f.patterns)
			}
			if log := readShardLog(t, r.LogDir); !strings.Contains(log, tc.want) {
				t.Errorf("log does not name the failed step %q:\n%s", tc.want, log)
			}
		})
	}
}

// TestShardsRunInThePackageDirectory: the build runs in the tier's directory, and
// the list and every shard run in the package's own directory, as go test runs
// a test binary there. A nested module's package is the module's path joined.
func TestShardsRunInThePackageDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, dir, pkg, printed, pkgDir string
	}{
		{name: "the root", dir: ".", pkg: fixturePkg, printed: fixturePkg, pkgDir: "fixture"},
		{name: "a nested module", dir: "tracker", pkg: "./store/pgtest", printed: "./tracker/store/pgtest", pkgDir: "tracker/store/pgtest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &shardFake{listed: fixtureTests}
			var stderr bytes.Buffer
			r := shardRunner(t, f, &stderr)
			r.Shards = map[string]int{tc.printed: 3}
			r.Tier(TierIntegration, tc.dir, nil, taggedArgs, []string{tc.pkg})
			var builds, lists, shards int
			for _, c := range f.calls {
				switch {
				case slices.Contains(c.argv, "-c"):
					builds++
					if c.dir != tc.dir {
						t.Errorf("build ran in %q, want %q", c.dir, tc.dir)
					}
				case slices.Contains(c.argv, "-test.list"):
					lists++
					if c.dir != tc.pkgDir {
						t.Errorf("list ran in %q, want %q", c.dir, tc.pkgDir)
					}
				case slices.Contains(c.argv, "-test.run"):
					shards++
					if c.dir != tc.pkgDir {
						t.Errorf("shard ran in %q, want %q", c.dir, tc.pkgDir)
					}
				}
			}
			if builds != 1 || lists != 1 || shards != 3 {
				t.Fatalf("builds, lists, shards = %d, %d, %d; want 1, 1, 3", builds, lists, shards)
			}
		})
	}
}

// TestUnreadableShardPlanIsRed: a shard plan that cannot be read runs nothing,
// and the integration tier is RED with no names, as an unreadable list is.
func TestUnreadableShardPlanIsRed(t *testing.T) {
	t.Parallel()
	repo := baseRepo()
	repo.lists[". *_test.go"] = []string{"x/x_test.go"}
	repo.files["x/x_test.go"] = taggedTest
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, repo, fake, "", "")
	s.ShardErr = errors.New("shard.txt line 1: want a ./package name and a shard count")
	line, red := s.Integration()
	if !red || !strings.Contains(line, "integration RED passed=0 failed=0 skipped=0") {
		t.Errorf("Integration() = %q, %v; want a RED summary with no names", line, red)
	}
	if !strings.Contains(stderr.String(), "shard plan") {
		t.Errorf("stderr = %q, want it to name the shard plan", stderr.String())
	}
	if runs := fake.testRuns(); len(runs) != 0 {
		t.Errorf("go test runs = %v, want none", runs)
	}
}

// TestShardPlanNamingAContainerPackageRunsItWhole: the container tier does not
// shard, so a shard.txt entry for a container package runs it whole and says so
// on stderr, rather than dropping the entry without a word.
func TestShardPlanNamingAContainerPackageRunsItWhole(t *testing.T) {
	t.Parallel()
	repo := baseRepo()
	repo.files[dockerList] = "internal/beads\n"
	fake := &fakeTier{head: "abc123"}
	s, stderr := newIntegration(t, repo, fake, "", "")
	s.Shards = map[string]int{"./internal/beads": 3}
	s.Integration()
	if !strings.Contains(stderr.String(), "./internal/beads runs whole") {
		t.Errorf("stderr = %q, want it to name the container package that runs whole", stderr.String())
	}
	var runs int
	for _, c := range fake.calls {
		if c.argv[0] == "gt" && c.argv[len(c.argv)-1] == "./internal/beads" {
			runs++
			if slices.Contains(c.argv, "-test.run") || slices.Contains(c.argv, "-c") {
				t.Errorf("container package ran sharded: %q", c.argv)
			}
		}
	}
	if runs != 1 {
		t.Errorf("container package runs = %d, want 1 whole run", runs)
	}
}

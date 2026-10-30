package tiersweep

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeGo is the Exec a test injects. It records each call and writes the
// scripted output and returns the scripted code for the package the call names,
// which is the command's last argument. A package with no script passes.
type fakeGo struct {
	codes  map[string]int
	output map[string]string
	calls  []fakeCall
}

// fakeCall is one Exec call: the directory it ran in and the command line.
type fakeCall struct {
	dir  string
	argv []string
}

func (f *fakeGo) exec(dir string, _ []string, argv []string, out io.Writer) int {
	f.calls = append(f.calls, fakeCall{dir: dir, argv: argv})
	pkg := argv[len(argv)-1]
	fmt.Fprint(out, f.output[pkg])
	return f.codes[pkg]
}

// ranPackages lists the package each recorded call named, in order.
func (f *fakeGo) ranPackages() []string {
	var pkgs []string
	for _, c := range f.calls {
		pkgs = append(pkgs, c.argv[len(c.argv)-1])
	}
	return pkgs
}

// newRunner is a Runner over fake, logging into a fresh temporary directory
// and writing its stderr to the returned buffer.
func newRunner(t *testing.T, fake *fakeGo, skipList string) (*Runner, *bytes.Buffer, string) {
	t.Helper()
	logDir := t.TempDir()
	stderr := &bytes.Buffer{}
	return &Runner{
		Exec:     fake.exec,
		LogDir:   logDir,
		Timeout:  "7m",
		SkipList: skipList,
		Stderr:   stderr,
	}, stderr, logDir
}

// stderrLines returns the lines the Runner wrote that begin with four spaces:
// the failure excerpts, not the progress lines.
func stderrLines(stderr *bytes.Buffer) []string {
	var lines []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, "    ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestTierCountsAndNames: a package's counted name is the one a failure reports,
// so a nested module's package reads ./tracker/store/pgtest and its root ./tracker.
func TestTierCountsAndNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		dir        string
		pkgs       []string
		codes      map[string]int
		want       Counts
		wantStderr string
	}{
		{
			name:       "two pass and one fails in the root module",
			dir:        ".",
			pkgs:       []string{"./internal/a", "./internal/b", "./internal/c"},
			codes:      map[string]int{"./internal/c": 1},
			want:       Counts{Passed: 2, Failed: 1, FailedNames: []string{"./internal/c"}},
			wantStderr: "tier-sweep: integration ./internal/c ...\n",
		},
		{
			name:       "a nested module names its packages under the module dir",
			dir:        "tracker",
			pkgs:       []string{"./", "./store/pgtest"},
			codes:      map[string]int{"./store/pgtest": 1},
			want:       Counts{Passed: 1, Failed: 1, FailedNames: []string{"./tracker/store/pgtest"}},
			wantStderr: "tier-sweep: integration ./tracker ...\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeGo{codes: tc.codes}
			r, stderr, _ := newRunner(t, fake, "")
			got := r.Tier(TierIntegration, tc.dir, nil, nil, tc.pkgs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Tier() = %+v, want %+v", got, tc.want)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr has no %q line:\n%s", tc.wantStderr, stderr)
			}
		})
	}
}

// TestTierCommandLine: the command is prefix, go test -count=1 -timeout, args,
// the optional -skip, then the package, and it runs in the module's directory.
func TestTierCommandLine(t *testing.T) {
	t.Parallel()
	prefix := []string{"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1"}
	cases := []struct {
		name     string
		dir      string
		prefix   []string
		args     []string
		skipList string
		pkg      string
		wantDir  string
		want     []string
	}{
		{
			name:    "prefix and args, no skip",
			dir:     ".",
			prefix:  prefix,
			args:    []string{"-tags", "integration"},
			pkg:     "./internal/cmd",
			wantDir: ".",
			want: []string{"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1",
				"go", "test", "-count=1", "-timeout", "7m", "-tags", "integration", "./internal/cmd"},
		},
		{
			name:     "a regexp skip follows the args and precedes the package",
			dir:      ".",
			prefix:   prefix,
			args:     []string{"-tags", "integration"},
			skipList: "integration:./internal/cmd:TestSlow",
			pkg:      "./internal/cmd",
			wantDir:  ".",
			want: []string{"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1",
				"go", "test", "-count=1", "-timeout", "7m", "-tags", "integration", "-skip", "TestSlow", "./internal/cmd"},
		},
		{
			name:    "a nested module runs in its own directory",
			dir:     "tracker",
			pkg:     "./store",
			wantDir: "tracker",
			want:    []string{"go", "test", "-count=1", "-timeout", "7m", "./store"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeGo{}
			r, _, _ := newRunner(t, fake, tc.skipList)
			r.Tier(TierIntegration, tc.dir, tc.prefix, tc.args, []string{tc.pkg})
			if len(fake.calls) != 1 {
				t.Fatalf("Exec called %d times, want 1", len(fake.calls))
			}
			if fake.calls[0].dir != tc.wantDir {
				t.Errorf("ran in %q, want %q", fake.calls[0].dir, tc.wantDir)
			}
			if !reflect.DeepEqual(fake.calls[0].argv, tc.want) {
				t.Errorf("command = %q, want %q", fake.calls[0].argv, tc.want)
			}
		})
	}
}

// TestTierSkipsWholePackage: a package named in TIER_SWEEP_SKIP for this tier
// is counted skipped and never run; a regexp entry runs it with -skip.
func TestTierSkipsWholePackage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		skipList   string
		pkgs       []string
		want       Counts
		wantRan    []string
		wantStderr string
		wantSkip   string
	}{
		{
			name:       "a whole-package entry for this tier skips it",
			skipList:   "integration:./internal/b",
			pkgs:       []string{"./internal/a", "./internal/b"},
			want:       Counts{Passed: 1, Skipped: 1},
			wantRan:    []string{"./internal/a"},
			wantStderr: "tier-sweep: integration ./internal/b SKIPPED (TIER_SWEEP_SKIP)\n",
		},
		{
			name:     "an entry for another tier does not skip it",
			skipList: "race:./internal/b",
			pkgs:     []string{"./internal/b"},
			want:     Counts{Passed: 1},
			wantRan:  []string{"./internal/b"},
		},
		{
			name:     "a regexp entry runs the package with -skip",
			skipList: "integration:./internal/a:^TestSlow$",
			pkgs:     []string{"./internal/a"},
			want:     Counts{Passed: 1},
			wantRan:  []string{"./internal/a"},
			wantSkip: "^TestSlow$",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeGo{}
			r, stderr, _ := newRunner(t, fake, tc.skipList)
			got := r.Tier(TierIntegration, ".", nil, nil, tc.pkgs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Tier() = %+v, want %+v", got, tc.want)
			}
			if ran := fake.ranPackages(); !reflect.DeepEqual(ran, tc.wantRan) {
				t.Errorf("ran %q, want %q", ran, tc.wantRan)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr has no %q line:\n%s", tc.wantStderr, stderr)
			}
			gotSkip := ""
			for i, arg := range fake.calls[0].argv {
				if arg == "-skip" {
					gotSkip = fake.calls[0].argv[i+1]
				}
			}
			if gotSkip != tc.wantSkip {
				t.Errorf("-skip = %q, want %q", gotSkip, tc.wantSkip)
			}
		})
	}
}

// TestTierLogsAndFailureExcerpt: each package logs to tier-<name>.log with the
// name's ./ dropped and / read as _, and a failing one shows its first five
// failure lines on stderr, indented four spaces, out of seven matches.
func TestTierLogsAndFailureExcerpt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dir     string
		pkg     string
		wantLog string
	}{
		{name: "a root package", dir: ".", pkg: "./internal/cmd", wantLog: "integration-internal_cmd.log"},
		{name: "a nested module package", dir: "tracker", pkg: "./store", wantLog: "integration-tracker_store.log"},
	}
	var wantExcerpt []string
	for i := 1; i <= 5; i++ {
		wantExcerpt = append(wantExcerpt, fmt.Sprintf("    --- FAIL: Test%d (0.00s)", i))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeGo{
				codes:  map[string]int{tc.pkg: 1},
				output: map[string]string{tc.pkg: sevenFailures()},
			}
			r, stderr, logDir := newRunner(t, fake, "")
			r.Tier(TierIntegration, tc.dir, nil, nil, []string{tc.pkg})
			if _, err := os.Stat(filepath.Join(logDir, tc.wantLog)); err != nil {
				t.Errorf("no log %s: %v", tc.wantLog, err)
			}
			if got := stderrLines(stderr); !reflect.DeepEqual(got, wantExcerpt) {
				t.Errorf("excerpt = %q, want %q", got, wantExcerpt)
			}
		})
	}
}

// sevenFailures is a failing package's output: a run line, then seven failures.
func sevenFailures() string {
	var out strings.Builder
	out.WriteString("=== RUN TestA\n")
	for i := 1; i <= 7; i++ {
		fmt.Fprintf(&out, "--- FAIL: Test%d (0.00s)\n", i)
	}
	return out.String()
}

// TestTierExcerptMarkers: the excerpt takes a line that starts with --- FAIL,
// panic: or FAIL and nothing else, so the ok line and an indented subtest's
// --- FAIL line stay out.
func TestTierExcerptMarkers(t *testing.T) {
	t.Parallel()
	output := "ok  \tgithub.com/x/a\t0.1s\n" +
		"FAIL\tgithub.com/x/b\t0.2s\n" +
		"panic: boom\n" +
		"    --- FAIL: TestNested (0.00s)\n" +
		"--- FAIL: TestTop (0.00s)\n"
	fake := &fakeGo{codes: map[string]int{"./b": 1}, output: map[string]string{"./b": output}}
	r, stderr, _ := newRunner(t, fake, "")
	r.Tier(TierIntegration, ".", nil, nil, []string{"./b"})

	want := []string{
		"    FAIL\tgithub.com/x/b\t0.2s",
		"    panic: boom",
		"    --- FAIL: TestTop (0.00s)",
	}
	if got := stderrLines(stderr); !reflect.DeepEqual(got, want) {
		t.Errorf("excerpt = %q, want %q", got, want)
	}
}

// TestTierLogCannotBeCreated: a package whose log cannot be created is counted
// failed and never run, so a missing log directory does not pass.
func TestTierLogCannotBeCreated(t *testing.T) {
	t.Parallel()
	fake := &fakeGo{}
	r, stderr, _ := newRunner(t, fake, "")
	r.LogDir = filepath.Join(t.TempDir(), "missing")
	got := r.Tier(TierIntegration, ".", nil, nil, []string{"./internal/a"})
	want := Counts{Failed: 1, FailedNames: []string{"./internal/a"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tier() = %+v, want %+v", got, want)
	}
	if len(fake.calls) != 0 {
		t.Errorf("Exec ran %d times, want 0", len(fake.calls))
	}
	if !strings.Contains(stderr.String(), "    create log ") {
		t.Errorf("stderr does not name the log that failed:\n%s", stderr)
	}
}

// TestTierFailsWhenLogDoesNotClose: a package passes only when its log also
// closes, so a log that failed to write does not pass as clean.
func TestTierFailsWhenLogDoesNotClose(t *testing.T) {
	t.Parallel()
	closeFirst := func(_ string, _ []string, _ []string, out io.Writer) int {
		_ = out.(*os.File).Close()
		return 0
	}
	r, stderr, _ := newRunner(t, &fakeGo{}, "")
	r.Exec = closeFirst
	got := r.Tier(TierIntegration, ".", nil, nil, []string{"./internal/a"})
	want := Counts{Failed: 1, FailedNames: []string{"./internal/a"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tier() = %+v, want %+v", got, want)
	}
	if !strings.Contains(stderr.String(), "    close log ") {
		t.Errorf("stderr does not report the log close:\n%s", stderr)
	}
}

// TestTierFailedLogUnreadable: a failed package whose log cannot be read is
// still counted failed, and the reason shows on stderr.
func TestTierFailedLogUnreadable(t *testing.T) {
	t.Parallel()
	removeLog := func(_ string, _ []string, _ []string, out io.Writer) int {
		_ = os.Remove(out.(*os.File).Name())
		return 1
	}
	r, stderr, _ := newRunner(t, &fakeGo{}, "")
	r.Exec = removeLog
	got := r.Tier(TierIntegration, ".", nil, nil, []string{"./internal/a"})
	want := Counts{Failed: 1, FailedNames: []string{"./internal/a"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tier() = %+v, want %+v", got, want)
	}
	if !strings.Contains(stderr.String(), "    read log ") {
		t.Errorf("stderr does not report the unreadable log:\n%s", stderr)
	}
}

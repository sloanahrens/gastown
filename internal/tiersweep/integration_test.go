package tiersweep

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// taggedTest is a test file the integration build tag selects.
const taggedTest = "//go:build integration\n\npackage x\n"

// plainTest is a test file the integration build tag does not select.
const plainTest = "package x\n"

// noDocker is a docker.txt with no container package, so a nested-module case
// runs only the packages it names.
const noDocker = "# no container packages\n"

// fakeRepo is the Source a test injects. Listings are keyed by directory and
// pattern ("." "*_test.go"), file contents by path. A missing listing is empty.
type fakeRepo struct {
	lists   map[string][]string
	listErr map[string]error
	files   map[string]string
}

func (r *fakeRepo) LsFiles(dir, pattern string) ([]string, error) {
	key := dir + " " + pattern
	if err := r.listErr[key]; err != nil {
		return nil, err
	}
	return r.lists[key], nil
}

func (r *fakeRepo) ReadFile(name string) ([]byte, error) {
	text, ok := r.files[name]
	if !ok {
		return nil, fmt.Errorf("no such file: %s", name)
	}
	return []byte(text), nil
}

// baseRepo is a repository with no test files and no container packages.
func baseRepo() *fakeRepo {
	return &fakeRepo{
		lists:   map[string][]string{},
		listErr: map[string]error{},
		files:   map[string]string{dockerList: noDocker},
	}
}

// tierCall is one Exec call the integration tier made.
type tierCall struct {
	dir  string
	env  []string
	argv []string
}

// fakeTier is the Exec a test injects. It answers git rev-parse, the trigger's
// two verbs and each go test by directory and package, and records every call.
// Nothing runs.
type fakeTier struct {
	head       string
	headFails  bool
	skipsOut   string
	skipsCode  int
	recordCode int
	// codes maps "dir pkg" to a go test exit code. A package with no entry passes.
	codes map[string]int
	calls []tierCall
}

func (f *fakeTier) exec(dir string, env []string, argv []string, out io.Writer) int {
	f.calls = append(f.calls, tierCall{dir: dir, env: env, argv: argv})
	switch {
	case argv[0] == "git":
		if f.headFails {
			return 128
		}
		fmt.Fprintln(out, f.head)
		return 0
	case f.isTriggerVerb(argv, "skips"):
		fmt.Fprint(out, f.skipsOut)
		return f.skipsCode
	case f.isTriggerVerb(argv, "record"):
		if f.recordCode != 0 {
			fmt.Fprintln(out, "tiertrigger: record: boom")
		}
		return f.recordCode
	}
	return f.codes[dir+" "+argv[len(argv)-1]]
}

// isTriggerVerb reports whether argv is the trigger command for verb.
func (f *fakeTier) isTriggerVerb(argv []string, verb string) bool {
	return argv[1] == "run" && argv[len(argv)-2] == verb
}

// testRuns lists each go test call as "dir pkg", in order.
func (f *fakeTier) testRuns() []string {
	var runs []string
	for _, c := range f.calls {
		if c.argv[0] == "gt" {
			runs = append(runs, c.dir+" "+c.argv[len(c.argv)-1])
		}
	}
	return runs
}

// triggerCalls lists the trigger calls for verb.
func (f *fakeTier) triggerCalls(verb string) []tierCall {
	var calls []tierCall
	for _, c := range f.calls {
		if f.isTriggerVerb(c.argv, verb) {
			calls = append(calls, c)
		}
	}
	return calls
}

// newIntegration is a Sweep over repo and fake, logging into a fresh temporary
// directory and writing its stderr to the returned buffer. The clock stands
// still, so every elapsed time reads 0s.
func newIntegration(t *testing.T, repo *fakeRepo, fake *fakeTier, skip, trigger string) (*Sweep, *bytes.Buffer) {
	t.Helper()
	stderr := &bytes.Buffer{}
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	return &Sweep{
		Opts:    Options{SkipList: skip, Timeout: "7m", LogDir: t.TempDir(), Trigger: trigger},
		Exec:    fake.exec,
		Src:     repo,
		Environ: []string{"PATH=/fake/bin"},
		Stderr:  stderr,
		Now:     func() time.Time { return at },
	}, stderr
}

// setTracker puts one nested module, tracker, with one tagged package at
// store/pgtest. The root lists the same file, which must not run from the root.
func setTracker(r *fakeRepo) {
	r.lists[". */go.mod"] = []string{"tracker/go.mod"}
	r.lists[". *_test.go"] = []string{"tracker/store/pgtest/x_test.go"}
	r.lists["tracker *_test.go"] = []string{"store/pgtest/x_test.go"}
	r.files["tracker/store/pgtest/x_test.go"] = taggedTest
}

// TestIntegrationNestedModuleRunsInItsDirectory: a nested module's tagged
// package runs with the module as its directory, and never from the root.
func TestIntegrationNestedModuleRunsInItsDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		setup     func(r *fakeRepo)
		codes     map[string]int
		skip      string
		wantRuns  []string
		wantFrag  string
		wantLines []string
		noLines   []string
	}{
		{
			name:     "a package in tracker runs with cwd tracker",
			setup:    setTracker,
			wantRuns: []string{"tracker ./store/pgtest"},
			wantFrag: "integration GREEN passed=1 failed=0 skipped=0",
		},
		{
			name:     "a failure names ./tracker/store/pgtest",
			setup:    setTracker,
			codes:    map[string]int{"tracker ./store/pgtest": 1},
			wantRuns: []string{"tracker ./store/pgtest"},
			wantFrag: "integration RED passed=0 failed=1 skipped=0 failed: ./tracker/store/pgtest",
		},
		{
			name: "a module with no tagged package runs nothing and prints nothing",
			setup: func(r *fakeRepo) {
				r.lists[". */go.mod"] = []string{"other/go.mod"}
				r.lists["other *_test.go"] = []string{"plain_test.go"}
				r.files["other/plain_test.go"] = plainTest
				r.lists[". *_test.go"] = []string{"r/r_test.go"}
				r.files["r/r_test.go"] = taggedTest
			},
			wantRuns: []string{". ./r"},
			wantFrag: "integration GREEN passed=1 failed=0 skipped=0",
			noLines:  []string{"other"},
		},
		{
			name: "a tagged file at the module root runs as ./tracker",
			setup: func(r *fakeRepo) {
				r.lists[". */go.mod"] = []string{"tracker/go.mod"}
				r.lists[". *_test.go"] = []string{"tracker/root_test.go"}
				r.lists["tracker *_test.go"] = []string{"root_test.go"}
				r.files["tracker/root_test.go"] = taggedTest
			},
			wantRuns:  []string{"tracker ./"},
			wantFrag:  "integration GREEN passed=1 failed=0 skipped=0",
			wantLines: []string{"tier-sweep: integration ./tracker ..."},
		},
		{
			name: "a file ending in go.mod is not a module",
			setup: func(r *fakeRepo) {
				r.lists[". */go.mod"] = []string{"tracker/go.mod", "extra/notgo.mod"}
				r.lists[". *_test.go"] = []string{"extra/x_test.go"}
				r.files["extra/x_test.go"] = taggedTest
			},
			wantRuns: []string{". ./extra"},
			wantFrag: "integration GREEN passed=1 failed=0 skipped=0",
		},
		{
			name:     "the same skip entry applies to a nested package",
			setup:    setTracker,
			skip:     "integration:./tracker/store/pgtest",
			wantFrag: "integration GREEN passed=0 failed=0 skipped=1",
			wantLines: []string{
				"tier-sweep: integration ./tracker/store/pgtest SKIPPED (TIER_SWEEP_SKIP)",
			},
		},
		{
			name: "a package under a deeper module runs in that module only",
			setup: func(r *fakeRepo) {
				r.lists[". */go.mod"] = []string{"tracker/go.mod", "tracker/store/go.mod"}
				r.lists[". *_test.go"] = []string{"tracker/store/pg/x_test.go"}
				r.lists["tracker *_test.go"] = []string{"store/pg/x_test.go"}
				r.lists["tracker/store *_test.go"] = []string{"pg/x_test.go"}
				r.files["tracker/store/pg/x_test.go"] = taggedTest
			},
			wantRuns: []string{"tracker/store ./pg"},
			wantFrag: "integration GREEN passed=1 failed=0 skipped=0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := baseRepo()
			tc.setup(repo)
			fake := &fakeTier{head: "abc123", codes: tc.codes}
			s, stderr := newIntegration(t, repo, fake, tc.skip, "")
			line, _ := s.Integration()
			if !reflect.DeepEqual(fake.testRuns(), tc.wantRuns) {
				t.Errorf("go test runs = %v, want %v", fake.testRuns(), tc.wantRuns)
			}
			if !strings.Contains(line, tc.wantFrag) {
				t.Errorf("summary = %q, want it to contain %q", line, tc.wantFrag)
			}
			for _, want := range tc.wantLines {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
			for _, no := range tc.noLines {
				if strings.Contains(stderr.String(), no) {
					t.Errorf("stderr = %q, want no mention of %q", stderr.String(), no)
				}
			}
		})
	}
}

// TestIntegrationRootTestdataFileIsKept pins the script's gap: a root-level
// testdata/ file is not excluded from the tagged list, only one under a
// directory with something before the slash is.
func TestIntegrationRootTestdataFileIsKept(t *testing.T) {
	t.Parallel()
	repo := baseRepo()
	repo.lists[". *_test.go"] = []string{"testdata/x_test.go"}
	repo.files["testdata/x_test.go"] = taggedTest
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, repo, fake, "", "")
	line, red := s.Integration()
	if want := []string{". ./testdata"}; !reflect.DeepEqual(fake.testRuns(), want) {
		t.Errorf("go test runs = %v, want %v", fake.testRuns(), want)
	}
	if red || !strings.Contains(line, "integration GREEN passed=1") {
		t.Errorf("summary = %q (red %v), want GREEN passed=1", line, red)
	}
}

// TestIntegrationNamesOrder: the names keep the script's order. The root's
// failures come first, then each nested module's, then the untagged group,
// which "untagged:" labels as the names after it.
func TestIntegrationNamesOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		setup    func(r *fakeRepo)
		codes    map[string]int
		wantFrag string
	}{
		{
			name: "a root failure survives a nested module that reports nothing",
			setup: func(r *fakeRepo) {
				r.lists[". */go.mod"] = []string{"other/go.mod"}
				r.lists["other *_test.go"] = []string{"plain_test.go"}
				r.files["other/plain_test.go"] = plainTest
				r.lists[". *_test.go"] = []string{"r/r_test.go"}
				r.files["r/r_test.go"] = taggedTest
			},
			codes:    map[string]int{". ./r": 1},
			wantFrag: "integration RED passed=0 failed=1 skipped=0 failed: ./r",
		},
		{
			name: "a nested failure precedes the untagged group",
			setup: func(r *fakeRepo) {
				setTracker(r)
				r.lists[". *_test.go"] = append(r.lists[". *_test.go"], "r/r_test.go")
				r.files["r/r_test.go"] = taggedTest
				r.files[dockerList] = "internal/beads\ninternal/testutil\n"
			},
			codes: map[string]int{
				". ./r":                  1,
				"tracker ./store/pgtest": 1,
				". ./internal/beads":     1,
				". ./internal/testutil":  1,
			},
			wantFrag: "integration RED passed=0 failed=4 skipped=0 failed: ./r ./tracker/store/pgtest untagged:./internal/beads ./internal/testutil",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := baseRepo()
			tc.setup(repo)
			fake := &fakeTier{head: "abc123", codes: tc.codes}
			s, _ := newIntegration(t, repo, fake, "", "")
			line, red := s.Integration()
			if !red || !strings.Contains(line, tc.wantFrag) {
				t.Errorf("summary = %q, want it to contain %q", line, tc.wantFrag)
			}
		})
	}
}

// TestIntegrationTriggerSkipsAndRecord: the container trigger's skips reach the
// runner, a passing container run records exactly once, and a red or unrunnable
// trigger never fails the tier on its own.
func TestIntegrationTriggerSkipsAndRecord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		trigger     string
		skipsOut    string
		skipsCode   int
		recordCode  int
		codes       map[string]int
		wantRuns    []string
		wantRecords int
		wantStderr  string
		wantEnv     string
		wantFrag    string
	}{
		{
			name:        "a package the trigger reports not due is skipped",
			skipsOut:    "integration-docker:./internal/beads\n",
			wantRuns:    []string{". ./internal/testutil"},
			wantRecords: 1,
			wantStderr:  "tier-sweep: integration-docker ./internal/beads SKIPPED (TIER_SWEEP_SKIP)",
			wantFrag:    "integration GREEN passed=1 failed=0 skipped=1",
		},
		{
			name:        "a passing container run records once",
			wantRuns:    []string{". ./internal/beads", ". ./internal/testutil"},
			wantRecords: 1,
			wantFrag:    "integration GREEN passed=2 failed=0 skipped=0",
		},
		{
			name:        "a red container run never records",
			codes:       map[string]int{". ./internal/beads": 1},
			wantRuns:    []string{". ./internal/beads", ". ./internal/testutil"},
			wantRecords: 0,
			wantFrag:    "integration RED passed=1 failed=1 skipped=0 failed: untagged:./internal/beads",
		},
		{
			name:        "a trigger that cannot run leaves every package running and the tier GREEN",
			skipsOut:    "tier-sweep: TIER_SWEEP_TRIGGER: skips integration-docker failed (boom); every container package runs\n",
			skipsCode:   1,
			wantRuns:    []string{". ./internal/beads", ". ./internal/testutil"},
			wantRecords: 1,
			wantStderr:  "every container package runs",
			wantFrag:    "integration GREEN passed=2 failed=0 skipped=0",
		},
		{
			name:        "a failing record is ignored and the tier stays GREEN",
			recordCode:  1,
			wantRuns:    []string{". ./internal/beads", ". ./internal/testutil"},
			wantRecords: 1,
			wantStderr:  "tiertrigger: record: boom",
			wantFrag:    "integration GREEN passed=2 failed=0 skipped=0",
		},
		{
			name:        "TIER_SWEEP_TRIGGER=0 reaches the child",
			trigger:     "0",
			wantRuns:    []string{". ./internal/beads", ". ./internal/testutil"},
			wantRecords: 1,
			wantEnv:     "TIER_SWEEP_TRIGGER=0",
			wantFrag:    "integration GREEN passed=2 failed=0 skipped=0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := baseRepo()
			repo.files[dockerList] = "internal/beads\ninternal/testutil\n"
			fake := &fakeTier{
				head:       "abc123",
				skipsOut:   tc.skipsOut,
				skipsCode:  tc.skipsCode,
				recordCode: tc.recordCode,
				codes:      tc.codes,
			}
			s, stderr := newIntegration(t, repo, fake, "", tc.trigger)
			line, _ := s.Integration()
			if !reflect.DeepEqual(fake.testRuns(), tc.wantRuns) {
				t.Errorf("go test runs = %v, want %v", fake.testRuns(), tc.wantRuns)
			}
			if got := len(fake.triggerCalls("record")); got != tc.wantRecords {
				t.Errorf("record calls = %d, want %d", got, tc.wantRecords)
			}
			if !strings.Contains(line, tc.wantFrag) {
				t.Errorf("summary = %q, want it to contain %q", line, tc.wantFrag)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
			for _, c := range append(fake.triggerCalls("skips"), fake.triggerCalls("record")...) {
				if !envHas(c.env, "TIER_SWEEP_TESTED=abc123") {
					t.Errorf("trigger env %v lacks TIER_SWEEP_TESTED=abc123", c.env)
				}
				if tc.wantEnv != "" && !envHas(c.env, tc.wantEnv) {
					t.Errorf("trigger env %v lacks %s", c.env, tc.wantEnv)
				}
			}
		})
	}
}

// TestIntegrationEmptyIsRed: a tier with no tagged package and no container
// package is RED with no names, the same as any tier that counted nothing.
func TestIntegrationEmptyIsRed(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{head: "abc123"}
	s, _ := newIntegration(t, baseRepo(), fake, "", "")
	line, red := s.Integration()
	want := fmt.Sprintf("tier-sweep: integration RED passed=0 failed=0 skipped=0 (logs %s) in 0s", s.Opts.LogDir)
	if line != want || !red {
		t.Errorf("Integration() = %q, %v; want %q, true", line, red, want)
	}
	if runs := fake.testRuns(); len(runs) != 0 {
		t.Errorf("go test runs = %v, want none", runs)
	}
}

// TestIntegrationUnreadableListIsRed: a list the tier cannot read runs nothing
// and is RED with no names. An empty list would pass without a run.
func TestIntegrationUnreadableListIsRed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(r *fakeRepo)
		want  string
	}{
		{
			name: "the nested module list fails",
			setup: func(r *fakeRepo) {
				r.listErr[". */go.mod"] = errors.New("ls-files broke")
			},
			want: "list nested modules",
		},
		{
			name: "docker.txt is missing",
			setup: func(r *fakeRepo) {
				delete(r.files, dockerList)
			},
			want: "read " + dockerList,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := baseRepo()
			tc.setup(repo)
			fake := &fakeTier{head: "abc123"}
			s, stderr := newIntegration(t, repo, fake, "", "")
			line, red := s.Integration()
			if !red || !strings.Contains(line, "integration RED passed=0 failed=0 skipped=0") {
				t.Errorf("Integration() = %q, %v; want a RED summary with no names", line, red)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tc.want)
			}
			if runs := fake.testRuns(); len(runs) != 0 {
				t.Errorf("go test runs = %v, want none", runs)
			}
		})
	}
}

// envHas reports whether env holds the exact entry kv.
func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

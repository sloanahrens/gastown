package tiersweep

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// raceSweep is a Sweep over fake for one race run, logging into a fresh
// temporary directory. The clock stands still, so every elapsed time reads 0s.
func raceSweep(t *testing.T, fake *fakeTier) *Sweep {
	t.Helper()
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	return &Sweep{
		Opts:   Options{Timeout: "7m", LogDir: t.TempDir()},
		Exec:   fake.exec,
		Stderr: io.Discard,
		Now:    func() time.Time { return at },
	}
}

func TestRaceCommandLine(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{}
	s := raceSweep(t, fake)
	s.raceTier(raceRuns)
	want := []string{
		"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1",
		"go", "test", "-count=1", "-timeout", "7m",
		"-race", "-tags", "integration",
		"-run", "^TestCommandTreeWalkIsReadOnlyUnderParallelTests$",
		"./internal/cmd",
	}
	if len(fake.calls) != 1 {
		t.Fatalf("race ran %d commands, want 1: %v", len(fake.calls), fake.calls)
	}
	if got := fake.calls[0]; got.dir != "." || !reflect.DeepEqual(got.argv, want) {
		t.Errorf("race ran in %q as %q, want in \".\" as %q", got.dir, got.argv, want)
	}
}

func TestRaceRunsInTheSweepEnvironment(t *testing.T) {
	t.Parallel()
	fake := &fakeTier{}
	s := raceSweep(t, fake)
	s.Environ = []string{"PATH=/usr/bin"}
	s.Opts.Tested = "abc123"
	s.raceTier(raceRuns)
	want := []string{"PATH=/usr/bin", "TIER_SWEEP_TESTED=abc123", "TIER_SWEEP_SKIP="}
	if len(fake.calls) != 1 {
		t.Fatalf("race ran %d commands, want 1: %v", len(fake.calls), fake.calls)
	}
	if got := fake.calls[0].env; !reflect.DeepEqual(got, want) {
		t.Errorf("race ran with env %q, want %q", got, want)
	}
}

func TestRaceSummary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		code int
		// want is the summary line; %s is the log directory the sweep writes.
		want string
		red  bool
	}{
		{
			name: "pass",
			code: 0,
			want: "tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs %s) in 0s",
		},
		{
			name: "fail",
			code: 1,
			want: "tier-sweep: race RED passed=0 failed=1 skipped=0 failed: ./internal/cmd~integration-walk (logs %s) in 0s",
			red:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeTier{codes: map[string]int{". ./internal/cmd": c.code}}
			s := raceSweep(t, fake)
			line, red := s.raceTier(raceRuns)
			want := fmt.Sprintf(c.want, s.Opts.LogDir)
			if line != want || red != c.red {
				t.Errorf("raceTier(raceRuns) = %q, %v; want %q, %v", line, red, want, c.red)
			}
		})
	}
}

func TestRaceTierRunsEveryEntryAndSumsTheirCounts(t *testing.T) {
	t.Parallel()
	runs := []raceRun{
		{pkgs: []string{"./one"}, args: []string{"-race", "-run", "One"}},
		{pkgs: []string{"./two"}, args: []string{"-race", "-run", "Two"}},
	}
	head := []string{"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1",
		"go", "test", "-count=1", "-timeout", "7m"}
	want := [][]string{
		append(append(append([]string{}, head...), runs[0].args...), "./one"),
		append(append(append([]string{}, head...), runs[1].args...), "./two"),
	}
	fake := &fakeTier{}
	s := raceSweep(t, fake)
	line, red := s.raceTier(runs)
	if w := fmt.Sprintf("tier-sweep: race GREEN passed=2 failed=0 skipped=0 (logs %s) in 0s", s.Opts.LogDir); line != w || red {
		t.Errorf("raceTier() = %q, %v; want %q, false", line, red, w)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("race ran %d commands, want 2", len(fake.calls))
	}
	for i := range want {
		if got := fake.calls[i].argv; !reflect.DeepEqual(got, want[i]) {
			t.Errorf("race call %d ran %q, want %q", i, got, want[i])
		}
	}
	fake = &fakeTier{codes: map[string]int{". ./two": 1}}
	s = raceSweep(t, fake)
	line, red = s.raceTier(runs)
	if w := fmt.Sprintf("tier-sweep: race RED passed=1 failed=1 skipped=0 failed: ./two (logs %s) in 0s", s.Opts.LogDir); line != w || !red {
		t.Errorf("raceTier() = %q, %v; want %q, true", line, red, w)
	}
	// A RED in the first entry is not masked by a later pass (gt-uqklea).
	fake = &fakeTier{codes: map[string]int{". ./one": 1}}
	s = raceSweep(t, fake)
	line, red = s.raceTier(runs)
	if w := fmt.Sprintf("tier-sweep: race RED passed=1 failed=1 skipped=0 failed: ./one (logs %s) in 0s", s.Opts.LogDir); line != w || !red {
		t.Errorf("raceTier() = %q, %v; want %q, true", line, red, w)
	}
}

// TestRaceTierRunsEveryPackageOfOneEntry: an entry that names several packages
// runs each with the entry's args and label, so a package after the first is
// run, counted and named rather than silently dropped (gt-yznnu5).
func TestRaceTierRunsEveryPackageOfOneEntry(t *testing.T) {
	t.Parallel()
	entry := raceRun{pkgs: []string{"./one", "./two"}, args: []string{"-race", "-run", "Shared"}, label: "shared"}
	fake := &fakeTier{codes: map[string]int{". ./two": 1}}
	s := raceSweep(t, fake)
	line, red := s.raceTier([]raceRun{entry})
	want := fmt.Sprintf("tier-sweep: race RED passed=1 failed=1 skipped=0 failed: ./two~shared (logs %s) in 0s", s.Opts.LogDir)
	if line != want || !red {
		t.Errorf("raceTier(%v) = %q, %v; want %q, true", []raceRun{entry}, line, red, want)
	}
	if got, want := fake.testRuns(), []string{". ./one", ". ./two"}; !slices.Equal(got, want) {
		t.Errorf("raceTier(%v) ran %q, want %q", []raceRun{entry}, got, want)
	}
	head := []string{"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1", "go", "test", "-count=1", "-timeout", "7m"}
	for _, pkg := range entry.pkgs {
		want := append(append(append([]string{}, head...), entry.args...), pkg)
		if got := goTestCall(t, fake, ".", pkg); !reflect.DeepEqual(got, want) {
			t.Errorf("race ran %s as %q, want %q", pkg, got, want)
		}
	}
}

// TestRaceTierKeepsTwoEntriesOnOnePackageApart: a labeled entry keeps its own
// log file and resume key from another entry on the same package, an empty
// label keeps the name it has today, and a bad label is refused before any run.
func TestRaceTierKeepsTwoEntriesOnOnePackageApart(t *testing.T) {
	t.Parallel()
	first := raceRun{pkgs: []string{"./a"}, args: []string{"-run", "First"}, label: "first"}
	plain := raceRun{pkgs: []string{"./a"}, args: []string{"-run", "Plain"}}
	// ran lists each go test's -run value, read from the flag, not argv's layout.
	ran := func(fake *fakeTier) (got []string) {
		for _, c := range fake.calls {
			for i := 0; i+1 < len(c.argv); i++ {
				if c.argv[i] == "-run" {
					got = append(got, c.argv[i+1])
				}
			}
		}
		return got
	}
	// Either order: neither entry inherits the other's label, and each writes
	// the log its own name gives it.
	for _, runs := range [][]raceRun{{first, plain}, {plain, first}} {
		fake := &fakeTier{}
		s := raceSweep(t, fake)
		s.raceTier(runs)
		entries, err := os.ReadDir(s.Opts.LogDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].Name() != LogName(TierRace, "./a") || entries[1].Name() != LogName(TierRace, "./a~first") {
			t.Errorf("raceTier(%v) logs = %v, want one per entry", runs, entries)
		}
	}
	path := filepath.Join(t.TempDir(), "resume.jsonl")
	seedResume(t, path, "shaA", "", TierRace, "./a~first")
	fake := &fakeTier{}
	s := raceSweep(t, fake)
	s.Resume = &Resume{Path: path, Key: primeKey("shaA"), ReadFile: os.ReadFile, AppendFile: appendToFile}
	s.raceTier([]raceRun{first, plain})
	if got := ran(fake); !slices.Equal(got, []string{"Plain"}) {
		t.Errorf("with a record for the labeled entry raceTier ran %q, want only the plain entry", got)
	}
	fake = &fakeTier{}
	s = raceSweep(t, fake)
	s.Opts.SkipList = "race:./a~first"
	want := fmt.Sprintf("tier-sweep: race GREEN passed=1 failed=0 skipped=1 (logs %s) in 0s", s.Opts.LogDir)
	if line, red := s.raceTier([]raceRun{first, plain}); line != want || red || !slices.Equal(ran(fake), []string{"Plain"}) {
		t.Errorf("skipping the labeled entry = %q, %v, ran %q; want %q, false, [Plain]", line, red, ran(fake), want)
	}
	fake = &fakeTier{codes: map[string]int{". ./a": 1}}
	s = raceSweep(t, fake)
	line, red := s.raceTier([]raceRun{plain})
	want = fmt.Sprintf("tier-sweep: race RED passed=0 failed=1 skipped=0 failed: ./a (logs %s) in 0s", s.Opts.LogDir)
	if line != want || !red {
		t.Errorf("raceTier with no label = %q, %v; want %q, true", line, red, want)
	}
	fake = &fakeTier{}
	s = raceSweep(t, fake)
	line, red = s.raceTier([]raceRun{{pkgs: []string{"./a"}, args: []string{"-run", "Bad"}, label: "a b"}})
	want = fmt.Sprintf(`tier-sweep: race RED passed=0 failed=0 skipped=0: label "a b" is not [A-Za-z0-9._-]+ (logs %s) in 0s`, s.Opts.LogDir)
	if line != want || !red || len(fake.calls) != 0 {
		t.Errorf("raceTier with a space in the label = %q, %v, ran %v; want %q, true, no command", line, red, fake.calls, want)
	}
}

// TestRaceTierRefusesTwoEntriesWithOnePackageAndLabel: two entries that would
// share one name, log file and resume key are refused before any run, naming
// the pair, while entries that differ in package or label still run. An entry
// naming no package is refused before any run too, naming its label.
func TestRaceTierRefusesTwoEntriesWithOnePackageAndLabel(t *testing.T) {
	t.Parallel()
	labeled := raceRun{pkgs: []string{"./one"}, args: []string{"-run", "One"}, label: "x"}
	plain := raceRun{pkgs: []string{"./one"}, args: []string{"-run", "One"}}
	other := plain
	other.label = "y"
	green := "tier-sweep: race GREEN passed=2 failed=0 skipped=0 (logs %s) in 0s"
	cases := []struct {
		name string
		runs []raceRun
		// want is the summary line; %s is the log directory the sweep writes.
		want string
		red  bool
		ran  int
	}{
		{"same package and label", []raceRun{labeled, labeled}, "tier-sweep: race RED passed=0 failed=0 skipped=0: ./one~x is listed twice (logs %s) in 0s", true, 0},
		{"same package different labels", []raceRun{labeled, other}, green, false, 2},
		{"same package one label empty", []raceRun{labeled, plain}, green, false, 2},
		{"no package", []raceRun{{pkgs: nil, args: []string{"-run", "One"}, label: "empty"}}, `tier-sweep: race RED passed=0 failed=0 skipped=0: entry "empty" has no package (logs %s) in 0s`, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeTier{}
			s := raceSweep(t, fake)
			line, red := s.raceTier(c.runs)
			want := fmt.Sprintf(c.want, s.Opts.LogDir)
			if line != want || red != c.red {
				t.Errorf("raceTier(%v) = %q, %v; want %q, %v", c.runs, line, red, want, c.red)
			}
			if len(fake.calls) != c.ran {
				t.Errorf("raceTier(%v) ran %d commands, want %d", c.runs, len(fake.calls), c.ran)
			}
		})
	}
}

// raceRepo is a fake repository with the race tier's test listing and nested
// module listing.
func raceRepo(testFiles, goMods []string) *fakeRepo {
	repo := baseRepo()
	repo.lists[". *_test.go"] = testFiles
	repo.lists[". */go.mod"] = goMods
	return repo
}

// TestRacePackageList covers the race tier's package list: every root-module
// test package RacePackages finds, in its order, less raceExcluded. A listing
// that cannot be read is an error, while no test file is an empty list.
//
//testpolicy:allow parallel — swaps the package-level raceExcluded that racePackageList reads, so no parallel test may run beside it
func TestRacePackageList(t *testing.T) {
	t.Run("the production exclusion list drops nothing", func(t *testing.T) {
		got, err := racePackageList(&Sweep{Src: raceRepo([]string{"alpha/a_test.go", "beta/b_test.go"}, nil)})
		if err != nil {
			t.Fatalf("racePackageList() error = %v, want nil", err)
		}
		if want := []string{"./alpha", "./beta"}; !slices.Equal(got, want) {
			t.Errorf("racePackageList() = %q, want %q: a test race is a defect to fix, not a skip", got, want)
		}
	})
	t.Run("two tracked test dirs give both packages", func(t *testing.T) {
		got, err := racePackageList(&Sweep{Src: raceRepo([]string{"zeta/z_test.go", "pkg.go", "alpha/a_test.go"}, nil)})
		if err != nil {
			t.Fatalf("racePackageList() error = %v, want nil", err)
		}
		if want := []string{"./alpha", "./zeta"}; !slices.Equal(got, want) {
			t.Errorf("racePackageList() = %q, want %q", got, want)
		}
	})
	t.Run("a test in a nested module is left out", func(t *testing.T) {
		got, err := racePackageList(&Sweep{Src: raceRepo([]string{"alpha/a_test.go", "tracker/y_test.go"}, []string{"tracker/go.mod"})})
		if err != nil {
			t.Fatalf("racePackageList() error = %v, want nil", err)
		}
		if want := []string{"./alpha"}; !slices.Equal(got, want) {
			t.Errorf("racePackageList() = %q, want %q: a nested module's test is not the root tier's", got, want)
		}
	})
	t.Run("an unreadable module listing is an error", func(t *testing.T) {
		repo := baseRepo()
		repo.listErr[". */go.mod"] = errors.New("git listing: boom")
		_, err := racePackageList(&Sweep{Src: repo})
		if err == nil || !strings.Contains(err.Error(), "list nested modules") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("racePackageList() error = %v, want it to name the read and its cause", err)
		}
	})
	t.Run("an unreadable test listing is an error", func(t *testing.T) {
		repo := baseRepo()
		repo.listErr[". *_test.go"] = errors.New("git listing: boom")
		_, err := racePackageList(&Sweep{Src: repo})
		if err == nil || !strings.Contains(err.Error(), "list test files") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("racePackageList() error = %v, want it to name the read and its cause", err)
		}
	})
	t.Run("no test file is an empty list", func(t *testing.T) {
		got, err := racePackageList(&Sweep{Src: baseRepo()})
		if err != nil {
			t.Fatalf("racePackageList() error = %v, want nil", err)
		}
		if len(got) != 0 {
			t.Errorf("racePackageList() = %q, want no packages", got)
		}
	})
	t.Run("an excluded name is dropped", func(t *testing.T) {
		repo := raceRepo([]string{"alpha/a_test.go", "beta/b_test.go"}, nil)
		//testpolicy:allow no-global-swap — the exclusion list is the input under test
		raceExcluded = []string{"./beta"}
		t.Cleanup(func() {
			//testpolicy:allow no-global-swap — restores the exclusion list for later tests
			raceExcluded = nil
		})
		got, err := racePackageList(&Sweep{Src: repo})
		if err != nil {
			t.Fatalf("racePackageList() error = %v, want nil", err)
		}
		if want := []string{"./alpha"}; !slices.Equal(got, want) {
			t.Errorf("racePackageList() = %q, want %q", got, want)
		}
	})
}

// TestRaceRunsWithPackages covers the race tier's table with the all-packages
// entry: raceRuns' own entries unchanged, then one entry running every package
// racePackageList lists under -race. A listing that cannot be read is returned
// as the error, never as a table quietly missing that entry.
func TestRaceRunsWithPackages(t *testing.T) {
	t.Parallel()
	t.Run("the all-packages entry follows the existing entries", func(t *testing.T) {
		t.Parallel()
		repo := raceRepo([]string{"alpha/a_test.go", "zeta/z_test.go"}, nil)
		before := slices.Clone(raceRuns)
		got, err := raceRunsWithPackages(&Sweep{Src: repo})
		if err != nil {
			t.Fatalf("raceRunsWithPackages() error = %v, want nil", err)
		}
		want := append(slices.Clone(before), raceRun{
			pkgs:  []string{"./alpha", "./zeta"},
			args:  []string{"-race"},
			label: "all-packages",
		})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("raceRunsWithPackages() = %v, want %v", got, want)
		}
		if !reflect.DeepEqual(raceRuns, before) {
			t.Errorf("raceRuns = %v after the call, want it unchanged at %v: a caller must not see the entry appear twice", raceRuns, before)
		}
	})
	t.Run("an unreadable list is an error", func(t *testing.T) {
		t.Parallel()
		repo := baseRepo()
		repo.listErr[". *_test.go"] = errors.New("git listing: boom")
		got, err := raceRunsWithPackages(&Sweep{Src: repo})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("raceRunsWithPackages() error = %v, want the listing's cause", err)
		}
		if got != nil {
			t.Errorf("raceRunsWithPackages() = %v on error, want nil so no run starts", got)
		}
	})
}

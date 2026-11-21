package tiersweep

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	s.Race()
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
	s.Race()
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
			line, red := s.Race()
			want := fmt.Sprintf(c.want, s.Opts.LogDir)
			if line != want || red != c.red {
				t.Errorf("Race() = %q, %v; want %q, %v", line, red, want, c.red)
			}
		})
	}
}

func TestRaceTierRunsEveryEntryAndSumsTheirCounts(t *testing.T) {
	t.Parallel()
	runs := []raceRun{
		{pkg: "./one", args: []string{"-race", "-run", "One"}},
		{pkg: "./two", args: []string{"-race", "-run", "Two"}},
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

// TestRaceTierKeepsTwoEntriesOnOnePackageApart: a labeled entry keeps its own
// log file and resume key from another entry on the same package, an empty
// label keeps the name it has today, and a bad label is refused before any run.
func TestRaceTierKeepsTwoEntriesOnOnePackageApart(t *testing.T) {
	t.Parallel()
	first := raceRun{pkg: "./a", args: []string{"-run", "First"}, label: "first"}
	plain := raceRun{pkg: "./a", args: []string{"-run", "Plain"}}
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
	line, red = s.raceTier([]raceRun{{pkg: "./a", args: []string{"-run", "Bad"}, label: "a b"}})
	want = fmt.Sprintf(`tier-sweep: race RED passed=0 failed=0 skipped=0: label "a b" is not [A-Za-z0-9._-]+ (logs %s) in 0s`, s.Opts.LogDir)
	if line != want || !red || len(fake.calls) != 0 {
		t.Errorf("raceTier with a space in the label = %q, %v, ran %v; want %q, true, no command", line, red, fake.calls, want)
	}
}

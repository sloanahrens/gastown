package tiersweep

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// racePkg is the package raceRuns' entry runs its test in.
const racePkg = "./internal/cmd"

// raceArgs are the go test arguments of raceRuns' entry: the race detector on
// the integration build, for the one test that walks the command tree under
// parallel tests. Only -race shows a regression there.
var raceArgs = []string{"-race", "-tags", "integration", "-run", "^TestCommandTreeWalkIsReadOnlyUnderParallelTests$"}

// raceLabelRE is what an entry's label may hold: the daemon reads the labeled
// name as a path and refuses a space or bracket (tierSweepUnitRE).
var raceLabelRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// raceRun is one entry of the race tier: the packages it runs with its own go
// test arguments.
type raceRun struct {
	pkgs []string
	args []string
	// label keeps two entries on one package's log and resume key apart.
	label string
	// pooled starts the entry's serial.txt packages on the pool too, set per
	// entry so one that does not ask for it never inherits it (gt-2ekofw).
	pooled bool
}

// raceRuns is the race tier's fixed entries; raceRunsWithPackages adds the one
// that covers every listed package.
var raceRuns = []raceRun{{pkgs: []string{racePkg}, args: raceArgs, label: "integration-walk"}}

// raceExcluded names the race tier's packages that are not run. It is empty:
// a test race is a defect to fix, never a skip, and an entry needs a bead and
// a reason.
var raceExcluded []string

// racePackageList lists the root module's test packages the race tier runs:
// every package RacePackages finds, less raceExcluded. The nested modules are
// read as planIntegration reads them, so a test in a nested module never
// enters the root race run. A listing that cannot be read is an error naming
// the read, not an empty list: an empty list would pass without a run.
// raceRunsWithPackages is its caller.
func racePackageList(s *Sweep) ([]string, error) {
	goMods, err := s.Src.LsFiles(".", "*/go.mod")
	if err != nil {
		return nil, fmt.Errorf("list nested modules: %w", err)
	}
	files, err := s.Src.LsFiles(".", "*_test.go")
	if err != nil {
		return nil, fmt.Errorf("list test files: %w", err)
	}
	pkgs := RacePackages(slices.Concat(files, goMods))
	if len(raceExcluded) > 0 {
		pkgs = slices.DeleteFunc(pkgs, func(pkg string) bool { return slices.Contains(raceExcluded, pkg) })
	}
	return pkgs, nil
}

// raceRunsWithPackages is the race tier's table with one entry added: raceRuns
// followed by every package racePackageList lists, under -race, labeled
// all-packages. It builds a new table, so raceRuns is neither changed nor
// appended to and a second call sees the same table. A listing that cannot be
// read is returned as the error, never as a table missing its entry.
func raceRunsWithPackages(s *Sweep) ([]raceRun, error) {
	pkgs, err := racePackageList(s)
	if err != nil {
		return nil, err
	}
	return append(slices.Clone(raceRuns), raceRun{pkgs: pkgs, args: []string{"-race"}, label: "all-packages"}), nil
}

// Race runs the race tier, raceRunsWithPackages' table, and returns its summary
// line and whether it is RED.
func (s *Sweep) Race() (line string, red bool) {
	start := s.Now()
	runs, err := raceRunsWithPackages(s)
	if err != nil {
		// A table that cannot be built runs nothing, so the tier fails closed:
		// RED with no names, the cause on stderr.
		fmt.Fprintf(s.Stderr, "%s: %s: %v\n", Name, TierRace, err)
		return Summary(TierRace, s.Opts.LogDir, 0, 0, 0, "", int(s.Now().Sub(start)/time.Second))
	}
	return s.raceTier(runs)
}

// raceRefusal reports why raceTier refuses its table: one line naming the
// cause on stderr, and a RED summary with no counts. The reason rides the
// stderr line because the summary's shape is the daemon's to parse
// (tierSweepSummaryRE), which reads only a package list after the skipped
// count, so a reason there would leave the line unparsed and its verdict lost
// (gt-xp74il).
func raceRefusal(s *Sweep, reason string, seconds int) (string, bool) {
	fmt.Fprintf(s.Stderr, "%s: %s: %s\n", Name, TierRace, reason)
	return Summary(TierRace, s.Opts.LogDir, 0, 0, 0, "", seconds)
}

// raceTier runs each entry in runs and sums their counts into one summary.
func (s *Sweep) raceTier(runs []raceRun) (line string, red bool) {
	start := s.Now()
	elapsed := func() int { return int(s.Now().Sub(start) / time.Second) }
	// A run's log file and resume key are its package name, followed by ~label
	// when it has one (Runner.tasks). Two runs that build the same name would
	// share one log and key, the second counted passed from the first's record,
	// so both are refused before anything runs.
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		// An entry with no package has no name to log or resume under, and
		// Runner.Tier would run no command for it, so the tier would report
		// GREEN having run nothing.
		if len(run.pkgs) == 0 {
			return raceRefusal(s, fmt.Sprintf("entry %q has no package", run.label), elapsed())
		}
		if run.label != "" && !raceLabelRE.MatchString(run.label) {
			return raceRefusal(s, fmt.Sprintf("label %q is not [A-Za-z0-9._-]+", run.label), elapsed())
		}
		name := run.pkgs[0]
		if run.label != "" {
			name += "~" + run.label
		}
		if seen[name] {
			return raceRefusal(s, fmt.Sprintf("%s is listed twice", name), elapsed())
		}
		seen[name] = true
	}
	runner := &Runner{
		Exec:     s.Exec,
		LogDir:   s.Opts.LogDir,
		Timeout:  s.Opts.Timeout,
		SkipList: s.Opts.SkipList,
		Env:      s.exportEnv(s.Opts.Tested, s.Opts.SkipList),
		Stderr:   s.Stderr,
		Timings:  s.Timings,
		Now:      s.Now,
		Jobs:     s.Opts.Jobs,
		Walls:    s.Walls,
		Resume:   s.Resume,
	}
	passed, failed, skipped := 0, 0, 0
	var names []string
	for _, run := range runs {
		runner.Label = run.label
		runner.Pooled = run.pooled
		c := runner.Tier(TierRace, ".", slotPrefix(s.Opts.Nice, s.Opts.NoSlot), run.args, run.pkgs)
		passed += c.Passed
		failed += c.Failed
		skipped += c.Skipped
		names = append(names, c.FailedNames...)
	}
	return Summary(TierRace, s.Opts.LogDir, passed, failed, skipped, strings.Join(names, " "), elapsed())
}

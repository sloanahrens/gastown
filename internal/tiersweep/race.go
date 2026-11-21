package tiersweep

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// racePkg is the one package the race tier runs its test in.
const racePkg = "./internal/cmd"

// raceArgs are the go test arguments for the race tier: the race detector on
// the integration build, for the one test that walks the command tree under
// parallel tests. Only -race shows a regression there.
var raceArgs = []string{"-race", "-tags", "integration", "-run", "^TestCommandTreeWalkIsReadOnlyUnderParallelTests$"}

// raceLabelRE is what an entry's label may hold: the daemon reads the labelled
// name as a path and refuses a space or bracket (tierSweepUnitRE).
var raceLabelRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// raceRun is one package the race tier runs, with its own go test arguments.
type raceRun struct {
	pkg  string
	args []string
	// label keeps two entries on one package's log and resume key apart.
	label string
}

// raceRuns is the race tier's table: the packages the race detector covers.
var raceRuns = []raceRun{{racePkg, raceArgs, "integration-walk"}}

// Race runs the race tier and returns its summary line and whether it is RED.
func (s *Sweep) Race() (line string, red bool) {
	return s.raceTier(raceRuns)
}

// raceTier runs each entry in runs and sums their counts into one summary.
func (s *Sweep) raceTier(runs []raceRun) (line string, red bool) {
	start := s.Now()
	elapsed := func() int { return int(s.Now().Sub(start) / time.Second) }
	for _, run := range runs {
		if run.label != "" && !raceLabelRE.MatchString(run.label) {
			return fmt.Sprintf("tier-sweep: race RED passed=0 failed=0 skipped=0: label %q is not [A-Za-z0-9._-]+ (logs %s) in %s", run.label, s.Opts.LogDir, FormatDuration(elapsed())), true
		}
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
		c := runner.Tier(TierRace, ".", slotPrefix(s.Opts.Nice, s.Opts.NoSlot), run.args, []string{run.pkg})
		passed += c.Passed
		failed += c.Failed
		skipped += c.Skipped
		names = append(names, c.FailedNames...)
	}
	return Summary(TierRace, s.Opts.LogDir, passed, failed, skipped, strings.Join(names, " "), elapsed())
}

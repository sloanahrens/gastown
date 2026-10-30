package tiersweep

import (
	"strings"
	"time"
)

// racePkg is the one package the race tier runs its test in.
const racePkg = "./internal/cmd"

// raceArgs are the go test arguments for the race tier: the race detector on
// the integration build, for the one test that walks the command tree under
// parallel tests. Only -race shows a regression there.
var raceArgs = []string{"-race", "-tags", "integration", "-run", "^TestCommandTreeWalkIsReadOnlyUnderParallelTests$"}

// Race runs the race tier and returns its summary line and whether it is RED.
// The run is one package in the root module, under the container slot.
func (s *Sweep) Race() (line string, red bool) {
	start := s.Now()
	elapsed := func() int { return int(s.Now().Sub(start) / time.Second) }
	runner := &Runner{
		Exec:     s.Exec,
		LogDir:   s.Opts.LogDir,
		Timeout:  s.Opts.Timeout,
		SkipList: s.Opts.SkipList,
		Env:      s.exportEnv(s.Opts.Tested, s.Opts.SkipList),
		Stderr:   s.Stderr,
		Timings:  s.Timings,
		Now:      s.Now,
	}
	c := runner.Tier(TierRace, ".", slotPrefix(s.Opts.Nice), raceArgs, []string{racePkg})
	return Summary(TierRace, s.Opts.LogDir, c.Passed, c.Failed, c.Skipped, strings.Join(c.FailedNames, " "), elapsed())
}

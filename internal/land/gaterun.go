package land

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GateRun is one gate run's record. ParseGateRunLog fills the stage and verdict fields from the
// run's job log; the other fields are set by whatever records the run, not by the parser.
type GateRun struct {
	TS                  time.Time        `json:"ts"`
	Rig                 string           `json:"rig"`
	Bead                string           `json:"bead"`
	SHA                 string           `json:"sha"`
	RunID               string           `json:"run_id"`
	QueueS              float64          `json:"queue_s"`
	RunS                float64          `json:"run_s"`
	SetupS              float64          `json:"setup_s"`
	LintS               float64          `json:"lint_s"`
	BuildS              float64          `json:"build_s"`
	UnitWallS           float64          `json:"unit_wall_s"`
	TierS               float64          `json:"tier_s"`
	TierBarS            float64          `json:"tier_bar_s"`
	Over                bool             `json:"over"`
	PolePkg             string           `json:"pole_pkg"`
	PoleS               float64          `json:"pole_s"`
	Packages            []GateRunPackage `json:"packages"`
	SlowTests           []GateRunTest    `json:"slow_tests"`
	NumCPU              int              `json:"num_cpu"`
	GOMAXPROCS          int              `json:"gomaxprocs"`
	Cgroup              string           `json:"cgroup"`
	CachedPkgs          int              `json:"cached_pkgs"`
	Passed              bool             `json:"passed"`
	RunningRuns         int              `json:"running_runs"`
	RunnerPeriods       int64            `json:"runner_periods"`
	RunnerThrottled     int64            `json:"runner_throttled"`
	RunnerThrottledUsec int64            `json:"runner_throttled_usec"`
}

// GateRunPackage is one package's wall time; GateRunTest is one slow test's.
type GateRunPackage struct {
	Pkg string  `json:"pkg"`
	S   float64 `json:"s"`
}

type GateRunTest struct {
	Pkg  string  `json:"pkg"`
	Test string  `json:"test"`
	S    float64 `json:"s"`
}

// gateWallRE reads the wall seconds off a verdict line: "after 139s wall" or "PASSED in 139s wall".
var gateWallRE = regexp.MustCompile(`(?:after|PASSED in) (\d+)s wall`)

// ParseGateRunLog reads a gate job log into a GateRun. Setup runs from the task line to the first
// lint line, lint to the build line and build to the unit tier line; the verdict line gives the
// unit wall and passed. A missing marker, or a stage that does not run forward, leaves its field
// zero, and a line without a leading RFC3339 timestamp is skipped.
func ParseGateRunLog(log string) GateRun {
	var run GateRun
	var task, lint, build, unit time.Time
	for _, line := range strings.Split(log, "\n") {
		at, msg, ok := splitGateLine(line)
		if !ok {
			continue
		}
		switch {
		case task.IsZero() && strings.HasPrefix(msg, "task received"):
			task = at
		case lint.IsZero() && strings.HasPrefix(msg, "lint:"):
			lint = at
		case build.IsZero() && strings.HasPrefix(msg, "gate: build"):
			build = at
		case unit.IsZero() && strings.HasPrefix(msg, "gate: unit tier"):
			unit = at
		case strings.HasPrefix(msg, "gate: PASSED in "), strings.HasPrefix(msg, "gate: FAILED at "):
			run.Passed = strings.HasPrefix(msg, "gate: PASSED in ")
			// A wall too large for a float64 is left zero; the verdict still counts.
			if m := gateWallRE.FindStringSubmatch(msg); m != nil {
				if wall, err := strconv.ParseFloat(m[1], 64); err == nil {
					run.UnitWallS = wall
				}
			}
		}
	}
	run.SetupS = stageSeconds(task, lint)
	run.LintS = stageSeconds(lint, build)
	run.BuildS = stageSeconds(build, unit)
	return run
}

// splitGateLine splits a line into its RFC3339 timestamp and the text after it; ok is false when
// the line has no text or its first field is not a timestamp.
func splitGateLine(line string) (at time.Time, msg string, ok bool) {
	stamp, msg, found := strings.Cut(line, " ")
	at, err := time.Parse(time.RFC3339, stamp)
	return at, msg, found && err == nil
}

// stageSeconds is the seconds from a stage's start to its end: zero when either marker is missing
// or the end is before the start.
func stageSeconds(from, to time.Time) float64 {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0
	}
	return to.Sub(from).Seconds()
}

package land

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GateRun is one gate run's record. ParseGateRunLog fills the stage, verdict and vet fields from
// the run's job log; the other fields are set by whatever records the run, not by the parser.
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
	VetS                float64          `json:"vet_s"`
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

// vetWallRE reads the integration vet's summary line, as internal/testpolicy/cmd/integrationvet
// writes it: "integration vet: 79 packages in 2 modules type-check with -tags integration in
// 1m5.8s". The package and module counts are not kept. The line is matched whole, so the vet's
// failure and error lines, which share the prefix, are not read as a wall.
var vetWallRE = regexp.MustCompile(`^integration vet: \d+ packages in \d+ modules type-check with -tags integration in (\S+)$`)

// ParseGateRunLog reads a gate job log into a GateRun. Setup runs from the runner's task line
// ("Runner <id> ... received task <n> ...") to the first lint line, lint to the build line and
// build to the unit tier line; the verdict line gives the unit wall and passed. The integration
// vet's summary line gives its wall in seconds as VetS. The tierwall report gives the tier,
// packages, slow tests, CPUs and pole; a "(cached)" ok line counts one cached package. A missing
// marker, or a stage that does not run forward, leaves its field zero, a tierwall line or a vet
// duration it cannot read is skipped rather than read as a value, and a line without a leading
// RFC3339 timestamp is skipped.
func ParseGateRunLog(log string) GateRun {
	var run GateRun
	var task, lint, build, unit time.Time
	var tw gateTierwall
	for _, line := range strings.Split(log, "\n") {
		at, msg, ok := splitGateLine(line)
		if !ok {
			continue
		}
		if f := strings.Fields(msg); len(f) == 3 && f[0] == "ok" && f[2] == "(cached)" {
			run.CachedPkgs++
		}
		switch {
		case task.IsZero() && strings.HasPrefix(msg, "Runner ") && strings.Contains(msg, " received task "):
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
		case strings.HasPrefix(msg, "integration vet: "):
			// A duration that does not parse, or a negative one, is no wall: leave VetS zero.
			if m := vetWallRE.FindStringSubmatch(strings.TrimSuffix(msg, "\r")); m != nil {
				if d, err := time.ParseDuration(m[1]); err == nil && d > 0 {
					run.VetS = d.Seconds()
				}
			}
		case strings.HasPrefix(msg, "tierwall: "):
			tw.parse(&run, strings.TrimSuffix(strings.TrimPrefix(msg, "tierwall: "), "\r"))
		}
	}
	run.SetupS = stageSeconds(task, lint)
	run.LintS = stageSeconds(lint, build)
	run.BuildS = stageSeconds(build, unit)
	run.PolePkg, run.PoleS = gatePole(run.Packages)
	return run
}

// tierwall line shapes, as internal/testpolicy/cmd/tierwall writes them. Each is matched whole, so
// a line that does not parse as one is not half-read as another.
var (
	tierwallTierRE = regexp.MustCompile(`^tier (\d+(?:\.\d+)?)s bar (\d+(?:\.\d+)?)s (OK|OVER)$`)
	tierwallCPURe  = regexp.MustCompile(`^cpus: NumCPU=(\d+) GOMAXPROCS=(\d+) cgroup=(.+)$`)
	tierwallPkgRE  = regexp.MustCompile(`^(\S+) (\d+(?:\.\d+)?)s$`)
	tierwallSlowRE = regexp.MustCompile(`^  (\S+) (\S+) (\d+(?:\.\d+)?)s$`)
)

// gateTierwall is the state kept while reading the tierwall report: whether the slow-tests block
// is open, so a test-shaped line outside it is not read as one.
type gateTierwall struct{ slow bool }

// parse reads one line of the tierwall report, msg being the text after "tierwall: ".
func (w *gateTierwall) parse(run *GateRun, msg string) {
	if msg == "slow tests" {
		w.slow = true
		return
	}
	if w.slow {
		if m := tierwallSlowRE.FindStringSubmatch(msg); m != nil {
			if s, ok := gateSecs(m[3]); ok {
				run.SlowTests = append(run.SlowTests, GateRunTest{Pkg: m[1], Test: m[2], S: s})
			}
			return
		}
		w.slow = false
	}
	if m := tierwallTierRE.FindStringSubmatch(msg); m != nil {
		s, sok := gateSecs(m[1])
		bar, bok := gateSecs(m[2])
		if sok && bok {
			run.TierS, run.TierBarS, run.Over = s, bar, m[3] == "OVER"
		}
		return
	}
	if m := tierwallCPURe.FindStringSubmatch(msg); m != nil {
		ncpu, err := strconv.Atoi(m[1])
		procs, perr := strconv.Atoi(m[2])
		if err == nil && perr == nil {
			run.NumCPU, run.GOMAXPROCS, run.Cgroup = ncpu, procs, strings.TrimSpace(m[3])
		}
		return
	}
	if m := tierwallPkgRE.FindStringSubmatch(msg); m != nil {
		if s, ok := gateSecs(m[2]); ok {
			run.Packages = append(run.Packages, GateRunPackage{Pkg: m[1], S: s})
		}
	}
}

// gateSecs reads a tierwall seconds field such as "29.3s"; ok is false when it is too large for a
// float64.
func gateSecs(field string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSuffix(field, "s"), 64)
	return v, err == nil
}

// gatePole is the longest package, the first of equals; empty when the report listed none.
func gatePole(pkgs []GateRunPackage) (string, float64) {
	name, s := "", 0.0
	for i, p := range pkgs {
		if i == 0 || p.S > s {
			name, s = p.Pkg, p.S
		}
	}
	return name, s
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

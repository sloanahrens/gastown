// Package isolationprobe computes the isolation-probe verdict a hosted
// workspace run records; docs/guides/gke-isolation-probes.md states the rules
// and calls the command built in cmd/isolation-verdict.
package isolationprobe

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// windowLines is the 5000-line window the run's log command fetches; a log
// that fills it may have lost its start, so the verdict fails it closed.
const windowLines = 5000

// Result is one probe's reading: a known failure, an unproven fallback, or no
// known failure.
type Result string

const (
	// Fail is a probe the log shows failing, or one read from an unusable run.
	Fail Result = "fail"
	// Inconclusive is a probe that ran on a fallback path the run does not
	// show to hold the isolation property.
	Inconclusive Result = "inconclusive"
	// NoKnownFailure is a probe the run shows no failure for; it is not a pass.
	NoKnownFailure Result = "no-known-failure"
)

// Outcome is the verdict's overall reading, which is never a pass.
type Outcome string

const (
	// OutcomeFail is the verdict when any probe failed.
	OutcomeFail Outcome = "FAIL"
	// OutcomeInconclusive is the verdict when no probe failed and any probe
	// fell back.
	OutcomeInconclusive Outcome = "INCONCLUSIVE"
	// OutcomeNoKnownFailure is the verdict when no probe failed or fell back.
	OutcomeNoKnownFailure Outcome = "NO-KNOWN-FAILURE"
)

// Reading is one probe's result.
type Reading struct {
	Probe  string
	Result Result
}

// Verdict is a run directory's reading.
type Verdict struct {
	Readings []Reading
	Outcome  Outcome
	// Problems states why an input was unusable; they are printed beside the
	// verdict, never in its lines, so verdict.txt keeps its recorded shape.
	Problems []string
}

// Lines renders the verdict as the runbook records it: one line per probe,
// then the VERDICT line naming every probe that failed or fell back.
func (v Verdict) Lines() []string {
	lines := make([]string, 0, len(v.Readings)+1)
	for _, r := range v.Readings {
		lines = append(lines, fmt.Sprintf("probe=%s result=%s", r.Probe, r.Result))
	}
	switch {
	case v.Outcome == OutcomeNoKnownFailure:
		lines = append(lines, "VERDICT NO-KNOWN-FAILURE")
	case v.Outcome == OutcomeInconclusive:
		lines = append(lines, "VERDICT INCONCLUSIVE probe="+strings.Join(v.names(Inconclusive), ","))
	default:
		// An unset outcome is a verdict no probe vouched for, so it reads as
		// a failure rather than as the clean line.
		lines = append(lines, "VERDICT FAIL probe="+strings.Join(v.names(Fail), ","))
	}
	return lines
}

// ExitCode is 0 only for NO-KNOWN-FAILURE; every other verdict is non-zero so
// a caller that checks the status never admits co-tenancy on a fallback.
func (v Verdict) ExitCode() int {
	if v.Outcome == OutcomeNoKnownFailure {
		return 0
	}
	return 1
}

// names returns the probes whose reading is result, in print order.
func (v Verdict) names(result Result) []string {
	var out []string
	for _, r := range v.Readings {
		if r.Result == result {
			out = append(out, r.Probe)
		}
	}
	return out
}

// probe is one supervisor probe and the log patterns that decide its reading.
type probe struct {
	name      string
	failures  []*regexp.Regexp
	fallbacks []*regexp.Regexp
}

// probes are the supervisor's three probes in print order. The patterns are
// the runbook's table, matched case-insensitively one log line at a time; a
// fallback pattern only applies once no failure pattern has matched the probe.
var probes = []probe{
	{
		name: "landlock",
		failures: []*regexp.Regexp{
			regexp.MustCompile(`(?i)Landlock probe child exited with status 1`),
			regexp.MustCompile(`(?i)incompatible access-rights`),
			regexp.MustCompile(`(?i)ABI[ =:]*[0-2]([^0-9]|$)`),
		},
	},
	{
		name: "seccomp-user-notify",
		failures: []*regexp.Regexp{
			regexp.MustCompile(`(?i)notification launcher disappeared`),
			regexp.MustCompile(`(?i)ControlSupervisorStartFailed`),
		},
		fallbacks: []*regexp.Regexp{
			regexp.MustCompile(`(?i)legacy`),
			regexp.MustCompile(`(?i)read-only listener`),
		},
	},
	{
		name: "task-memory",
		failures: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(task-memory|process_vm_readv).*(fail|denied)`),
		},
		fallbacks: []*regexp.Regexp{
			regexp.MustCompile(`(?i)/proc/[^ ]*/mem`),
		},
	},
}

// Evaluate reads dir for the expected node and computes the verdict; an input
// it cannot read, or one that contradicts the run, fails every probe.
func Evaluate(dir, node string) Verdict {
	var (
		problems   []string
		failClosed bool
	)

	status, err := os.ReadFile(filepath.Join(dir, "create.status"))
	switch {
	case err != nil:
		problems = append(problems, "create.status could not be read")
		failClosed = true
	case strings.TrimRight(string(status), "\n") != "0":
		problems = append(problems, "create.status does not read 0")
		failClosed = true
	}

	list, err := os.ReadFile(filepath.Join(dir, "list.txt"))
	switch {
	case err != nil:
		problems = append(problems, "list.txt could not be read")
		failClosed = true
	case !sandboxReady(list):
		problems = append(problems, "list.txt does not show isolation-probe exactly Ready")
		failClosed = true
	}

	if node == "" {
		problems = append(problems, "no expected node was given")
		failClosed = true
	}
	placement, err := os.ReadFile(filepath.Join(dir, "placement.txt"))
	switch {
	case err != nil:
		problems = append(problems, "placement.txt could not be read")
		failClosed = true
	case !placedOn(placement, node):
		problems = append(problems, "placement.txt does not name the expected node")
		failClosed = true
	}

	log, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	var logLines []string
	switch {
	case err != nil:
		problems = append(problems, "log.txt could not be read")
		failClosed = true
	case len(log) == 0:
		problems = append(problems, "log.txt is empty")
		failClosed = true
	case countLines(log) >= windowLines:
		problems = append(problems, "log.txt fills the 5000-line window")
		failClosed = true
	default:
		logLines = strings.Split(string(log), "\n")
	}

	v := Verdict{Problems: problems}
	for _, p := range probes {
		switch {
		case failClosed || matchAny(logLines, p.failures):
			v.Readings = append(v.Readings, Reading{Probe: p.name, Result: Fail})
		case matchAny(logLines, p.fallbacks):
			v.Readings = append(v.Readings, Reading{Probe: p.name, Result: Inconclusive})
		default:
			v.Readings = append(v.Readings, Reading{Probe: p.name, Result: NoKnownFailure})
		}
	}
	switch {
	case len(v.names(Fail)) > 0:
		v.Outcome = OutcomeFail
	case len(v.names(Inconclusive)) > 0:
		v.Outcome = OutcomeInconclusive
	default:
		v.Outcome = OutcomeNoKnownFailure
	}
	return v
}

// sandboxReady reports whether the sandbox list carries a row for
// isolation-probe whose Ready field is not preceded by Not.
func sandboxReady(list []byte) bool {
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "isolation-probe" {
			continue
		}
		for i := 1; i < len(fields); i++ {
			if fields[i] == "Ready" && fields[i-1] != "Not" {
				return true
			}
		}
	}
	return false
}

// placedOn reports whether a placement row puts the sandbox on node; an empty
// node names nothing, so it matches no row.
func placedOn(placement []byte, node string) bool {
	if node == "" {
		return false
	}
	for _, line := range strings.Split(string(placement), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == node {
			return true
		}
	}
	return false
}

// countLines counts a final segment without a newline as a line, so a log that
// fills the window either way is caught.
func countLines(log []byte) int {
	if len(log) == 0 {
		return 0
	}
	n := bytes.Count(log, []byte("\n"))
	if log[len(log)-1] != '\n' {
		n++
	}
	return n
}

// matchAny reports whether any line matches any of patterns.
func matchAny(lines []string, patterns []*regexp.Regexp) bool {
	for _, line := range lines {
		for _, re := range patterns {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}

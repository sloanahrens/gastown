package done

import "strings"

// GateRun is one Gate line that runs go test with a -run filter: the line
// trimmed of surrounding space, the package arguments go test is given, and
// the pattern -run selects (gt-hx8f5i).
type GateRun struct {
	Line     string
	Packages []string
	Pattern  string
}

// gateRunLines returns the go test lines of a bead's Gate section that narrow
// the run with -run, in the order they appear. A line that is not go test, one
// with no -run, one naming ./..., and one that is not a single plain command
// are dropped: the branch check reads one pattern against named packages
// (gt-hx8f5i).
func gateRunLines(gate string) []GateRun {
	var runs []GateRun
	for _, line := range strings.Split(gate, "\n") {
		if run, ok := gateRun(strings.TrimSpace(line)); ok {
			runs = append(runs, run)
		}
	}
	return runs
}

// gateRun reads one Gate line, returning false for anything but the form
// "go test <package...> -run <pattern> <flags>": the packages are the tokens
// before the first flag, and a shell metacharacter outside the -run pattern
// means the line holds more than this command.
func gateRun(line string) (GateRun, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "test" {
		return GateRun{}, false
	}
	run := GateRun{Line: line}
	filtered, flags := false, false
	for i := 2; i < len(fields); i++ {
		field := fields[i]
		name := strings.TrimLeft(field, "-")
		switch {
		case name == "run":
			if i+1 == len(fields) {
				return GateRun{}, false
			}
			i++
			run.Pattern, filtered, flags = unquotePattern(fields[i]), true, true
		case strings.HasPrefix(name, "run="):
			run.Pattern, filtered, flags = unquotePattern(strings.TrimPrefix(name, "run=")), true, true
		case strings.ContainsAny(field, "&|;<>`(){}"):
			return GateRun{}, false
		case !flags && !strings.HasPrefix(field, "-"):
			run.Packages = append(run.Packages, field)
		default:
			flags = true
		}
	}
	if !filtered || len(run.Packages) == 0 {
		return GateRun{}, false
	}
	for _, pkg := range run.Packages {
		if pkg == "./..." {
			return GateRun{}, false
		}
	}
	return run, true
}

// unquotePattern drops one pair of matching shell quotes around a -run
// pattern, so 'TestA|TestB', "TestA|TestB" and TestA|TestB read alike.
func unquotePattern(pattern string) string {
	if len(pattern) >= 2 {
		first, last := pattern[0], pattern[len(pattern)-1]
		if first == last && (first == '\'' || first == '"') {
			return pattern[1 : len(pattern)-1]
		}
	}
	return pattern
}

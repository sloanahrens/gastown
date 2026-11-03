package specdispatch

import (
	"fmt"
	"strings"
)

// ContainerSuitePackages returns the repo-relative Go package paths whose
// tests spin real Dolt/testcontainers containers (or a shell script that
// launches them). The runtime guard in internal/cmd/tap_guard_container_suite.go
// and the Gate lint both judge this one list, so the two cannot drift.
//
// A package belongs when its test binary links internal/testutil, the only
// route into container-spinning code: run
// `go list -test -f '{{.ImportPath}} {{join .Deps "\n"}}' ./internal/...`
// and keep every package that imports the testutil path. An entry that never
// starts a container costs one needless `gt slot run`; a missing one is
// invisible to the guard entirely (gt-e2rs).
//
// Every call returns a fresh slice, so a caller cannot mutate another's list.
func ContainerSuitePackages() []string {
	return []string{
		"internal/beads",
		"internal/cmd",
		"internal/crew",
		"internal/daemon",
		"internal/deps",
		"internal/doctor",
		"internal/doltserver",
		"internal/git",
		"internal/mail",
		"internal/plugin",
		"internal/polecat",
		"internal/protocol",
		"internal/rig",
		"internal/testutil",
	}
}

// containerSuiteGateRefusal refuses the first Gate line that runs a listed
// package's whole test suite with no -run filter: the suite spins containers,
// and a polecat running it wedges its session. The runtime guard catches an
// ad-hoc run; this catches the line when the bead is shaped (gt-hgt9j.4).
func containerSuiteGateRefusal(gate string) (Refusal, bool) {
	packages := ContainerSuitePackages()
	for _, line := range strings.Split(gate, "\n") {
		for _, segment := range shellSegments(line) {
			args, ok := goTestArgs(strings.Fields(segment))
			if !ok || hasRunFilter(args) {
				continue
			}
			for _, arg := range args {
				if pkg := containerSuitePackageArg(packages, arg); pkg != "" {
					return Refusal{
						Field:  "## Gate",
						Reason: fmt.Sprintf("go test ./%s runs the container suite; narrow it with -run '<TestName>'", pkg),
					}, true
				}
			}
		}
	}
	return Refusal{}, false
}

// shellSegments splits one Gate line at its unquoted shell operators (&&,
// ||, ;, |), so each command is judged on its own flags: a -run on a later
// command does not narrow an earlier one. A quoted operator, such as the bar
// in -run 'TestA|TestB', stays inside its segment.
func shellSegments(line string) []string {
	var segments []string
	start := 0
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case ';':
			segments = append(segments, line[start:i])
			start = i + 1
		case '|':
			segments = append(segments, line[start:i])
			if i+1 < len(line) && line[i+1] == '|' {
				i++
			}
			start = i + 1
		case '&':
			if i+1 < len(line) && line[i+1] == '&' {
				segments = append(segments, line[start:i])
				i++
				start = i + 1
			}
		}
	}
	return append(segments, line[start:])
}

// goTestArgs returns the unquoted arguments after "go test" in one command
// segment, and false when the segment does not invoke go test. The binary is
// matched by its base name, so an absolute path to the toolchain counts too.
func goTestArgs(fields []string) ([]string, bool) {
	for i := 0; i+1 < len(fields); i++ {
		if !isGoBinaryArg(unquoteToken(fields[i])) || unquoteToken(fields[i+1]) != "test" {
			continue
		}
		args := make([]string, 0, len(fields)-i-2)
		for _, field := range fields[i+2:] {
			args = append(args, unquoteToken(field))
		}
		return args, true
	}
	return nil, false
}

// unquoteToken strips the wrapping quotes a Gate line may put around a command
// or one of its arguments, so `go test ./internal/cmd` and
// "go test ./internal/cmd" are judged like the bare form.
func unquoteToken(token string) string {
	return strings.Trim(token, "`\"'")
}

// isGoBinaryArg reports whether field names the go command, either bare or as
// a path to it.
func isGoBinaryArg(field string) bool {
	name := field
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name == "go"
}

// hasRunFilter reports whether the arguments narrow the test run with -run,
// in either the "-run value" or the "-run=value" spelling.
func hasRunFilter(args []string) bool {
	for _, arg := range args {
		flag := strings.TrimLeft(arg, "-")
		if flag == "run" || strings.HasPrefix(flag, "run=") {
			return true
		}
	}
	return false
}

// containerSuitePackageArg returns the listed package arg targets, and "" when
// it targets none. Only the two spellings count: ./<package> and
// ./<package>/.... The match is exact, so ./internal/cmdfoo is not
// ./internal/cmd.
func containerSuitePackageArg(packages []string, arg string) string {
	for _, pkg := range packages {
		if arg == "./"+pkg || arg == "./"+pkg+"/..." {
			return pkg
		}
	}
	return ""
}

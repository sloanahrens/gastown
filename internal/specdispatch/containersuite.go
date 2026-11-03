package specdispatch

import (
	"fmt"
	"strings"
)

// ContainerSuitePackages returns the repo-relative Go package paths whose test
// binaries link internal/testutil, a fresh slice per call (gt-e2rs). To extend
// it, list the packages whose test binaries import internal/testutil (go list
// -test -deps over ./internal/...). A false entry costs one extra -run wrap; a
// missing entry makes both the runtime guard and the Gate lint blind to that
// package, so add a new package here when it first links testutil.
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

// shellSegments splits one Gate line at every shell separator character (&, |,
// ;, parentheses and braces), so each command is judged on its own flags: a
// -run on a later command does not narrow an earlier one, whatever separator
// joins them. Quotes and backticks are deleted rather than parsed, so wrapping
// or an unbalanced quote cannot hide a separator (gt-hgt9j.4).
func shellSegments(line string) []string {
	return strings.FieldsFunc(stripQuotes(line), func(r rune) bool {
		switch r {
		case '&', '|', ';', '(', ')', '{', '}':
			return true
		}
		return false
	})
}

// stripQuotes removes every backtick, single quote and double quote from a
// Gate line, so a quoted or backtick-wrapped command reads like the bare form.
func stripQuotes(line string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '`', '\'', '"':
			return -1
		}
		return r
	}, line)
}

// goTestArgs returns the arguments after "go test" in one command segment, and
// false when the segment does not invoke go test. The binary is matched by its
// base name, so an absolute path to the toolchain counts too.
func goTestArgs(fields []string) ([]string, bool) {
	for i := 0; i+1 < len(fields); i++ {
		if !isGoBinaryArg(fields[i]) || fields[i+1] != "test" {
			continue
		}
		return fields[i+2:], true
	}
	return nil, false
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

// hasRunFilter reports whether the arguments narrow the test run with -run, in
// either the "-run value" or the "-run=value" spelling. The leading dash is
// required, so a flag value spelled run (such as -tags run) is not a filter
// (gt-hgt9j.4).
func hasRunFilter(args []string) bool {
	for _, arg := range args {
		if arg == "-run" || arg == "--run" || strings.HasPrefix(arg, "-run=") || strings.HasPrefix(arg, "--run=") {
			return true
		}
	}
	return false
}

// containerSuitePackageArg returns the listed package the argument targets,
// and "" when it targets none. Only the relative spellings count: ./<package>,
// ./<package>/ and ./<package>/.... An import path spelled out in full is not
// matched (a known gap). The match is exact, so ./internal/cmdfoo is not
// ./internal/cmd.
func containerSuitePackageArg(packages []string, arg string) string {
	for _, pkg := range packages {
		if arg == "./"+pkg || arg == "./"+pkg+"/" || arg == "./"+pkg+"/..." {
			return pkg
		}
	}
	return ""
}

// Package lintpin checks the golangci-lint on PATH against the version the
// gastown Makefile pins, and installs the pin when it is absent or differs.
//
// GOLANGCI_LINT_VERSION in the Makefile is the pin's one home; make lint and
// make lint-tools pass it to cmd/lintpin, so no recipe repeats the version.
// The gate runner's CI base image carries the pin, which is why the lint stage
// compiles nothing there (gt-zzo1g2).
package lintpin

import (
	"fmt"
	"strings"
)

const (
	// Command is the linter's name on PATH.
	Command = "golangci-lint"
	// Module is the package that installs the pin.
	Module = "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"
)

// Runner runs name with args and returns its standard output, so a test drives
// every branch without a process.
type Runner func(name string, args ...string) ([]byte, error)

// Short returns pin without the leading v that golangci-lint version --short
// leaves out.
func Short(pin string) string { return strings.TrimPrefix(pin, "v") }

// Verify returns the version when the linter on PATH reports pin and refuses
// every other answer: a runner error, no answer, or another version.
func Verify(run Runner, pin string) (string, error) {
	out, err := run(Command, "version", "--short")
	if err != nil {
		return "", fmt.Errorf("golangci-lint is missing from PATH or cannot report its version: the gate runner's CI base image must carry %s (gt-zzo1g2), or run 'make lint-tools'", pin)
	}
	got := strings.TrimSpace(string(out))
	switch got {
	case "":
		return "", fmt.Errorf("golangci-lint version --short printed no version, so PATH cannot be shown to carry the pin %s (gt-zzo1g2): run 'make lint-tools' and put its directory first on PATH, or rebuild the CI base image with the pin", pin)
	case Short(pin):
		return got, nil
	default:
		return got, fmt.Errorf("golangci-lint on PATH is %s, not the pinned %s (gt-zzo1g2): run 'make lint-tools' and put its directory first on PATH, or rebuild the CI base image with the pin", got, pin)
	}
}

// Install installs the pin with the Go toolchain, and only when Verify refuses
// the linter already on PATH. It reports whether it installed anything.
func Install(run Runner, pin string) (installed bool, err error) {
	if _, err := Verify(run, pin); err == nil {
		return false, nil
	}
	if _, err := run("go", "install", Module+"@"+pin); err != nil {
		return false, fmt.Errorf("installing %s@%s: %w", Module, pin, err)
	}
	return true, nil
}

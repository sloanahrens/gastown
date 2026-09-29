// Package deps manages external dependencies for Gas Town.
package deps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/util"
)

// BeadsStatus represents the state of the beads installation.
type BeadsStatus int

const (
	BeadsOK       BeadsStatus = iota // bd found and reported a version
	BeadsNotFound                    // bd not in PATH
	BeadsUnknown                     // bd found but its version could not be read
)

// CheckBeads checks that bd is on PATH and reports a version. It is a cheap
// presence probe, not a compatibility verdict: whether the town may run on
// this bd is CheckBDHandshake's decision (schema level and JSON contract),
// because a semver says nothing about either.
// Returns status and the installed version (if found).
func CheckBeads() (BeadsStatus, string) {
	// Check if bd exists in PATH
	path, err := exec.LookPath("bd")
	if err != nil {
		return BeadsNotFound, ""
	}
	_ = path // bd found

	// Get version (with timeout to prevent hanging on broken bd installs).
	// 10s is generous but necessary: under heavy CI load (parallel test
	// packages), even a trivial shell script can take >3s to start.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a clean environment that strips BEADS target env vars
	// to prevent stale shell state from leaking into version checks.
	baseEnv := beads.StripBDTargetEnv(os.Environ())
	cmd := beads.CommandContextWithEnv(ctx, "", baseEnv, "version")
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	return beadsStatusFromOutput(output, err)
}

// beadsStatusFromOutput classifies the result of running "bd version".
func beadsStatusFromOutput(output []byte, err error) (BeadsStatus, string) {
	if err != nil {
		return BeadsUnknown, ""
	}

	version := parseBeadsVersion(string(output))
	if version == "" {
		return BeadsUnknown, ""
	}
	return BeadsOK, version
}

// EnsureBeads returns nil when bd is on PATH and reports a version. It never
// installs bd: gastown installing upstream bd@latest over the fork is how
// production gets migrated or skewed (G3-07). An unreadable version is an
// error, not a pass.
func EnsureBeads() error {
	status, _ := CheckBeads()
	return beadsErrorForStatus(status)
}

func beadsErrorForStatus(status BeadsStatus) error {
	switch status {
	case BeadsOK:
		return nil
	case BeadsNotFound:
		return fmt.Errorf("beads (bd) not found in PATH\n\nFix: %s", BDInstallHint)
	default:
		return fmt.Errorf("beads (bd) is on PATH but its version could not be determined\n\nFix: %s", BDInstallHint)
	}
}

// parseBeadsVersion extracts version from "bd version X.Y.Z ..." output.
func parseBeadsVersion(output string) string {
	// Match patterns like "bd version 0.52.0" or "bd version 0.52.0 (dev: ...)"
	re := regexp.MustCompile(`bd version (\d+\.\d+\.\d+)`)
	matches := re.FindStringSubmatch(output)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

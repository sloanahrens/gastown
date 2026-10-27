// Package deps manages external dependencies for Gas Town.
package deps

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// BeadsStatus represents the state of the beads installation.
type BeadsStatus int

const (
	BeadsOK       BeadsStatus = iota // bd found and reported a version
	BeadsNotFound                    // bd not in PATH
	BeadsUnknown                     // bd found but its version could not be read
)

// beadsProbeTimeout bounds the presence probe, so a broken bd install cannot
// hang every gt command. 10s is generous but necessary: under heavy CI load
// (parallel test packages), even a trivial shell script can take >3s to start.
const beadsProbeTimeout = 10 * time.Second

// CheckBeads checks that bd is on PATH and reports a version. It is a cheap
// presence probe, not a compatibility verdict: whether the town may run on
// this bd is CheckBDHandshake's decision (schema level and JSON contract),
// because a semver says nothing about either. It reads no database.
// Returns status and the installed version (if found).
func CheckBeads() (BeadsStatus, string) {
	// A plain wrapper: bd runs in the caller's directory with the BEADS target
	// variables stripped from the environment, so a stale BEADS_DIR or
	// BEADS_DOLT_* in the caller's shell cannot steer the probe, and bd
	// resolves the workspace itself.
	store := beads.NewPlain("", beads.StripBDTargetEnv(os.Environ())).
		WithTimeout(beadsProbeTimeout).WithProcessGroup()
	info, err := store.Version()
	return beadsStatusFromVersion(info, err)
}

// beadsStatusFromVersion classifies the store's answer: a report with a semver
// is bd, a bd that was not there to run is BeadsNotFound, and anything else is
// a bd whose version could not be read.
func beadsStatusFromVersion(info beads.VersionInfo, err error) (BeadsStatus, string) {
	if err != nil {
		if errors.Is(err, beads.ErrNotInstalled) {
			return BeadsNotFound, ""
		}
		return BeadsUnknown, ""
	}
	version := beadsSemverRE.FindString(info.Version)
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

// beadsSemverRE trims a report's version to its semver: a bd may add a build
// suffix, and "not-semver" is no version at all.
var beadsSemverRE = regexp.MustCompile(`^\d+\.\d+\.\d+`)

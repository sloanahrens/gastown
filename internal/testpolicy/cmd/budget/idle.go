package main

import (
	"errors"

	"github.com/steveyegge/gastown/internal/hostcpu"
)

// errNoCPUIdle is what hostCPUIdle reports when the host's CPU idle share
// could not be sampled. EnforceBudget reports an unknown reading rather than
// treating it as a quiet host.
var errNoCPUIdle = errors.New("the host's CPU idle share could not be read")

// hostCPUIdle is the share of the host's CPU that is idle (internal/hostcpu:
// one two-sample top). Unlike the 1-minute load average this runner read
// before (gt-6rieh), it falls as the host fills up, which is what decides
// whether this run's user-CPU measurements are the packages' own work or the
// host's.
func hostCPUIdle() (float64, error) {
	idle, ok := hostcpu.Idle()
	if !ok {
		return 0, errNoCPUIdle
	}
	return idle, nil
}

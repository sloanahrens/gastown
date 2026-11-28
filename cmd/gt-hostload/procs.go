package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

// psTimeout bounds the ps read, which lists every process once (gt-efoycy).
const psTimeout = 10 * time.Second

// scannerSuffix ends the command of the antivirus scanner's daemon, whose CPU
// seconds scannerSeconds adds up (gt-efoycy).
const scannerSuffix = "/BDLDaemon"

// readProcs returns the Mac's process list from `ps -axo pid=,time=,comm=`
// through read.
func readProcs(read func(timeout time.Duration, name string, args ...string) (string, error)) ([]hostload.Proc, error) {
	text, err := read(psTimeout, "ps", "-axo", "pid=,time=,comm=")
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs, err := hostload.ParseProcs(text)
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return procs, nil
}

// scannerSeconds adds the CPU seconds of every process whose command ends
// /BDLDaemon, and reports whether any such process was found.
func scannerSeconds(procs []hostload.Proc) (float64, bool) {
	var sec float64
	found := false
	for _, p := range procs {
		if strings.HasSuffix(p.Command, scannerSuffix) {
			sec += p.CPUTime
			found = true
		}
	}
	return sec, found
}

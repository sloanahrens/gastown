package main

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

const vmProcSuffix = "com.apple.Virtualization.VirtualMachine" // ends a VM process's command (gt-efoycy)

// vmSeconds totals each VM's CPU seconds, keyed by the name lsofVM gives its
// process: two processes of one VM add, an unnamed one is skipped, and a
// failed lsof loses only its process and reports one error (gt-co0vrw).
func vmSeconds(procs []hostload.Proc, read func(timeout time.Duration, name string, args ...string) (string, error)) (map[string]float64, []error) {
	seconds := map[string]float64{}
	var errs []error
	for _, p := range procs {
		if !strings.HasSuffix(p.Command, vmProcSuffix) {
			continue
		}
		name, err := lsofVM(p.PID, read)
		if err != nil {
			errs = append(errs, err)
		} else if name != "" {
			seconds[name] += p.CPUTime
		}
	}
	return seconds, errs
}

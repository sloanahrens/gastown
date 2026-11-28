package main

import (
	"errors"
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

// collectCPU returns the Mac's CPU record: its idle share and both memory
// values from top, then the scanner's and each VM's CPU seconds from ps and
// lsof. A failed read costs only its own values: the error is recorded and what
// it could not fill stays nil or absent, so the line prints '-' rather than a
// made-up 0 (gt-n52367, gt-5bg7m4, gt-6touna).
func collectCPU(read func(timeout time.Duration, name string, args ...string) (string, error)) (hostload.CPURecord, []error) {
	var rec hostload.CPURecord
	var errs []error
	idle, used, compressor, err := readTop(read)
	switch {
	case err != nil:
		errs = append(errs, err)
	case used > math.MaxInt64 || compressor > math.MaxInt64:
		errs = append(errs, errors.New("top: memory amount does not fit int64 bytes"))
	default:
		idlePct, usedBytes, compressorBytes := idle, int64(used), int64(compressor) //nolint:gosec // G115: guarded above by the MaxInt64 check
		rec.IdlePct, rec.UsedBytes, rec.CompressorBytes = &idlePct, &usedBytes, &compressorBytes
	}
	procs, err := readProcs(read)
	if err != nil {
		errs = append(errs, err)
	} else {
		vmSec, vmErrs := vmSeconds(procs, read)
		errs = append(errs, vmErrs...)
		if sec, found := scannerSeconds(procs); found {
			vmSec["scanner"] = sec
		}
		if len(vmSec) > 0 {
			rec.Seconds = vmSec
		}
	}
	return rec, errs
}

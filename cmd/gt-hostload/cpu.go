package main

import (
	"errors"
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

// collectCPU returns the Mac's CPU record from top first: its idle share and
// both memory values. A failed step records its error and leaves the values it
// could not read nil, so the line prints '-' rather than a made-up 0 (gt-n52367).
func collectCPU(read func(timeout time.Duration, name string, args ...string) (string, error)) (hostload.CPURecord, []error) {
	var rec hostload.CPURecord
	idle, used, compressor, err := readTop(read)
	if err != nil {
		return rec, []error{err}
	}
	if used > math.MaxInt64 || compressor > math.MaxInt64 {
		return rec, []error{errors.New("top: memory amount does not fit int64 bytes")}
	}
	idlePct, usedBytes, compressorBytes := idle, int64(used), int64(compressor) //nolint:gosec // G115: guarded above by the MaxInt64 check
	rec.IdlePct, rec.UsedBytes, rec.CompressorBytes = &idlePct, &usedBytes, &compressorBytes
	return rec, nil
}

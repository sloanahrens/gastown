package doltserver

import (
	"math"
	"strconv"
	"strings"
)

// The two vmmap -summary lines the footprint reader takes its numbers from.
// vmmap prints each value with one decimal place and a K, M or G unit.
const (
	footprintKey     = "Physical footprint"
	footprintPeakKey = "Physical footprint (peak)"
)

// serverFootprint reads the server process's physical memory footprint and
// the peak that process has reached, both in bytes, from
// `vmmap -summary <pid>`.
//
// RSS overstates what the process holds — freed C memory the kernel has not
// reclaimed stays in the resident size — so the footprint is the number that
// answers "how much memory is this server using" (gt-v1zyv).
//
// A platform without vmmap, a server with no PID, a vmmap that fails, and
// output that does not carry a line all read as zero for the value that line
// would have carried. Memory reporting is a report: it never fails the status
// command and it raises no warning, so there is no error to return.
func (h *host) serverFootprint(pid int) (footprint, peak int64) {
	if pid <= 0 || h.platformOS() != "darwin" {
		return 0, 0
	}
	out, err := h.probe("vmmap", "-summary", strconv.Itoa(pid))
	if err != nil {
		return 0, 0
	}
	return parseVMMapFootprint(string(out))
}

// parseVMMapFootprint takes the footprint and peak values from vmmap -summary
// output. Each line is read on its own: a line that is absent, or whose value
// cannot be read, leaves its own value zero.
func parseVMMapFootprint(out string) (footprint, peak int64) {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case footprintKey:
			footprint = parseVMMapBytes(strings.TrimSpace(value))
		case footprintPeakKey:
			peak = parseVMMapBytes(strings.TrimSpace(value))
		}
	}
	return footprint, peak
}

// vmmapUnits are the units vmmap -summary prints, in bytes.
var vmmapUnits = map[byte]float64{
	'K': 1 << 10,
	'M': 1 << 20,
	'G': 1 << 30,
}

// parseVMMapBytes converts a vmmap size such as "1.7G", "512.0M" or "34.5K"
// into bytes. Anything without a recognized unit, or with a value that is not
// a non-negative number, reads as zero — which the caller reports as
// unavailable.
// NaN, Inf and a size that int64 cannot hold also read as zero.
func parseVMMapBytes(s string) int64 {
	if s == "" {
		return 0
	}
	unit, ok := vmmapUnits[s[len(s)-1]]
	if !ok {
		return 0
	}
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || n < 0 {
		return 0
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	// int64 cannot hold 2^63 bytes or more, so an overflowing size reads as zero.
	if n*unit >= 1<<63 {
		return 0
	}
	return int64(math.Round(n * unit))
}

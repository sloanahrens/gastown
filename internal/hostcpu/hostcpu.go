// Package hostcpu reads the share of the host's CPU that is idle.
//
// macOS counts runnable threads in its 1-minute load average, not cores: this
// town's 24-core host read load 29.6 with 48.6% of its CPU idle, so a gate
// reading that average stands down work the host has room to run. The idle
// share falls as the host fills up, which is what the gates that decide
// whether the host can take another full suite read (gt-6rieh).
//
// internal/hoststat (gt-auiji) is the fuller host probe, and this package
// converges onto it once it lands.
package hostcpu

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds one Idle call. top takes two samples a second apart,
// so this is several times the probe's own cost.
const probeTimeout = 8 * time.Second

// Idle reports the percent of all cores that was idle over the probe's
// sample window. ok is false when the probe could not read it: a CPU that
// went unread must not read as an idle one, and the caller decides which way
// to fail.
func Idle() (idlePercent float64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := topOutput(ctx)
	if err != nil {
		return 0, false
	}
	return ParseIdle(string(out))
}

// ParseIdle reads the idle share off top's last CPU line: macOS writes "CPU
// usage: 23.14% user, 28.21% sys, 48.64% idle", Linux "%Cpu(s):  6.2 us,
// 12.5 sy, 81.2 id". Only the second of top's two samples is a reading of
// now — the first is cumulative since boot, which would judge this moment by
// the host's whole uptime — so a sample carrying fewer than two CPU lines
// has no reading at all, rather than a reading of the wrong interval.
func ParseIdle(s string) (float64, bool) {
	readings := cpuIdleReadings(s)
	if len(readings) < 2 {
		return 0, false
	}
	return readings[len(readings)-1], true
}

// cpuIdleReadings returns the idle share off every CPU line top printed, in
// the order they appear. A line counts only if it carries an idle field, so
// a process row that happens to hold the same words is skipped rather than
// parsed as a sample.
func cpuIdleReadings(s string) []float64 {
	var out []float64
	for _, line := range strings.Split(s, "\n") {
		if !isCPULine(line) {
			continue
		}
		if v, ok := parseIdleField(line); ok {
			out = append(out, v)
		}
	}
	return out
}

// isCPULine reports whether line is one of top's CPU summaries: macOS opens
// it "CPU usage: ...", Linux "%Cpu(s): ...". Both start the line, so the
// process table below them cannot be mistaken for one.
func isCPULine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(lower, "cpu usage") || strings.HasPrefix(lower, "%cpu")
}

// parseIdleField reads the number before top's "idle"/"id" field.
func parseIdleField(line string) (float64, bool) {
	fields := strings.Fields(line)
	for i, field := range fields {
		if i == 0 {
			continue
		}
		switch strings.Trim(field, ",%") {
		case "idle", "id":
			v, err := strconv.ParseFloat(strings.Trim(fields[i-1], ",%"), 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}
